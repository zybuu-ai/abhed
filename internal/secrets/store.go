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
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
	"weak"
)

// EnvFile names the environment variable that overrides the store's location.
const EnvFile = "ABHED_SECRETS_FILE"

var validName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// ValidName reports whether name is one a secret can be stored under.
func ValidName(name string) bool { return validName.MatchString(name) }

// Store is a file of named values, readable by its owner only. Every Store
// opened on one path in this process shares one lock, so a server that opens
// a store per request cannot lose one write to another; a write also takes a
// file lock beside the store, so another process writing it cannot either.
type Store struct {
	path string
	mu   *sync.Mutex
	// src, when set, holds the values in place of the file at path.
	src  Source
	name string
	// addHint says how a missing secret is added, %s the name; "" names
	// `abhed secret set`.
	addHint string
}

// pathLocks holds each path's lock weakly: the entry goes once no Store on
// the path is left, so a long-lived server opening a store per account does
// not keep a lock for every path it ever saw.
var (
	pathLocksMu sync.Mutex
	pathLocks   = map[string]weak.Pointer[sync.Mutex]{} // cleaned absolute path
)

func lockFor(path string) *sync.Mutex {
	key := filepath.Clean(path)
	if abs, err := filepath.Abs(path); err == nil {
		key = abs
	}
	pathLocksMu.Lock()
	defer pathLocksMu.Unlock()
	if mu := pathLocks[key].Value(); mu != nil {
		return mu
	}
	mu := new(sync.Mutex)
	w := weak.Make(mu)
	pathLocks[key] = w
	runtime.AddCleanup(mu, func(key string) {
		pathLocksMu.Lock()
		defer pathLocksMu.Unlock()
		if cur, ok := pathLocks[key]; ok && cur.Value() == nil {
			delete(pathLocks, key)
		}
	}, key)
	return mu
}

// lockedPaths is how many paths hold a lock entry, for tests.
func lockedPaths() int {
	pathLocksMu.Lock()
	defer pathLocksMu.Unlock()
	return len(pathLocks)
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

func Open(path string) *Store { return &Store{path: path, mu: lockFor(path)} }

// EnvAccountsDir names the environment variable that overrides AccountsDir.
const EnvAccountsDir = "ABHED_ACCOUNT_SECRETS_DIR"

// AccountsDir is where a server that keeps a store per account keeps them:
// $ABHED_ACCOUNT_SECRETS_DIR, else beside the operator's store, as secrets.d.
// It is state, refused to the agent's tools and sandbox like the store itself.
func AccountsDir() (string, error) {
	if p := os.Getenv(EnvAccountsDir); p != "" {
		return p, nil
	}
	p, err := DefaultPath()
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(p, filepath.Ext(p)) + ".d", nil
}

// Default opens the store at DefaultPath, the one the CLI uses. A path that
// cannot be worked out names a file that never exists, so the store is empty.
func Default() *Store {
	path, err := DefaultPath()
	if err != nil {
		path = ".abhed-secrets-unavailable"
	}
	return Open(path)
}

// AddedWith has a missing secret's error say hint, with %s the name, in place
// of `abhed secret set`: an account's store is added to over the API.
func (s *Store) AddedWith(hint string) *Store {
	s.addHint = hint
	return s
}

// Path reports where the store lives.
func (s *Store) Path() string { return s.path }

// MaxFileSize bounds the store: far beyond any set of credentials.
const MaxFileSize = 1 << 20

func (s *Store) load() (map[string]string, error) {
	if s.src != nil {
		return s.loadSource()
	}
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
		return nil, fmt.Errorf("the secrets store cannot be loaded: %w. %s", err, s.hint())
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
	// A temporary name of its own (CreateTemp makes it 0600), so no other
	// writer can truncate it.
	f, err := os.CreateTemp(filepath.Dir(s.path), "."+filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // gone after the rename; left by a failure
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
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
	if s.src != nil {
		return s.srcCall(func(ctx context.Context) error { return s.src.Set(ctx, name, value) })
	}
	unlock, err := fileLock(s.path)
	if err != nil {
		return err
	}
	defer unlock()
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
	if s.src != nil {
		return s.srcCall(func(ctx context.Context) error { return s.src.Remove(ctx, name) })
	}
	unlock, err := fileLock(s.path)
	if err != nil {
		return err
	}
	defer unlock()
	m, err := s.loadFixable()
	if err != nil {
		return err
	}
	delete(m, name)
	return s.save(m)
}

// Delete removes the whole store, under the lock Set and Remove take: a value
// being stored lands in the file before it is removed, never in one written
// back after. A missing store is not an error. A later Set makes a new one.
func (s *Store) Delete() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.src != nil {
		return fmt.Errorf("%s is kept by its driver; remove its secrets by name", s.name)
	}
	unlock, err := fileLock(s.path)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
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

// Offered lists what is stored, or nothing when the store cannot be read: a
// use of a name then reports why.
func (s *Store) Offered() []string {
	names, err := s.Names()
	if err != nil {
		return nil
	}
	return names
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
			if s.addHint != "" {
				return nil, fmt.Errorf("no secret named %s is stored; %s", n, fmt.Sprintf(s.addHint, n))
			}
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

	once      sync.Once
	all, keys *strings.Replacer
}

type pair struct {
	needle, label string
	// short marks a value under MinLength, which leaves JSON keys alone.
	short bool
	// encoded marks a form the value takes encoded or in another case, which
	// file path checks leave out.
	encoded bool
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

// Session is a Fresh redactor for one session, starting from the store as it
// is now. A store that cannot be loaded is an error, with a Fresh that
// withholds every payload until the store loads again.
func (l *Live) Session() (*Fresh, error) {
	r, err := l.store.LoadRedactor()
	if err != nil {
		return l.store.Fresh(Withholding()), err
	}
	return l.store.Fresh(r), nil
}

// Loads reports whether the store can be loaded now.
func (l *Live) Loads() error {
	_, err := l.store.LoadRedactor()
	return err
}

// Session is a Fresh redactor starting from the store as it is now; a store
// that cannot be loaded withholds every payload until it loads again.
func (s *Store) Session() *Fresh { return s.Fresh(s.Redactor()) }

// Fresh redacts with the values stored at each call, reading the store again
// whenever the file has changed, for a session that runs while secrets are
// added: a value bash can be given must be redacted from that moment on.
// Every value loaded during the session stays redacted after it is rotated
// or removed, since a command may have been given it before. A store that
// stops loading withholds every payload until it loads again.
type Fresh struct {
	store *Store
	mu    sync.Mutex
	stamp freshStamp
	red   *Redactor
	// down is set while the store at stamp could not be loaded.
	down bool
}

type freshStamp struct {
	ok      bool
	missing bool
	mod     int64
	size    int64
	inode   uint64
	ctime   int64
	// digest stands for a source's values, which have no file to stat.
	digest [32]byte
}

// Fresh returns a redactor over the store that starts from first, the
// reading a session was admitted with.
func (s *Store) Fresh(first *Redactor) *Fresh {
	f := &Fresh{store: s, red: first}
	f.stamp = f.stat()
	return f
}

func (f *Fresh) stat() freshStamp {
	if f.store.src != nil {
		return f.store.sourceStamp()
	}
	fi, err := os.Stat(f.store.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return freshStamp{ok: true, missing: true}
	case err != nil:
		return freshStamp{}
	}
	inode, ctime := fileIdentity(fi)
	return freshStamp{ok: true, mod: fi.ModTime().UnixNano(), size: fi.Size(), inode: inode, ctime: ctime}
}

// Current is the redactor for the values stored now.
func (f *Fresh) Current() *Redactor {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.stat()
	if st.ok && st == f.stamp && f.red != nil {
		if f.down {
			return Withholding()
		}
		return f.red
	}
	r, err := f.store.LoadRedactor()
	if err != nil || !st.ok {
		// Kept apart from the values seen so far, which a reload restores.
		f.stamp, f.down = st, true
		return Withholding()
	}
	if f.red != nil && !f.red.broken {
		r = r.union(f.red)
	}
	f.red, f.stamp, f.down = r, st, false
	return r
}

// union is r with every value of old it lacks, longest first as ever.
func (r *Redactor) union(old *Redactor) *Redactor {
	have := map[string]bool{}
	for _, p := range r.pairs {
		have[p.needle] = true
	}
	pairs := append([]pair(nil), r.pairs...)
	for _, p := range old.pairs {
		if !have[p.needle] {
			pairs = append(pairs, p)
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i].needle) > len(pairs[j].needle) })
	return &Redactor{pairs: pairs}
}

// Redact is Current().Redact.
func (f *Fresh) Redact(b []byte) []byte { return f.Current().Redact(b) }

// Span is Current().Span.
func (f *Fresh) Span() int { return f.Current().Span() }

// Names is Current().Names.
func (f *Fresh) Names() []string { return f.Current().Names() }

// FindSent is Current().FindSent.
func (f *Fresh) FindSent(text string) (string, bool) { return f.Current().FindSent(text) }

// FindInPath is Current().FindInPath.
func (f *Fresh) FindInPath(path string) (string, bool) { return f.Current().FindInPath(path) }

// Current is r itself, so a fixed reading can be joined with Fresh ones.
func (r *Redactor) Current() *Redactor { return r }

// Joined redacts with the values of all its parts as one redactor, each read
// as it is at the call; any part that cannot be loaded withholds every payload.
type Joined []interface{ Current() *Redactor }

// Current is the union of every part's values now. A value two parts hold
// is labelled by the earlier part, so a session's own name for it shows.
func (j Joined) Current() *Redactor {
	r := &Redactor{}
	for _, p := range j {
		c := p.Current()
		if c == nil || c.broken {
			return Withholding()
		}
		r = r.union(c)
	}
	return r
}

// Redact is Current().Redact.
func (j Joined) Redact(b []byte) []byte { return j.Current().Redact(b) }

// Span is Current().Span.
func (j Joined) Span() int { return j.Current().Span() }

// Names is Current().Names.
func (j Joined) Names() []string { return j.Current().Names() }

// FindSent is Current().FindSent.
func (j Joined) FindSent(text string) (string, bool) { return j.Current().FindSent(text) }

// FindInPath is Current().FindInPath.
func (j Joined) FindInPath(path string) (string, bool) { return j.Current().FindInPath(path) }

// Withholding returns a redactor that withholds every payload.
func Withholding() *Redactor { return &Redactor{broken: true} }

// LoadRedactor is Redactor for a session about to start: a missing store is
// empty, and one that exists but cannot be loaded is an error that names it.
func (s *Store) LoadRedactor() (*Redactor, error) {
	s.mu.Lock()
	m, err := s.load()
	s.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("refusing to start: the secrets store cannot be loaded, so stored values could not be redacted: %w. %s", err, s.hint())
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
		short := utf8.RuneCountInString(value) < MinLength
		for _, n := range []string{value, escaped(value, true), escaped(value, false)} {
			if !seen[n] {
				seen[n] = true
				pairs = append(pairs, pair{needle: n, label: label, short: short})
			}
		}
		for _, n := range encodedForms(value) {
			if n != "" && !seen[n] {
				seen[n] = true
				pairs = append(pairs, pair{needle: n, label: label, encoded: true})
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

// Redact returns the payload with every stored value replaced. In valid JSON
// each string literal is matched on its own; a payload that is not valid JSON,
// such as plain text, is matched as text, whole.
func (r *Redactor) Redact(b []byte) []byte {
	if r.broken {
		return nil // not JSON, so every caller withholds the payload
	}
	if len(r.pairs) == 0 {
		return b
	}
	if !json.Valid(b) {
		if red := r.replace(string(b), false); red != string(b) {
			return []byte(red)
		}
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

// replace redacts one string in a single pass, longest value first, so a
// label already written is never matched again; in a key, a short value is
// left alone.
func (r *Redactor) replace(s string, key bool) string {
	r.once.Do(func() {
		var all, keys []string
		for _, p := range r.pairs {
			all = append(all, p.needle, p.label)
			if !p.short {
				keys = append(keys, p.needle, p.label)
			}
		}
		r.all, r.keys = strings.NewReplacer(all...), strings.NewReplacer(keys...)
	})
	if key {
		return r.keys.Replace(s)
	}
	return r.all.Replace(s)
}

// EncodedMinLength is the fewest characters a value must have to be looked
// for in its encoded forms; shorter encodings match ordinary text too often.
const EncodedMinLength = 12

// encodedForms are the spellings a value takes when a command or a client
// encodes it: base64 and base64url at each of the three byte alignments,
// hex, percent-encoding, and its lower and upper case. A value split,
// reversed or only partly sent is not among them.
func encodedForms(v string) []string {
	if utf8.RuneCountInString(v) < EncodedMinLength {
		return nil
	}
	out := []string{strings.ToLower(v), strings.ToUpper(v),
		hex.EncodeToString([]byte(v)), strings.ToUpper(hex.EncodeToString([]byte(v))),
		url.QueryEscape(v), url.PathEscape(v), strings.ReplaceAll(url.QueryEscape(v), "+", "%20")}
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		for off := 0; off < 3; off++ {
			out = append(out, alignedBase64(enc, v, off))
		}
	}
	return out
}

// alignedBase64 is the run of base64 characters that v alone decides when it
// starts off bytes into a 3-byte group, whatever bytes surround it.
func alignedBase64(enc *base64.Encoding, v string, off int) string {
	full := enc.EncodeToString(append(make([]byte, off), v...))
	start := (8*off + 5) / 6
	end := 8 * (off + len(v)) / 6
	if start >= end {
		return ""
	}
	return full[start:end]
}

// Span is the byte length of the longest text a value is matched as, or 0
// when none is stored: the most a caller streaming fragments must hold back.
func (r *Redactor) Span() int {
	if len(r.pairs) == 0 {
		return 0
	}
	return len(r.pairs[0].needle)
}

// Names are the names of the stored values, never the values.
func (r *Redactor) Names() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range r.pairs {
		name := strings.TrimSuffix(strings.TrimPrefix(p.label, "[secret:"), "]")
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
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

// FindSent reports whether text holds a stored value in any form it could
// take on its way to another server: as written, percent-encoded any number
// of times (a malformed escape elsewhere does not stop the decoding), with
// '+' as a space, and in any case, since a host or a search engine may fold
// it. A store that could not be loaded holds everything, with an empty label.
func (r *Redactor) FindSent(text string) (label string, found bool) {
	forms := []string{text, strings.ReplaceAll(text, "+", " ")}
	for s := text; len(forms) < 2+maxDecodes; {
		next := lenientUnescape(s)
		if next == s {
			break
		}
		forms = append(forms, next, strings.ReplaceAll(next, "+", " "))
		s = next
	}
	for _, f := range forms {
		if label, found := r.FindFold(f); found {
			return label, true
		}
	}
	return "", false
}

// maxDecodes bounds the rounds of percent-decoding; text encoded more deeply
// than this is not a form any server decodes back.
const maxDecodes = 16

// lenientUnescape turns each valid %XX into its byte and leaves the rest,
// a malformed escape included, as written.
func lenientUnescape(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

// PathMinLength is the fewest characters a value must have to be looked for
// in a file path: shorter ones, such as "postgres" or "test", name ordinary
// files and directories too often.
const PathMinLength = 12

// FindInPath reports whether a file path holds a stored value of at least
// PathMinLength characters, as written and in its case. A store that could
// not be loaded holds everything, with an empty label.
func (r *Redactor) FindInPath(path string) (label string, found bool) {
	if r.broken {
		return "", true
	}
	for _, p := range r.pairs {
		if !p.encoded && utf8.RuneCountInString(p.needle) >= PathMinLength && strings.Contains(path, p.needle) {
			return p.label, true
		}
	}
	return "", false
}
