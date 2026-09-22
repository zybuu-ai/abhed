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

// Redactor returns a function that replaces every stored value in a JSON
// payload with [secret:NAME]. Values are matched in their JSON-escaped form,
// which is how they would appear inside an event, and the longest first so a
// value that contains another is replaced whole.
func (s *Store) Redactor() func([]byte) []byte {
	s.mu.Lock()
	m, err := s.load()
	s.mu.Unlock()
	if err != nil || len(m) == 0 {
		return func(b []byte) []byte { return b }
	}
	type pair struct{ needle, label string }
	pairs := make([]pair, 0, len(m))
	for name, value := range m {
		esc, _ := json.Marshal(value)
		needle := string(esc[1 : len(esc)-1])
		if needle == "" {
			continue
		}
		pairs = append(pairs, pair{needle, "[secret:" + name + "]"})
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].needle) > len(pairs[j].needle) })
	return func(b []byte) []byte {
		text := string(b)
		for _, p := range pairs {
			text = strings.ReplaceAll(text, p.needle, p.label)
		}
		return []byte(text)
	}
}
