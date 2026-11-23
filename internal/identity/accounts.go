package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/zybuu-ai/abhed/internal/filelock"
	"github.com/zybuu-ai/abhed/internal/home"
)

// Accounts on this machine are kept in ~/.abhed/accounts/accounts.json,
// 0600 in a 0700 folder, inside the state the agent's tools and sandbox
// cannot reach. They are a separate space for each person who uses this OS
// login, not encryption: whoever can act as the OS user can read the files,
// as they always could. The OS account stays the confidentiality boundary.

// Kinds of entry.
const (
	KindAccount = "account" // a username and password on this machine
	KindServer  = "server"  // a sign-in to an Abhed server, kept as a profile
)

// DefaultBcryptCost is the cost of a password hash here. The file can be read
// by whoever is the OS user, so it is above the library default.
const DefaultBcryptCost = 12

// processCost is the cost stores opened from now on use; 0 means the default.
var processCost atomic.Int32

// SetHashCost sets the bcrypt cost of every store this process opens after
// the call. Tests set it once, before any test runs; a store already open
// keeps the cost it was opened with.
func SetHashCost(cost int) { processCost.Store(int32(cost)) }

func openCost() int {
	if c := int(processCost.Load()); c > 0 {
		return c
	}
	return DefaultBcryptCost
}

// MinPassword is the fewest characters a password may have.
const MinPassword = 10

// After this many wrong passwords in a row, each further try waits.
const freeTries = 3

// maxLock is the longest a run of wrong passwords makes the next try wait.
const maxLock = 15 * time.Minute

var (
	ErrNoAccount     = errors.New("no such account on this machine")
	ErrAccountExists = errors.New("that username is already taken on this machine")
	ErrBadPassword   = errors.New("incorrect username or password")
	ErrWeakPassword  = fmt.Errorf("a password needs at least %d characters", MinPassword)
	ErrCommon        = errors.New("that password is one of the most common; choose another")
	ErrBadUsername   = errors.New("a username is 1 to 32 lowercase letters, digits, dots, dashes or underscores, starting with a letter or digit")
	ErrBadCode       = errors.New("that recovery code is not this account's")
	ErrSamePassword  = errors.New("choose a new password; it cannot be the same as the current one")
)

// LockedError is a sign-in refused because of recent wrong passwords.
type LockedError struct{ Until time.Time }

func (e *LockedError) Error() string {
	return fmt.Sprintf("too many wrong passwords; try again after %s", e.Until.Local().Format("15:04:05"))
}

var validUsername = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// commonPasswords are refused outright; a short list, as the floor.
var commonPasswords = []string{"password12", "password123", "1234567890", "12345678910", "qwertyuiop",
	"iloveyou12", "abhedabhed", "letmein123", "welcome123", "passw0rd12", "administrator", "0123456789",
	"qwerty1234", "1q2w3e4r5t", "password1234", "trustno1234"}

// Reset is a password reset to tell the person about at their next sign-in.
type Reset struct {
	At     time.Time `json:"at"`
	By     string    `json:"by"` // "recovery-code" or "os-user"
	OSUser string    `json:"os_user,omitempty"`
	Host   string    `json:"host,omitempty"`
}

// Entry is one account or server sign-in on this machine.
type Entry struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
	Name     string `json:"name,omitempty"`
	// URL is the server, for a server sign-in.
	URL string `json:"url,omitempty"`
	// Profile names the server sign-in in this entry's own profiles file.
	Profile      string    `json:"profile,omitempty"`
	Hash         string    `json:"hash,omitempty"`
	RecoveryHash string    `json:"recovery_hash,omitempty"`
	Revocations  int64     `json:"revocations,omitempty"`
	Failed       int       `json:"failed,omitempty"`
	LockedUntil  time.Time `json:"locked_until,omitzero"`
	// LegacyUser is the $USER the record used before sign-in, for the account
	// that took over that data: sessions recorded under it are this one's.
	LegacyUser string    `json:"legacy_user,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsed   time.Time `json:"last_used,omitzero"`
	// Reset is a reset not yet shown at a sign-in.
	Reset *Reset `json:"reset,omitempty"`
	// RememberDigest and RememberRev check the remembered sign-in: the token's
	// digest and the revocation count when it was issued.
	RememberDigest string `json:"remember_digest,omitempty"`
	RememberRev    int64  `json:"remember_rev,omitempty"`
}

// Display is who the entry is to a person.
func (e Entry) Display() string {
	switch {
	case e.Kind == KindServer && e.Email != "":
		return e.Email + " on " + hostOf(e.URL)
	case e.Kind == KindServer:
		return e.Username + " on " + hostOf(e.URL)
	case e.Email != "":
		return e.Username + " (" + e.Email + ")"
	}
	return e.Username
}

func hostOf(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

type accountsFile struct {
	Version  int      `json:"version"`
	Accounts []*Entry `json:"accounts"`
}

type remembered struct {
	ID       string    `json:"id"`
	Token    string    `json:"token"`
	IssuedAt time.Time `json:"issued_at"`
}

// Store is the accounts on this machine.
type Store struct {
	dir  string
	now  func() time.Time
	cost int
}

// WithCost returns the store hashing new passwords at cost, for a test that
// owns the store and wants fast hashes without touching the process.
func (s *Store) WithCost(cost int) *Store {
	c := *s
	c.cost = cost
	return &c
}

// Open opens the store under ~/.abhed/accounts, making the folder 0700.
func Open() (*Store, error) {
	dir, err := home.MachinePath(home.AccountsDir)
	if err != nil {
		return nil, err
	}
	return OpenAt(dir)
}

// OpenAt opens the store in dir.
func OpenAt(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the accounts folder: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return nil, fmt.Errorf("%s must be a folder, not a link", dir)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s holds account passwords and is open to other users (mode %o); run chmod 700 on it", dir, fi.Mode().Perm())
	}
	return &Store{dir: dir, now: time.Now, cost: openCost()}, nil
}

// Dir is the accounts folder.
func (s *Store) Dir() string { return s.dir }

func (s *Store) file() string { return filepath.Join(s.dir, "accounts.json") }

func (s *Store) rememberedFile() string { return filepath.Join(s.dir, "remembered.json") }

func private(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file, not a link", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is open to other users (mode %o); run chmod 600 on it", path, fi.Mode().Perm())
	}
	return nil
}

func readJSON(path string, v any) (bool, error) {
	if err := private(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the accounts folder, checked private above
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name, done := tmp.Name(), false
	defer func() {
		if !done {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	done = true
	return nil
}

func (s *Store) load() (*accountsFile, error) {
	f := &accountsFile{Version: 1}
	if _, err := readJSON(s.file(), f); err != nil {
		return nil, err
	}
	return f, nil
}

// change reads, changes and writes the accounts under a lock other
// processes take too.
func (s *Store) change(do func(f *accountsFile) error) error {
	unlock, err := filelock.Lock(s.file()+".lock", 5*time.Second, "%s is held by another abhed (waited %s); nothing was changed")
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	if err := do(f); err != nil {
		return err
	}
	return writeJSON(s.file(), f)
}

func find(f *accountsFile, kind, username string) *Entry {
	for _, e := range f.Accounts {
		if e.Kind == kind && strings.EqualFold(e.Username, username) {
			return e
		}
	}
	return nil
}

func findID(f *accountsFile, id string) *Entry {
	for _, e := range f.Accounts {
		if e.ID == id {
			return e
		}
	}
	return nil
}

// List is every entry, oldest first.
func (s *Store) List() ([]Entry, error) {
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(f.Accounts))
	for _, e := range f.Accounts {
		out = append(out, *e)
	}
	return out, nil
}

// Get is the account on this machine named username.
func (s *Store) Get(username string) (Entry, error) {
	f, err := s.load()
	if err != nil {
		return Entry{}, err
	}
	if e := find(f, KindAccount, username); e != nil {
		return *e, nil
	}
	return Entry{}, ErrNoAccount
}

// ByID is the entry with id.
func (s *Store) ByID(id string) (Entry, error) {
	f, err := s.load()
	if err != nil {
		return Entry{}, err
	}
	if e := findID(f, id); e != nil {
		return *e, nil
	}
	return Entry{}, ErrNoAccount
}

// CheckPassword reports why password cannot be used.
func CheckPassword(password string) error {
	if len([]rune(password)) < MinPassword {
		return ErrWeakPassword
	}
	if slices.Contains(commonPasswords, strings.ToLower(password)) {
		return ErrCommon
	}
	return nil
}

// CheckUsername reports why username cannot name an account here.
func CheckUsername(username string) error {
	if !validUsername.MatchString(username) {
		return ErrBadUsername
	}
	return nil
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newRecoveryCode is 16 random bytes in base32, in groups of four.
func newRecoveryCode() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	var parts []string
	for len(s) > 4 {
		parts = append(parts, s[:4])
		s = s[4:]
	}
	return strings.Join(append(parts, s), "-")
}

func normalCode(c string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(c)))
}

func (s *Store) hash(secret string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(secret), s.cost)
	return string(h), err
}

// Create adds an account and makes its folder. It returns the recovery code,
// shown once.
func (s *Store) Create(username, password, email, name string) (Entry, string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if err := CheckUsername(username); err != nil {
		return Entry{}, "", err
	}
	if err := CheckPassword(password); err != nil {
		return Entry{}, "", err
	}
	pw, err := s.hash(password)
	if err != nil {
		return Entry{}, "", err
	}
	code := newRecoveryCode()
	rh, err := s.hash(normalCode(code))
	if err != nil {
		return Entry{}, "", err
	}
	e := &Entry{ID: newID(), Kind: KindAccount, Username: username, Email: strings.TrimSpace(email),
		Name: strings.TrimSpace(name), Hash: pw, RecoveryHash: rh, CreatedAt: s.now().UTC()}
	if err := s.change(func(f *accountsFile) error {
		if find(f, KindAccount, username) != nil {
			return ErrAccountExists
		}
		f.Accounts = append(f.Accounts, e)
		return nil
	}); err != nil {
		return Entry{}, "", err
	}
	if _, err := s.Root(*e); err != nil {
		return Entry{}, "", err
	}
	return *e, code, nil
}

// AddServer records a sign-in to a server as an entry of its own, or finds
// the one there is, and makes its folder.
func (s *Store) AddServer(url, username, email, profile string) (Entry, error) {
	var out Entry
	err := s.change(func(f *accountsFile) error {
		for _, e := range f.Accounts {
			if e.Kind == KindServer && e.URL == url && strings.EqualFold(e.Username, username) {
				e.Email, e.Profile = email, profile
				out = *e
				return nil
			}
		}
		e := &Entry{ID: newID(), Kind: KindServer, Username: username, Email: email, URL: url,
			Profile: profile, CreatedAt: s.now().UTC()}
		f.Accounts = append(f.Accounts, e)
		out = *e
		return nil
	})
	if err != nil {
		return Entry{}, err
	}
	_, err = s.Root(out)
	return out, err
}

// Root is the entry's own folder, made 0700 when missing.
func (s *Store) Root(e Entry) (string, error) {
	if !validID(e.ID) {
		return "", fmt.Errorf("account id %q is not one this store makes", e.ID)
	}
	dir := filepath.Join(s.dir, e.ID)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return "", fmt.Errorf("%s must be a folder, not a link", dir)
	}
	return dir, nil
}

var idRe = regexp.MustCompile(`^[0-9a-f]{24}$`)

func validID(id string) bool { return idRe.MatchString(id) }

// dummies holds, per cost, a hash no password matches.
var dummies sync.Map

// dummyHash is a hash at the store's cost that no password matches, so an
// unknown username takes as long as a wrong password.
func (s *Store) dummyHash() []byte {
	if h, ok := dummies.Load(s.cost); ok {
		return h.([]byte)
	}
	h, _ := bcrypt.GenerateFromPassword([]byte(newID()), s.cost)
	got, _ := dummies.LoadOrStore(s.cost, h)
	return got.([]byte)
}

func lockFor(failed int) time.Duration {
	if failed < freeTries {
		return 0
	}
	d := 30 * time.Second << min(failed-freeTries, 10)
	return min(d, maxLock)
}

// Authenticate checks a password. Wrong ones in a row make the next try
// wait; a right one clears the count.
func (s *Store) Authenticate(username, password string) (Entry, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	var out Entry
	var result error
	err := s.change(func(f *accountsFile) error {
		e := find(f, KindAccount, username)
		if e == nil {
			// The same work as a real account, so timing does not tell.
			_ = bcrypt.CompareHashAndPassword(s.dummyHash(), []byte(password))
			result = ErrBadPassword
			return nil
		}
		if now := s.now(); now.Before(e.LockedUntil) {
			result = &LockedError{Until: e.LockedUntil}
			return nil
		}
		if bcrypt.CompareHashAndPassword([]byte(e.Hash), []byte(password)) != nil {
			e.Failed++
			if d := lockFor(e.Failed); d > 0 {
				e.LockedUntil = s.now().Add(d).UTC()
			}
			result = ErrBadPassword
			return nil
		}
		e.Failed, e.LockedUntil, e.LastUsed = 0, time.Time{}, s.now().UTC()
		out = *e
		return nil
	})
	if err != nil {
		return Entry{}, err
	}
	return out, result
}

// TakeReset returns the reset not yet shown for id, and clears it.
func (s *Store) TakeReset(id string) (*Reset, error) {
	var r *Reset
	err := s.change(func(f *accountsFile) error {
		if e := findID(f, id); e != nil {
			r, e.Reset = e.Reset, nil
		}
		return nil
	})
	return r, err
}

// ChangePassword sets a new password after checking the current one. Every
// remembered sign-in of the account ends.
func (s *Store) ChangePassword(username, current, next string) error {
	if _, err := s.Authenticate(username, current); err != nil {
		return err
	}
	if current == next {
		return ErrSamePassword
	}
	return s.setPassword(username, next, nil, nil)
}

// ResetWithCode sets a new password with the recovery code, and makes a new
// code, returned.
func (s *Store) ResetWithCode(username, code, next string) (string, error) {
	e, err := s.Get(username)
	if err != nil {
		return "", err
	}
	if e.RecoveryHash == "" || bcrypt.CompareHashAndPassword([]byte(e.RecoveryHash), []byte(normalCode(code))) != nil {
		return "", ErrBadCode
	}
	fresh := newRecoveryCode()
	return fresh, s.setPassword(username, next, &Reset{At: s.now().UTC(), By: "recovery-code"}, &fresh)
}

// ResetAsOSUser sets a new password with no code: whoever is the OS user can
// already read the account's files. The next sign-in says it happened.
func (s *Store) ResetAsOSUser(username, next, osUser, host string) error {
	if _, err := s.Get(username); err != nil {
		return err
	}
	return s.setPassword(username, next, &Reset{At: s.now().UTC(), By: "os-user", OSUser: osUser, Host: host}, nil)
}

func (s *Store) setPassword(username, next string, reset *Reset, code *string) error {
	if err := CheckPassword(next); err != nil {
		return err
	}
	pw, err := s.hash(next)
	if err != nil {
		return err
	}
	var rh string
	if code != nil {
		if rh, err = s.hash(normalCode(*code)); err != nil {
			return err
		}
	}
	return s.change(func(f *accountsFile) error {
		e := find(f, KindAccount, username)
		if e == nil {
			return ErrNoAccount
		}
		e.Hash, e.Revocations = pw, e.Revocations+1
		e.Failed, e.LockedUntil = 0, time.Time{}
		if reset != nil {
			e.Reset = reset
		}
		if rh != "" {
			e.RecoveryHash = rh
		}
		return nil
	})
}

// Remove deletes the entry. Its folder is left for the caller to keep or
// delete.
func (s *Store) Remove(id string) error {
	return s.change(func(f *accountsFile) error {
		i := slices.IndexFunc(f.Accounts, func(e *Entry) bool { return e.ID == id })
		if i < 0 {
			return ErrNoAccount
		}
		f.Accounts = slices.Delete(f.Accounts, i, i+1)
		return nil
	})
}

// SetLegacyUser marks the entry as the one that took over the data recorded
// before sign-in as user.
func (s *Store) SetLegacyUser(id, user string) error {
	return s.change(func(f *accountsFile) error {
		for _, e := range f.Accounts {
			if e.LegacyUser != "" && e.ID != id {
				return fmt.Errorf("the data from before sign-in already belongs to %s", e.Username)
			}
		}
		e := findID(f, id)
		if e == nil {
			return ErrNoAccount
		}
		e.LegacyUser = user
		return nil
	})
}

// LegacyOwner is the entry that took over the data from before sign-in.
func (s *Store) LegacyOwner() (Entry, bool, error) {
	f, err := s.load()
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range f.Accounts {
		if e.LegacyUser != "" {
			return *e, true, nil
		}
	}
	return Entry{}, false, nil
}

func digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Remember makes id the sign-in this machine starts with, until Forget or
// until the entry's revocation count moves.
func (s *Store) Remember(id string) error {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	if err := s.change(func(f *accountsFile) error {
		e := findID(f, id)
		if e == nil {
			return ErrNoAccount
		}
		e.RememberDigest, e.RememberRev = digest(token), e.Revocations
		e.LastUsed = s.now().UTC()
		return nil
	}); err != nil {
		return err
	}
	return writeJSON(s.rememberedFile(), remembered{ID: id, Token: token, IssuedAt: s.now().UTC()})
}

// Remembered is the sign-in this machine starts with, if one is held and
// still good: issued for the entry as it stands, and younger than maxAge
// when maxAge is set (nil: until sign-out).
func (s *Store) Remembered(maxDays *int) (Entry, bool, error) {
	var r remembered
	ok, err := readJSON(s.rememberedFile(), &r)
	if err != nil || !ok {
		return Entry{}, false, err
	}
	if maxDays != nil && !s.now().Before(r.IssuedAt.Add(time.Duration(*maxDays)*24*time.Hour)) {
		return Entry{}, false, nil
	}
	e, err := s.ByID(r.ID)
	if errors.Is(err, ErrNoAccount) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	if e.RememberDigest == "" || e.RememberRev != e.Revocations ||
		subtle.ConstantTimeCompare([]byte(e.RememberDigest), []byte(digest(r.Token))) != 1 {
		return Entry{}, false, nil
	}
	return e, true, nil
}

// Forget ends the remembered sign-in: the next start asks again.
func (s *Store) Forget() error {
	var r remembered
	if ok, _ := readJSON(s.rememberedFile(), &r); ok {
		_ = s.change(func(f *accountsFile) error {
			if e := findID(f, r.ID); e != nil {
				e.RememberDigest = ""
			}
			return nil
		})
	}
	if err := os.Remove(s.rememberedFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ForgetIf ends the remembered sign-in only when it is id's: a process
// signs out who it works as, never someone another process signed in since.
// It reports whether it did.
func (s *Store) ForgetIf(id string) (bool, error) {
	var r remembered
	ok, err := readJSON(s.rememberedFile(), &r)
	if err != nil || !ok || r.ID != id {
		return false, err
	}
	return true, s.Forget()
}

// SignOutEverywhere ends every remembered sign-in of the account.
func (s *Store) SignOutEverywhere(id string) error {
	return s.change(func(f *accountsFile) error {
		e := findID(f, id)
		if e == nil {
			return ErrNoAccount
		}
		e.Revocations++
		return nil
	})
}
