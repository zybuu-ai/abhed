package orgpolicy

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// StateVersion is the state file format.
const StateVersion = 1

// State is the client's enrolment: the source, the pinned keys and the last
// verified envelope, kept under ~/.abhed where the agent cannot reach it.
type State struct {
	V int `json:"v"`
	// Profile names the `abhed login` profile whose token fetches the policy.
	Profile string `json:"profile"`
	URL     string `json:"url"`
	// Keys are the pinned signing keys, base64 Ed25519.
	Keys []string `json:"keys"`
	// Envelope is the last policy that verified, and FetchedAt when.
	Envelope  *Envelope `json:"envelope,omitempty"`
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	// Epoch is the highest epoch accepted; an earlier one is a rollback.
	Epoch int64 `json:"epoch,omitempty"`
	// Failure is a fetched policy that did not verify, newer than Envelope;
	// while it stands the fallback applies.
	Failure *Failure `json:"failure,omitempty"`
	// LastAttempt and LastError are the last fetch and why it failed, for status.
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
}

// Failure is a fetched policy refused.
type Failure struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// PathOverride, when set, is the state file (tests only).
var PathOverride string

// Path is ~/.abhed/org-policy.json.
func Path() (string, error) {
	if PathOverride != "" {
		return PathOverride, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".abhed", "org-policy.json"), nil
}

// LoadState reads the state; a missing file is (nil, nil). A link, a file
// other users may read or write, or one that does not read is an error.
func LoadState(path string) (*State, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file, not a link", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s may be written by other users (mode %o)", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the person's own state file, checked above
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if st.V != StateVersion {
		return nil, fmt.Errorf("%s is format %d; this build reads %d", path, st.V, StateVersion)
	}
	return &st, nil
}

// SaveState writes st 0600, replacing the file in one step.
func SaveState(path string, st *State) error {
	st.V = StateVersion
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil && (fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular()) {
		return fmt.Errorf("%s must be a regular file, not a link", path)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
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
	return os.Rename(name, path)
}

// Machine is what the machine's own managed file says about the org policy.
type Machine struct {
	// URL, when set, is the only server whose policy is taken.
	URL string
	// Keys, when set, are the only keys trusted; statements cannot rotate them.
	Keys []string
	// Required applies the fallback when no verified policy is held.
	Required bool
	// MaxAge, when set, caps how long a policy is applied without a fresh one.
	MaxAge time.Duration
}

// Statuses an evaluation ends in.
const (
	StatusNone     = "none"     // not enrolled; nothing applies
	StatusApplied  = "applied"  // a verified, fresh policy applies
	StatusFallback = "fallback" // the fallback applies, over any cached policy
)

// Effective is what applies now.
type Effective struct {
	Status string
	// Reason says why the fallback applies, or why nothing does.
	Reason string
	// Doc is the verified policy document, nil when none. With Fallback set,
	// the caller lays the fallback over it (or over nothing).
	Doc      []byte
	Fallback bool
	// Statement is the verified statement, when there is one, and KeyID the key that signed it.
	Statement *Statement
	KeyID     string
	// URL and Profile are the enrolment's.
	URL, Profile string
	// Age is how old the statement is.
	Age time.Duration
}

// Pins are the keys a state is checked against: the machine's when it sets
// any, else the state's own.
func Pins(st *State, m Machine) ([]ed25519.PublicKey, error) {
	if len(m.Keys) > 0 {
		return ParseKeys(m.Keys)
	}
	if st == nil {
		return nil, nil
	}
	return ParseKeys(st.Keys)
}

// Evaluate decides what applies from the state (nil when none) and the
// machine's settings at now. It never contacts a server.
func Evaluate(st *State, m Machine, now time.Time) Effective {
	fallback := func(e Effective, why string) Effective {
		e.Status, e.Reason, e.Fallback = StatusFallback, why, true
		return e
	}
	if st == nil {
		if m.Required {
			return fallback(Effective{URL: m.URL}, "the managed configuration requires an org policy, and this machine is not signed in to "+orURL(m.URL))
		}
		return Effective{Status: StatusNone, Reason: "not signed in to an org policy"}
	}
	e := Effective{URL: st.URL, Profile: st.Profile}
	if m.URL != "" && st.URL != m.URL {
		if m.Required {
			return fallback(Effective{URL: m.URL}, fmt.Sprintf("the managed configuration takes the org policy only from %s, and this machine is signed in to %s", m.URL, st.URL))
		}
		return Effective{Status: StatusNone, Reason: fmt.Sprintf("the org policy from %s is not taken: the managed configuration names %s", st.URL, m.URL)}
	}
	if st.Envelope == nil {
		why := "no policy has been fetched yet"
		if st.Failure != nil {
			why = "the fetched policy was refused: " + st.Failure.Reason
		}
		return fallback(e, why)
	}
	keys, err := Pins(st, m)
	if err != nil {
		return fallback(e, "the pinned keys do not read: "+err.Error())
	}
	stmt, kid, err := Open(*st.Envelope, keys)
	if err != nil {
		return fallback(e, "the cached policy does not verify: "+err.Error())
	}
	e.Statement, e.KeyID, e.Doc = &stmt, kid, []byte(stmt.Policy)
	e.Age = now.Sub(stmt.IssuedAt)
	maxAge := stmt.MaxAge()
	if m.MaxAge > 0 && m.MaxAge < maxAge {
		maxAge = m.MaxAge
	}
	switch {
	case st.Failure != nil && !st.Failure.At.Before(st.FetchedAt):
		return fallback(e, "the last fetched policy was refused: "+st.Failure.Reason)
	case stmt.IssuedAt.After(now.Add(Skew)):
		return fallback(e, "the cached policy was issued in the future ("+stmt.IssuedAt.UTC().Format(time.RFC3339)+"); check the clock")
	case e.Age > maxAge:
		return fallback(e, fmt.Sprintf("the cached policy is %s old, past its limit of %s, and no fresh one could be fetched", e.Age.Round(time.Minute), maxAge))
	}
	e.Status = StatusApplied
	return e
}

func orURL(u string) string {
	if u == "" {
		return "an org server"
	}
	return u
}

// Accept verifies env against st's pins (and subject and check, when set) and
// makes it st's policy, rotating pins; a refusal is kept in st.Failure.
func Accept(st *State, m Machine, env Envelope, subject string, check func([]byte) error, now time.Time) (Statement, error) {
	stmt, err := accept(st, m, env, subject, check)
	if err != nil {
		st.Failure = &Failure{At: now, Reason: err.Error()}
		return stmt, err
	}
	e := env
	st.Envelope, st.FetchedAt, st.Failure = &e, now, nil
	st.Epoch = max(st.Epoch, stmt.Epoch)
	if len(m.Keys) == 0 && len(stmt.Keys) > 0 {
		st.Keys = append([]string(nil), stmt.Keys...)
	}
	return stmt, nil
}

func accept(st *State, m Machine, env Envelope, subject string, check func([]byte) error) (Statement, error) {
	keys, err := Pins(st, m)
	if err != nil {
		return Statement{}, err
	}
	stmt, _, err := Open(env, keys)
	if err != nil {
		return stmt, err
	}
	if subject != "" && stmt.Subject != "" && stmt.Subject != subject {
		return stmt, fmt.Errorf("the policy was issued for %q, not %q", stmt.Subject, subject)
	}
	held := st.Epoch
	if st.Envelope != nil {
		if old, _, err := Open(*st.Envelope, keys); err == nil {
			held = max(held, old.Epoch)
		}
	}
	if stmt.Epoch < held {
		return stmt, fmt.Errorf("the policy is from epoch %d, older than the %d already held: a rollback is refused", stmt.Epoch, held)
	}
	if check != nil {
		if err := check(stmt.Policy); err != nil {
			return stmt, fmt.Errorf("the policy does not load: %w", err)
		}
	}
	return stmt, nil
}
