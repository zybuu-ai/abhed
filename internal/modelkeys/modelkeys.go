// Package modelkeys keeps the API keys of hosted model providers out of the
// environment and out of configuration: `abhed provider login` stores a key
// in the macOS Keychain where there is one, else in Abhed's own secret store
// (mode 600) under a name Abhed keeps for itself, never offered to a command.
// A provider's configuration names the key; the value is looked up when the
// provider is resolved, and every redactor in the process redacts it from then on.
package modelkeys

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/home"
	"github.com/zybuu-ai/abhed/internal/secrets"
)

// Where a key came from. None of them is the key.
const (
	// KindConfig is a key written in the configuration, or ABHED_API_KEY.
	KindConfig = "config"
	// KindEnv is the environment variable the provider names.
	KindEnv = "env"
	// KindKeychain is the macOS Keychain.
	KindKeychain = "keychain"
	// KindFile is Abhed's own secret store.
	KindFile = "file"
)

// Hosted is a provider type whose official endpoint takes a key: the name
// the key goes by (its customary environment variable) and the endpoint's host.
type Hosted struct {
	Type string `json:"type"`
	Key  string `json:"key"`
	Host string `json:"host"`
}

// hosted are the types a key is looked up for when the configuration names
// none, and only at their official endpoint: a key meant for one host is never
// sent to another.
var hosted = []Hosted{
	{"anthropic", "ANTHROPIC_API_KEY", "api.anthropic.com"},
	{"openai", "OPENAI_API_KEY", "api.openai.com"},
	{"gemini", "GEMINI_API_KEY", "generativelanguage.googleapis.com"},
	{"groq", "GROQ_API_KEY", "api.groq.com"},
	{"together", "TOGETHER_API_KEY", "api.together.xyz"},
	{"openrouter", "OPENROUTER_API_KEY", "openrouter.ai"},
	{"mistral", "MISTRAL_API_KEY", "api.mistral.ai"},
	{"deepseek", "DEEPSEEK_API_KEY", "api.deepseek.com"},
	{"xai", "XAI_API_KEY", "api.x.ai"},
}

// HostedTypes are the provider types `abhed provider login` knows by type.
func HostedTypes() []Hosted { return slices.Clone(hosted) }

// HostedType is the entry for typ.
func HostedType(typ string) (Hosted, bool) {
	i := slices.IndexFunc(hosted, func(h Hosted) bool { return h.Type == typ })
	if i < 0 {
		return Hosted{}, false
	}
	return hosted[i], true
}

// DefaultName is the key name a provider of type typ at baseURL uses when its
// configuration names none: the type's own name, only at its official
// endpoint (an empty baseURL is that endpoint). "" otherwise.
func DefaultName(typ, baseURL string) string {
	h, ok := HostedType(typ)
	if !ok {
		return ""
	}
	if strings.TrimSpace(baseURL) == "" {
		return h.Key
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), h.Host) || u.Port() != "" {
		return ""
	}
	return h.Key
}

// HostedByKey is the hosted type whose own key name is name.
func HostedByKey(name string) (Hosted, bool) {
	i := slices.IndexFunc(hosted, func(h Hosted) bool { return h.Key == name })
	if i < 0 {
		return Hosted{}, false
	}
	return hosted[i], true
}

// EndpointHost is the host a provider of type typ at baseURL sends to: the
// type's official host when baseURL is empty, else baseURL's host when it is
// https with no port; "" when it is neither.
func EndpointHost(typ, baseURL string) string {
	if strings.TrimSpace(baseURL) == "" {
		if h, ok := HostedType(typ); ok {
			return strings.ToLower(h.Host)
		}
		return ""
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.Port() != "" || u.Hostname() == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// HostedKeyAllowed reports whether the stored key name may go to a provider
// of type typ at baseURL, as far as the hosted types decide: a hosted type's
// own key (ANTHROPIC_API_KEY, ...) only to that type's official host, whoever
// names it. hosted is false for any other name, which the caller judges.
func HostedKeyAllowed(name, typ, baseURL string) (ok, hosted bool) {
	h, isHosted := HostedByKey(name)
	if !isHosted {
		return true, false
	}
	return EndpointHost(typ, baseURL) == strings.ToLower(h.Host), true
}

// HostedTypeOfKey is the type whose own key name is name, or name.
func HostedTypeOfKey(name string) string {
	if h, ok := HostedByKey(name); ok {
		return h.Type
	}
	return name
}

// ValidName reports whether name can be a stored key's name.
func ValidName(name string) bool {
	return secrets.ValidName(secrets.ModelKeyPrefix+name) && !strings.HasPrefix(name, secrets.ModelKeyPrefix)
}

// Keychain keeps generic passwords for Abhed. The macOS login Keychain is the
// real one; tests give their own, so none of them reaches it.
//
// service is the person's: Service before sign-in, ServiceFor(id) for an
// account, so one person's lookups never see another's keys.
type Keychain interface {
	// Get is the value stored for name, and whether there is one.
	Get(service, name string) (string, bool, error)
	// Set stores value for name, replacing any.
	Set(service, name, value string) error
	// Delete removes name, reporting whether it was there.
	Delete(service, name string) (bool, error)
}

// Service is the Keychain service model keys were kept under before sign-in,
// and still are when no one has signed in; the account is the key's name.
const Service = "Abhed model key"

// ServiceFor is the Keychain service of the Abhed account id.
func ServiceFor(id string) string { return Service + ": " + id }

// A person is who keys are kept for: kind is legacy (no sign-in), account
// or guest; id is the account's.
type person struct{ kind, id string }

// currentPerson is who is working in this process, from its person folder.
var currentPerson = func() person {
	if home.Legacy() {
		return person{kind: "legacy"}
	}
	p, err := home.Person()
	m, merr := home.Machine()
	if err != nil || merr != nil {
		return person{kind: "guest"}
	}
	rel, err := filepath.Rel(m, p)
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if err != nil || len(parts) != 2 || parts[0] != home.AccountsDir {
		// A guest, or a folder that is no account's: no stored key is anyone's here.
		return person{kind: "guest"}
	}
	return person{kind: "account", id: parts[1]}
}

// service is the Keychain service for who, "" for a guest, who keeps none.
func (who person) service() string {
	switch who.kind {
	case "legacy":
		return Service
	case "account":
		return ServiceFor(who.id)
	}
	return ""
}

// ErrGuest refuses a stored key to a guest: a guest keeps nothing.
var ErrGuest = errors.New("a guest keeps no stored key; sign in to keep one (abhed account signin), or set the key in the environment")

var (
	mu sync.Mutex
	// chain is the Keychain in use; nil where there is none.
	chain     Keychain
	chainSet  bool
	fileStore = secrets.Default
	// cache holds lookups briefly, as a provider is resolved several times
	// while a session starts and the Keychain is a process each time.
	cache = map[string]cached{}
)

type cached struct {
	value, kind string
	at          time.Time
}

const cacheFor = 15 * time.Second

// Use has this process keep keys in k (nil for no Keychain) and in the
// store at file ("" keeps Abhed's own store), and returns what undoes it. For
// tests, and for a program that keeps keys elsewhere.
func Use(k Keychain, file string) (restore func()) {
	mu.Lock()
	defer mu.Unlock()
	oldChain, oldSet, oldFile := chain, chainSet, fileStore
	chain, chainSet = k, true
	if file != "" {
		fileStore = func() *secrets.Store { return secrets.Open(file) }
	}
	cache = map[string]cached{}
	return func() {
		mu.Lock()
		defer mu.Unlock()
		chain, chainSet, fileStore = oldChain, oldSet, oldFile
		cache = map[string]cached{}
	}
}

// keychain is the Keychain to use now, under mu.
func keychain() Keychain {
	if chainSet {
		return chain
	}
	if why := NoKeychain(); why != "" {
		return nil
	}
	return systemKeychain()
}

// NoKeychain says why the system Keychain is not used in this process, or "".
// Never in a test binary, whatever else holds; otherwise as keychainOff says.
func NoKeychain() string {
	if testing.Testing() {
		return "this is a test"
	}
	if why := keychainOff(os.Getenv, tempHome()); why != "" {
		return why
	}
	if systemKeychain() == nil {
		return "this system has no Keychain Abhed uses"
	}
	return ""
}

// keychainOff says why the environment rules the Keychain out: under a test,
// inside an agent's command, with ABHED_KEYCHAIN=off, or under a temporary
// home (a test's or a harness's).
func keychainOff(getenv func(string) string, tempHome bool) string {
	switch {
	case getenv("ABHED_TESTENV") != "":
		return "this runs under a test"
	case getenv("ABHED_SANDBOX") != "":
		return "this runs inside an agent's command"
	case strings.EqualFold(getenv("ABHED_KEYCHAIN"), "off"):
		return "ABHED_KEYCHAIN is off"
	case tempHome:
		return "the home folder is a temporary one"
	}
	return ""
}

// tempHome reports a home folder inside a temporary folder.
func tempHome() bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return true
	}
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	for _, root := range []string{os.TempDir(), "/tmp", "/var/tmp", "/private/tmp", "/private/var/tmp", "/var/folders", "/private/var/folders"} {
		if r, err := filepath.EvalSymlinks(root); err == nil {
			root = r
		}
		if rel, err := filepath.Rel(root, home); err == nil && rel != "." && filepath.IsLocal(rel) {
			return true
		}
	}
	return false
}

// guard, when set, refuses Store and Remove: whoever works keeps nothing,
// as a guest does. Set once by the program.
var guard atomic.Pointer[func() error]

// SetGuard has Store and Remove ask g first, and refuse with its error.
func SetGuard(g func() error) { guard.Store(&g) }

func guarded() error {
	if g := guard.Load(); g != nil {
		return (*g)()
	}
	return nil
}

// Where says where Store would keep a key now: KindKeychain or KindFile.
func Where() string {
	mu.Lock()
	defer mu.Unlock()
	if keychain() != nil {
		return KindKeychain
	}
	return KindFile
}

// Label is a plain name for a kind, for messages.
func Label(kind string) string {
	switch kind {
	case KindKeychain:
		return "the macOS Keychain"
	case KindFile:
		path := "Abhed's secret store"
		if s := currentFile(); s != "" {
			path += " (" + s + ")"
		}
		return path
	case KindEnv:
		return "the environment"
	case KindConfig:
		return "the configuration"
	}
	return "nowhere"
}

func currentFile() string {
	mu.Lock()
	f := fileStore
	mu.Unlock()
	return f().Path()
}

// Lookup is the key stored under name and where it was found, Keychain
// first; "" with no error when none is stored. The value is redacted by
// every redactor in this process from now on.
func Lookup(name string) (value, kind string, err error) {
	if !ValidName(name) {
		return "", "", fmt.Errorf("%q is not a key name (upper-case letters, digits and _)", name)
	}
	who := currentPerson()
	if who.kind == "guest" {
		return "", "", nil
	}
	ck := who.service() + "\x00" + name
	mu.Lock()
	if c, ok := cache[ck]; ok && time.Since(c.at) < cacheFor {
		mu.Unlock()
		return c.value, c.kind, nil
	}
	k, store := keychain(), fileStore()
	mu.Unlock()
	if k != nil {
		v, ok, err := k.Get(who.service(), name)
		if err != nil {
			return "", "", fmt.Errorf("the Keychain could not be read for %s: %w", name, err)
		}
		if ok && v != "" {
			return remember(ck, name, v, KindKeychain), KindKeychain, nil
		}
	}
	// The secret store is the person's own file (home.PersonPath).
	v, ok, err := store.HeldValue(secrets.ModelKeyPrefix + name)
	if err != nil {
		return "", "", err
	}
	if ok && v != "" {
		return remember(ck, name, v, KindFile), KindFile, nil
	}
	// A miss is not kept: a key stored a moment later is found at once.
	return "", "", nil
}

func remember(ck, name, value, kind string) string {
	if value != "" {
		secrets.AlsoRedact(name, value)
	}
	mu.Lock()
	cache[ck] = cached{value: value, kind: kind, at: time.Now()}
	mu.Unlock()
	return value
}

// ErrShort is a value too short to be a key, which redaction would match in
// ordinary text.
var ErrShort = fmt.Errorf("a key is at least %d characters", secrets.MinLength)

// Store keeps value under name: in the Keychain when there is one, else in
// Abhed's secret store. It returns where; a copy in the other place is removed,
// so a lookup never finds an old key first.
func Store(name, value string) (string, error) {
	if err := guarded(); err != nil {
		return "", err
	}
	if !ValidName(name) {
		return "", fmt.Errorf("%q is not a key name (upper-case letters, digits and _)", name)
	}
	value = strings.TrimSpace(value)
	if len(value) < secrets.MinLength {
		return "", ErrShort
	}
	if strings.ContainsFunc(value, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
		return "", errors.New("a key is printable characters with no spaces")
	}
	who := currentPerson()
	if who.kind == "guest" {
		return "", ErrGuest
	}
	mu.Lock()
	k, store := keychain(), fileStore()
	delete(cache, who.service()+"\x00"+name)
	mu.Unlock()
	secrets.AlsoRedact(name, value)
	if k != nil {
		if err := k.Set(who.service(), name, value); err != nil {
			return "", fmt.Errorf("the key could not be stored in the Keychain: %w", err)
		}
		if err := store.UpdateHeld(func(func(string) (string, bool)) (map[string]string, []string, error) {
			return nil, []string{secrets.ModelKeyPrefix + name}, nil
		}); err != nil {
			return KindKeychain, fmt.Errorf("stored in the Keychain, but an older copy in %s could not be removed: %w", store.Path(), err)
		}
		return KindKeychain, nil
	}
	if err := store.SetHeld(secrets.ModelKeyPrefix+name, value); err != nil {
		return "", err
	}
	return KindFile, nil
}

// Remove deletes the key stored under name from the Keychain and the store,
// and says where it was.
func Remove(name string) ([]string, error) {
	if err := guarded(); err != nil {
		return nil, err
	}
	if !ValidName(name) {
		return nil, fmt.Errorf("%q is not a key name (upper-case letters, digits and _)", name)
	}
	who := currentPerson()
	if who.kind == "guest" {
		return nil, ErrGuest
	}
	mu.Lock()
	k, store := keychain(), fileStore()
	delete(cache, who.service()+"\x00"+name)
	mu.Unlock()
	var from []string
	if k != nil {
		ok, err := k.Delete(who.service(), name)
		if err != nil {
			return nil, fmt.Errorf("the Keychain item could not be removed: %w", err)
		}
		if ok {
			from = append(from, KindKeychain)
		}
	}
	held := secrets.ModelKeyPrefix + name
	if _, ok, err := store.HeldValue(held); err != nil {
		return from, err
	} else if ok {
		if err := store.UpdateHeld(func(func(string) (string, bool)) (map[string]string, []string, error) {
			return nil, []string{held}, nil
		}); err != nil {
			return from, err
		}
		from = append(from, KindFile)
	}
	return from, nil
}

// AdoptLegacy moves the Keychain keys named names from before sign-in to the
// account id, as the first account takes over the rest of that data (the
// secret store moves with the account's folder). A key the account already
// has is left as it is. It returns the names moved.
func AdoptLegacy(id string, names []string) ([]string, error) {
	if !validID(id) {
		return nil, fmt.Errorf("%q is not an account id", id)
	}
	return moveKeys(Service, ServiceFor(id), names)
}

// ReturnLegacy moves account id's Keychain keys named names back to the
// service from before sign-in, as `abhed account unmigrate` moves the rest of
// that data back. A key already there is left as it is.
func ReturnLegacy(id string, names []string) ([]string, error) {
	if !validID(id) {
		return nil, fmt.Errorf("%q is not an account id", id)
	}
	return moveKeys(ServiceFor(id), Service, names)
}

// moveKeys moves the Keychain items named names from one service to another,
// keeping one already at the destination, and returns the names moved.
func moveKeys(from, to string, names []string) ([]string, error) {
	mu.Lock()
	k := keychain()
	cache = map[string]cached{}
	mu.Unlock()
	if k == nil {
		return nil, nil
	}
	var moved, kept []string
	for _, name := range names {
		if !ValidName(name) || slices.Contains(moved, name) || slices.Contains(kept, name) {
			continue
		}
		v, ok, err := k.Get(from, name)
		if err != nil {
			return moved, err
		}
		if !ok || v == "" {
			continue
		}
		if cur, have, err := k.Get(to, name); err != nil {
			return moved, err
		} else if have && cur != "" && cur != v {
			// Both kept: which one is current is the person's to say.
			kept = append(kept, name)
			continue
		} else if !have || cur == "" {
			if err := k.Set(to, name, v); err != nil {
				return moved, err
			}
		}
		if _, err := k.Delete(from, name); err != nil {
			return moved, err
		}
		moved = append(moved, name)
	}
	if len(kept) > 0 {
		return moved, &KeptBothError{Names: kept}
	}
	return moved, nil
}

// KeptBothError names keys the destination already holds a different value
// of: neither copy was removed or replaced.
type KeptBothError struct{ Names []string }

func (e *KeptBothError) Error() string {
	return strings.Join(e.Names, ", ") + " stored both before sign-in and in the account, with different values; both were kept. " +
		"Run abhed provider login to store the current one"
}

var accountID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func validID(id string) bool { return accountID.MatchString(id) }

// HostedKeyNames are the key names of every hosted type.
func HostedKeyNames() []string {
	out := make([]string, 0, len(hosted))
	for _, h := range hosted {
		out = append(out, h.Key)
	}
	return out
}
