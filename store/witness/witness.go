// Package witness keeps the heads of the record somewhere the machine's owner
// does not control.
//
// A hash chain shows an edit by anyone who did not recompute it. The owner of
// the machine or the database can recompute it, or put back an older copy
// whose chain is whole. A head handed, as it is written, to a store outside
// that owner's reach is what catches both: the record must still hold every
// head the witness was given, line for line.
//
// Each head is a signed statement. The signature says which installation sent
// it; it does not stop the owner, who holds the key. What stops the owner is
// where the statements go: an append-only file on storage they cannot rewrite
// (a WORM or object-lock mount, another host's share), or an HTTPS endpoint
// that keeps what it is sent.
package witness

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Config says where heads go. File and URL may both be set.
type Config struct {
	// File is appended one statement per line. It should be on storage the
	// machine's owner cannot rewrite; a symbolic link is refused.
	File string
	// URL is sent each statement as a JSON POST, with Token as a bearer token.
	URL   string
	Token string
	// KeyFile holds the installation's signing key, made (0600) when missing.
	KeyFile string
	// Host names the installation in each statement; "" is the hostname.
	Host string
	// Client is the HTTP client for URL; nil is one with a 10 second timeout.
	Client *http.Client
	// OnError, when set, is told each failure to deliver a head as it
	// happens, from the writer's or a sender's goroutine; it must not block.
	// Errors then reports nothing.
	OnError func(error)
}

// Statement is one head as the witness keeps it.
type Statement struct {
	V int `json:"v"`
	// Store is "local" for the local record, "postgres" for the database.
	Store   string `json:"store"`
	Host    string `json:"host"`
	Tenant  string `json:"tenant"`
	Session string `json:"session"`
	// Lines is how many lines (local) or chained events (postgres) the
	// record held, and Hash the hash of the last.
	Lines int64     `json:"lines"`
	Seq   int64     `json:"seq,omitempty"`
	Hash  string    `json:"hash"`
	At    time.Time `json:"at"`
	Key   string    `json:"key"`
	Sig   string    `json:"sig,omitempty"`
}

// signed is the statement's bytes as signed: itself without the signature.
func (s Statement) signed() []byte {
	s.Sig = ""
	b, _ := json.Marshal(s)
	return b
}

// Verify checks the statement's signature against the key it names.
func (s Statement) Verify() error {
	key, err := base64.StdEncoding.DecodeString(s.Key)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("the statement names no valid key")
	}
	sig, err := base64.StdEncoding.DecodeString(s.Sig)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(key), s.signed(), sig) {
		return errors.New("the statement's signature does not hold")
	}
	return nil
}

// Witness sends heads to the configured places. Its methods never block on
// the network: a POST runs in the background, and Close waits for those.
type Witness struct {
	cfg  Config
	key  ed25519.PrivateKey
	mu   sync.Mutex // orders file appends
	wg   sync.WaitGroup
	errs chan error
}

// Open loads or makes the signing key and checks the file can be appended.
func Open(cfg Config) (*Witness, error) {
	if cfg.File == "" && cfg.URL == "" {
		return nil, errors.New("witness: neither a file nor a URL is set")
	}
	if cfg.Host == "" {
		cfg.Host, _ = os.Hostname()
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.URL != "" {
		if u, err := url.Parse(cfg.URL); err != nil || u.Host == "" || u.Scheme != "https" && u.Scheme != "http" {
			return nil, errors.New("witness: the URL is not an http(s) address with a host")
		}
	}
	key, err := loadKey(cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	w := &Witness{cfg: cfg, key: key, errs: make(chan error, 16)}
	if cfg.File != "" {
		f, err := openAppend(cfg.File)
		if err != nil {
			return nil, err
		}
		_ = f.Close()
	}
	return w, nil
}

// Errors reports failures to deliver a head, without blocking the writer,
// when Config.OnError is not set; at most the first 16 unread are kept.
func (w *Witness) Errors() <-chan error { return w.errs }

func (w *Witness) fail(err error) {
	if w.cfg.OnError != nil {
		w.cfg.OnError(err)
		return
	}
	select {
	case w.errs <- err:
	default:
	}
}

// Local is the local store's anchor: a session's head (or the index's,
// session "index") as it is synced.
func (w *Witness) Local(tenant, session string, lines, seq int64, hash string) {
	w.send(Statement{Store: "local", Tenant: tenant, Session: session, Lines: lines, Seq: seq, Hash: hash})
}

// Postgres is the database store's anchor: a session's event chain head.
func (w *Witness) Postgres(tenant, session string, pos int64, hash string) {
	w.send(Statement{Store: "postgres", Tenant: tenant, Session: session, Lines: pos, Hash: hash})
}

func (w *Witness) send(s Statement) {
	s.V, s.Host, s.At = 1, w.cfg.Host, time.Now().UTC()
	s.Key = base64.StdEncoding.EncodeToString(w.key.Public().(ed25519.PublicKey))
	s.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(w.key, s.signed()))
	line, _ := json.Marshal(s)
	if w.cfg.File != "" {
		w.mu.Lock()
		err := appendLine(w.cfg.File, line)
		w.mu.Unlock()
		if err != nil {
			w.fail(err)
		}
	}
	if w.cfg.URL != "" {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			if err := w.post(line); err != nil {
				w.fail(err)
			}
		}()
	}
}

func (w *Witness) post(body []byte) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("witness: a request to %s: %w", shown(w.cfg.URL), err)
	}
	req.Header.Set("Content-Type", "application/json")
	if w.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+w.cfg.Token)
	}
	resp, err := w.cfg.Client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = &url.Error{Op: ue.Op, URL: shown(w.cfg.URL), Err: ue.Err}
		}
		return fmt.Errorf("witness: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("witness: %s answered %s", shown(w.cfg.URL), resp.Status)
	}
	return nil
}

// shown is the witness's URL for an error, without user information, query
// or fragment, any of which may carry a credential.
func shown(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparsable URL)"
	}
	u.RawQuery, u.User, u.Fragment, u.RawFragment, u.ForceQuery = "", nil, "", "", false
	return u.String()
}

// Close waits up to timeout for heads still being posted.
func (w *Witness) Close(timeout time.Duration) {
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		w.fail(errors.New("witness: heads were still being sent at close"))
	}
}

// openAppend opens path for appending, refusing a link.
func openAppend(path string) (*os.File, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("witness: %s is a symbolic link", path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) // #nosec G304 -- the managed configuration names it
	if err != nil {
		return nil, fmt.Errorf("witness: %w", err)
	}
	return f, nil
}

func appendLine(path string, line []byte) error {
	f, err := openAppend(path)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	serr := f.Sync()
	cerr := f.Close()
	return errors.Join(werr, serr, cerr)
}

// loadKey reads the signing key's seed, making one when the file is missing.
func loadKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("witness: no key file is set")
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the key file the store keeps
	if errors.Is(err, os.ErrNotExist) {
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- as above
		if err != nil {
			return nil, fmt.Errorf("witness: make the signing key: %w", err)
		}
		_, werr := f.WriteString(hex.EncodeToString(seed) + "\n")
		if err := errors.Join(werr, f.Close()); err != nil {
			return nil, err
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if err != nil {
		return nil, fmt.Errorf("witness: read the signing key: %w", err)
	}
	seed, err := hex.DecodeString(string(bytes.TrimSpace(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("witness: %s is not a signing key", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// Read loads the statements in a witness file, one per line. A line that is
// not a statement, or whose signature does not hold, is returned as an error
// beside the rest.
func Read(r io.Reader) ([]Statement, []error) {
	var out []Statement
	var bad []error
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		var s Statement
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			bad = append(bad, fmt.Errorf("line %d: not a statement: %w", n, err))
			continue
		}
		if err := s.Verify(); err != nil {
			bad = append(bad, fmt.Errorf("line %d: %w", n, err))
			continue
		}
		out = append(out, s)
	}
	if err := sc.Err(); err != nil {
		bad = append(bad, err)
	}
	return out, bad
}

// Problem is a head the witness holds that the record no longer does.
// Removed marks one a recorded removal accounts for, such as a prune whose
// tombstone keeps the session's head: reported, but not a failure.
type Problem struct {
	Statement Statement
	Reason    string
	Removed   bool
}

// PublicKey is the installation's public key from its key file, which must
// exist: a check is never made against a key made for it.
func PublicKey(keyFile string) (ed25519.PublicKey, error) {
	if _, err := os.Stat(keyFile); err != nil {
		return nil, fmt.Errorf("witness: the installation's key: %w", err)
	}
	k, err := loadKey(keyFile)
	if err != nil {
		return nil, err
	}
	return k.Public().(ed25519.PublicKey), nil
}

// ParsePublicKey reads a public key as a statement carries it (base64).
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("witness: not a public key")
	}
	return ed25519.PublicKey(b), nil
}

// Removed says why a head the record no longer holds was removed (removed
// true), or why a removal does not account for it ("" for the general reason).
type Removed func(session string, line int64, hash string) (why string, removed bool, err error)

// Check compares statements for one store and tenant with the record as it
// is now: hashAt gives the hash at a line and whether the record holds it.
// A head the record no longer holds means lines were cut, rewritten or rolled
// back since the witness was given it, unless removed (which may be nil)
// names the recorded removal that took it. Each statement must be signed with
// key, the installation's: one that verifies under a key of its own proves
// nothing about who sent it.
func Check(stmts []Statement, key ed25519.PublicKey, store, tenant string, hashAt func(session string, line int64) (string, bool, error), removed Removed) ([]Problem, error) {
	if len(key) != ed25519.PublicKeySize {
		return nil, errors.New("witness: no installation key to check the statements against")
	}
	want := base64.StdEncoding.EncodeToString(key)
	var out []Problem
	for _, s := range stmts {
		if s.Store != store || s.Tenant != tenant {
			continue
		}
		if s.Key != want || s.Verify() != nil {
			out = append(out, Problem{s, fmt.Sprintf("a head of %s sent %s is not signed by this installation's key: it was not sent by this installation",
				s.Session, s.At.Format(time.RFC3339)), false})
			continue
		}
		h, ok, err := hashAt(s.Session, s.Lines)
		if err != nil {
			return out, err
		}
		switch {
		case !ok:
			why, gone := "", false
			if removed != nil {
				if why, gone, err = removed(s.Session, s.Lines, s.Hash); err != nil {
					return out, err
				}
			}
			if why != "" {
				out = append(out, Problem{s, why, gone})
				continue
			}
			out = append(out, Problem{s, fmt.Sprintf("the witness holds line %d of %s, sent %s, and the record no longer has it: lines were cut or an older copy was put back",
				s.Lines, s.Session, s.At.Format(time.RFC3339)), false})
		case h != s.Hash:
			out = append(out, Problem{s, fmt.Sprintf("line %d of %s is not the one the witness was given %s: the record was rewritten",
				s.Lines, s.Session, s.At.Format(time.RFC3339)), false})
		}
	}
	return out, nil
}

// Failed counts the problems no recorded removal accounts for.
func Failed(problems []Problem) int {
	n := 0
	for _, p := range problems {
		if !p.Removed {
			n++
		}
	}
	return n
}
