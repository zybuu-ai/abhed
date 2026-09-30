package customcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zybuu-ai/abhed/internal/nlink"
)

// The person's decisions about workspace commands are kept in
// ~/.abhed/command-trust.json, the person's own state, which the file tools
// and every sandbox tier keep the agent out of. A decision covers one
// workspace's commands with exactly the content hashed.

// Trust decisions.
const (
	DecisionTrusted  = "trusted"
	DecisionDeclined = "declined"
)

// TrustRecord is one decision.
type TrustRecord struct {
	SHA256   string    `json:"sha256"`
	Decision string    `json:"decision"`
	At       time.Time `json:"at"`
}

type trustFile struct {
	Version    int                    `json:"version"`
	Workspaces map[string]TrustRecord `json:"workspaces"`
}

// TrustStore is where decisions are kept.
type TrustStore struct {
	Path string
}

// DefaultTrustStore is the person's store under their home directory.
func DefaultTrustStore() (TrustStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return TrustStore{}, err
	}
	return TrustStore{Path: filepath.Join(home, ".abhed", "command-trust.json")}, nil
}

func (s TrustStore) read() (trustFile, error) {
	f := trustFile{Version: 1, Workspaces: map[string]TrustRecord{}}
	if n, err := nlink.Linked(s.Path); err != nil {
		return f, err
	} else if n > 0 {
		return f, nlink.Refusal(s.Path, n)
	}
	data, err := os.ReadFile(s.Path) // #nosec G304 -- the person's own trust store under their home
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return trustFile{Version: 1, Workspaces: map[string]TrustRecord{}}, fmt.Errorf("parse %s: %w", s.Path, err)
	}
	if f.Workspaces == nil {
		f.Workspaces = map[string]TrustRecord{}
	}
	return f, nil
}

// Decide says whether a workspace's commands with this hash are trusted, and
// why: stored, new, changed or declined. An unreadable store trusts nothing.
func (s TrustStore) Decide(workspace, sum string) (bool, string) {
	f, err := s.read()
	if err != nil {
		return false, "new"
	}
	rec, ok := f.Workspaces[canonical(workspace)]
	switch {
	case !ok:
		return false, "new"
	case rec.SHA256 != sum:
		return false, "changed"
	case rec.Decision == DecisionTrusted:
		return true, "stored"
	}
	return false, "declined"
}

// Record keeps the person's decision about exactly this content.
func (s TrustStore) Record(workspace, sum string, trusted bool) error {
	if sum == "" {
		return errors.New("no commands to decide on")
	}
	f, err := s.read()
	if err != nil {
		return err
	}
	d := DecisionDeclined
	if trusted {
		d = DecisionTrusted
	}
	f.Workspaces[canonical(workspace)] = TrustRecord{SHA256: sum, Decision: d, At: time.Now().UTC()}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".command-trust-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

func canonical(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return abs
}
