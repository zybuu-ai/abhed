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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// EnvFile names the environment variable that overrides the store's location.
const EnvFile = "ABHED_SECRETS_FILE"

var validName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

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

// Path reports where the store lives.
func (s *Store) Path() string { return s.path }

func (s *Store) load() (map[string]string, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(s.path); err == nil && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others (mode %o); run chmod 600 on it", s.path, info.Mode().Perm())
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
	m, err := s.load()
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
	m, err := s.load()
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
	m, err := s.load()
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
	m, err := s.load()
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

// Redactor replaces every stored value in a JSON payload with [secret:NAME].
// Each string literal is decoded and matched as text, so a match never
// straddles an escape and the output is always valid JSON. The longest value
// is replaced first, so a value that contains another is replaced whole.
type Redactor struct {
	pairs []pair
}

type pair struct{ needle, label string }

// Redactor returns a redactor for the values stored now.
func (s *Store) Redactor() *Redactor {
	s.mu.Lock()
	m, err := s.load()
	s.mu.Unlock()
	if err != nil || len(m) == 0 {
		return &Redactor{}
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
				pairs = append(pairs, pair{n, label})
			}
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i].needle) > len(pairs[j].needle) })
	return &Redactor{pairs: pairs}
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
		var text string
		if json.Unmarshal(b[i:j+1], &text) == nil {
			if red := r.text(text); red != text {
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

func (r *Redactor) text(s string) string {
	for _, p := range r.pairs {
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
