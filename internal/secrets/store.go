// Package secrets holds the credentials a session may hand to a command, and
// strips their values from anything that would enter the record.
//
// Two rules shape it. The model never sees a value, only a name: a secret
// enters the sandbox as an environment variable for the one command that asked
// for it, and only when policy allows that name. And the record is append-only,
// so a value that reached it could never be removed; redaction runs before the
// write, on the exact stored values, not on a guess at what a key looks like.
package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// EnvFile names the environment variable that overrides the store's location.
const EnvFile = "ABHED_SECRETS_FILE"

var validName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// ValidName reports whether name is one a secret can be stored under.
func ValidName(name string) bool { return validName.MatchString(name) }

// Store is a file of named values, readable by its owner only.
type Store struct {
	path string
	mu   sync.Mutex
}

// DefaultPath is $ABHED_SECRETS_FILE, else ~/.abhed/secrets.json: outside any
// workspace, and under the directory the agent's tools and sandbox cannot reach.
func DefaultPath() (string, error) {
	if p := os.Getenv(EnvFile); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".abhed", "secrets.json"), nil
}

func Open(path string) *Store { return &Store{path: path} }

// Default opens the store at DefaultPath, the one the CLI uses. A path that
// cannot be worked out names a file that never exists, so the store is empty.
func Default() *Store {
	path, err := DefaultPath()
	if err != nil {
		path = ".abhed-secrets-unavailable"
	}
	return Open(path)
}

// Path reports where the store lives.
func (s *Store) Path() string { return s.path }

// MaxFileSize bounds the store: far beyond any set of credentials.
const MaxFileSize = 1 << 20

func (s *Store) load() (map[string]string, error) {
	// Judged only on the open file, so what is checked is what is read.
	f, err := openStore(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	switch {
	case err != nil:
		return nil, err
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("%s is not a regular file (%s)", s.path, info.Mode().Type())
	case info.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("%s is readable by others (mode %o); run chmod 600 on it", s.path, info.Mode().Perm())
	case info.Size() == 0:
		return nil, fmt.Errorf("%s is empty (0 bytes); an empty store is {} or no file at all", s.path)
	case info.Size() > MaxFileSize:
		return nil, fmt.Errorf("%s is %d bytes, over the %d a store may hold", s.path, info.Size(), MaxFileSize)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("%s grew past %d bytes while it was read", s.path, MaxFileSize)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}

// fixHint says how to repair a store that cannot be loaded.
const fixHint = "Fix the file (a JSON object of NAME: value, chmod 600) or remove it and add the secrets again with `abhed secret set`"

// loadFixable is load with the repair named when the store cannot be loaded.
func (s *Store) loadFixable() (map[string]string, error) {
	m, err := s.load()
	if err != nil {
		return nil, fmt.Errorf("the secrets store cannot be loaded: %w. %s", err, fixHint)
	}
	return m, nil
}

func (s *Store) save(m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Set stores a value under name.
func (s *Store) Set(name, value string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("secret names are upper-case identifiers such as GITHUB_TOKEN, not %q", name)
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("the value is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadFixable()
	if err != nil {
		return err
	}
	m[name] = value
	return s.save(m)
}

// Remove forgets a secret. Removing a name that is not there is not an error.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadFixable()
	if err != nil {
		return err
	}
	delete(m, name)
	return s.save(m)
}

// Names lists what is stored, without values.
func (s *Store) Names() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadFixable()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// Env returns NAME=value pairs for the named secrets, for one command's
// environment. An unknown name is an error, so a typo never becomes an empty
// variable the command misreads as "not configured".
func (s *Store) Env(names []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadFixable()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		v, found := m[n]
		if !found {
			return nil, fmt.Errorf("no secret named %s is stored; the operator adds one with `abhed secret set %s`", n, n)
		}
		out = append(out, n+"="+v)
	}
	return out, nil
}

// Value returns one stored secret, for a tool that uses it itself rather than
// handing it to a command. An unknown name is an error, as in Env.
func (s *Store) Value(name string) (string, error) {
	env, err := s.Env([]string{name})
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(env[0], name+"="), nil
}

// Redactor replaces every stored value in a JSON payload with [secret:NAME].
// Each string literal is decoded and matched as text, so a match never
// straddles an escape and the output is always valid JSON. The longest value
// is replaced first, so a value that contains another is replaced whole.
type Redactor struct {
	pairs []pair
	// broken marks a store that could not be loaded: everything is withheld.
	broken bool
}

type pair struct {
	needle, label string
	// short marks a value under MinLength, which leaves JSON keys alone.
	short bool
}

// MinLength is the fewest characters `abhed secret set` accepts. A shorter value
// stored before is still redacted, but not in JSON keys, whose structure it could break.
const MinLength = 8

// Redactor returns a redactor for the values stored now. A store that exists
// but cannot be loaded gives one that withholds every payload; see LoadRedactor.
func (s *Store) Redactor() *Redactor {
	r, err := s.LoadRedactor()
	if err != nil {
		return &Redactor{broken: true}
	}
	return r
}

// Live redacts with the values stored when it was made, and Load reads the store
// again, for a process that starts many sessions, such as a server.
type Live struct {
	*Redactor
	store *Store
}

// Live returns a Live redactor over the store.
func (s *Store) Live() *Live { return &Live{Redactor: s.Redactor(), store: s} }

// Load reads the store again, as LoadRedactor does.
func (l *Live) Load() (*Redactor, error) { return l.store.LoadRedactor() }

// Fresh redacts with the values stored at each call, reading the store again
// whenever the file has changed, for a session that runs while secrets are
// added: a value bash can be given must be redacted from that moment on. A
// store that stops loading withholds every payload until it loads again.
type Fresh struct {
	store *Store
	mu    sync.Mutex
	stamp freshStamp
	red   *Redactor
}

type freshStamp struct {
	ok      bool
	missing bool
	mod     int64
	size    int64
}

// Fresh returns a redactor over the store that starts from first, the
// reading a session was admitted with.
func (s *Store) Fresh(first *Redactor) *Fresh {
	f := &Fresh{store: s, red: first}
	f.stamp = f.stat()
	return f
}

func (f *Fresh) stat() freshStamp {
	fi, err := os.Stat(f.store.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return freshStamp{ok: true, missing: true}
	case err != nil:
		return freshStamp{}
	}
	return freshStamp{ok: true, mod: fi.ModTime().UnixNano(), size: fi.Size()}
}

// Current is the redactor for the values stored now.
func (f *Fresh) Current() *Redactor {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.stat()
	if st.ok && st == f.stamp && f.red != nil {
		return f.red
	}
	r, err := f.store.LoadRedactor()
	if err != nil || !st.ok {
		r = Withholding()
	}
	f.red, f.stamp = r, st
	return r
}

// Redact is Current().Redact.
func (f *Fresh) Redact(b []byte) []byte { return f.Current().Redact(b) }

// Span is Current().Span.
func (f *Fresh) Span() int { return f.Current().Span() }

// Withholding returns a redactor that withholds every payload.
func Withholding() *Redactor { return &Redactor{broken: true} }

// LoadRedactor is Redactor for a session about to start: a missing store is
// empty, and one that exists but cannot be loaded is an error that names it.
func (s *Store) LoadRedactor() (*Redactor, error) {
	s.mu.Lock()
	m, err := s.load()
	s.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("refusing to start: the secrets store cannot be loaded, so stored values could not be redacted: %w. %s", err, fixHint)
	}
	if len(m) == 0 {
		return &Redactor{}, nil
	}
	pairs := make([]pair, 0, len(m))
	for name, value := range m {
		if value == "" {
			continue
		}
		label := "[secret:" + name + "]"
		// Text that embeds JSON holds the value escaped, so that form is a
		// needle too, with and without HTML escaping.
		seen := map[string]bool{}
		for _, n := range []string{value, escaped(value, true), escaped(value, false)} {
			if !seen[n] {
				seen[n] = true
				pairs = append(pairs, pair{n, label, utf8.RuneCountInString(value) < MinLength})
			}
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i].needle) > len(pairs[j].needle) })
	return &Redactor{pairs: pairs}, nil
}

func escaped(v string, html bool) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(html)
	_ = enc.Encode(v)
	out := strings.TrimSuffix(b.String(), "\n")
	return out[1 : len(out)-1]
}

// Redact returns the payload with every stored value replaced. A payload that
// is not valid JSON is scanned the same way, literal by literal.
func (r *Redactor) Redact(b []byte) []byte {
	if r.broken {
		return nil // not JSON, so every caller withholds the payload
	}
	if len(r.pairs) == 0 {
		return b
	}
	var out []byte
	last := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '"' {
			continue
		}
		j := i + 1
		for j < len(b) && b[j] != '"' {
			if b[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(b) {
			break
		}
		k := j + 1
		for k < len(b) && (b[k] == ' ' || b[k] == '\t' || b[k] == '\n' || b[k] == '\r') {
			k++
		}
		key := k < len(b) && b[k] == ':'
		var text string
		if json.Unmarshal(b[i:j+1], &text) == nil {
			if red := r.replace(text, key); red != text {
				enc, _ := json.Marshal(red)
				out = append(append(out, b[last:i]...), enc...)
				last = j + 1
			}
		}
		i = j
	}
	if out == nil {
		return b
	}
	return append(out, b[last:]...)
}

// replace redacts one string; in a key, a short value is left alone.
func (r *Redactor) replace(s string, key bool) string {
	for _, p := range r.pairs {
		if key && p.short {
			continue
		}
		s = strings.ReplaceAll(s, p.needle, p.label)
	}
	return s
}

// Span is the byte length of the longest text a value is matched as, or 0
// when none is stored: the most a caller streaming fragments must hold back.
func (r *Redactor) Span() int {
	if len(r.pairs) == 0 {
		return 0
	}
	return len(r.pairs[0].needle)
}

// FindFold is Find with case ignored.
func (r *Redactor) FindFold(s string) (label string, found bool) {
	if r.broken {
		return "", true
	}
	s = strings.ToLower(s)
	for _, p := range r.pairs {
		if strings.Contains(s, strings.ToLower(p.needle)) {
			return p.label, true
		}
	}
	return "", false
}

// Find reports whether s holds a stored value, and its label. A store that
// could not be loaded holds everything, since nothing can be ruled out.
func (r *Redactor) Find(s string) (label string, found bool) {
	if r.broken {
		return "", true
	}
	for _, p := range r.pairs {
		if strings.Contains(s, p.needle) {
			return p.label, true
		}
	}
	return "", false
}
