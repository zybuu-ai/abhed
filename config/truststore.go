package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zybuu-ai/abhed/internal/nlink"
)

// The trust store is ~/.abhed/trust.json: the user's own state, which the
// file tools and every sandbox tier already keep the agent out of.

const (
	decisionTrusted  = "trusted"
	decisionDeclined = "declined"
)

// TrustRecord is one recorded decision about a workspace's configuration.
type TrustRecord struct {
	SHA256   string `json:"sha256"`
	Decision string `json:"decision"`
	// AgentsSHA256 is the hash of the agent definitions the decision covers.
	// A record from before definitions were covered has none, and decides
	// nothing about them.
	AgentsSHA256 string `json:"agents_sha256,omitempty"`
	// AgentsDecision, when set, is the decision about the definitions where it
	// differs from Decision: a person may decline new definitions and keep a
	// configuration file they trusted.
	AgentsDecision string    `json:"agents_decision,omitempty"`
	At             time.Time `json:"at"`
}

type trustFile struct {
	Version    int                    `json:"version"`
	Workspaces map[string]TrustRecord `json:"workspaces"`
}

// TrustStorePath is where decisions are kept.
func TrustStorePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".abhed", "trust.json"), nil
}

func readTrust() (trustFile, string, error) {
	f := trustFile{Version: 1, Workspaces: map[string]TrustRecord{}}
	path, err := TrustStorePath()
	if err != nil {
		return f, "", err
	}
	if n, err := nlink.Linked(path); err != nil {
		return f, path, err
	} else if n > 0 {
		return f, path, nlink.Refusal(path, n)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the user's own trust store under their home
	if errors.Is(err, os.ErrNotExist) {
		return f, path, nil
	}
	if err != nil {
		return f, path, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return trustFile{Version: 1, Workspaces: map[string]TrustRecord{}}, path, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Workspaces == nil {
		f.Workspaces = map[string]TrustRecord{}
	}
	return f, path, nil
}

func lookupTrust(workspace string) (TrustRecord, bool, error) {
	f, _, err := readTrust()
	if err != nil {
		return TrustRecord{}, false, err
	}
	e, ok := f.Workspaces[workspace]
	return e, ok, nil
}

// updateTrust rewrites the store through a temporary file, owner-only,
// holding the store's lock so two decisions at once both land.
func updateTrust(change func(map[string]TrustRecord)) error {
	path, err := TrustStorePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := lockTrust(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	return rewriteTrust(change)
}

// lockWait bounds how long a decision waits for another to finish writing.
var lockWait = 5 * time.Second

// lockTrust holds an exclusive lock on the lock file beside the store until
// the returned function runs, waiting at most lockWait. The file itself is
// never removed, so every writer locks the same one.
func lockTrust(lock string) (func(), error) {
	return lockPath(lock, "the trust store is busy: another abhed has held %s for %s; nothing was recorded")
}

// LockFile holds an exclusive lock on lock, a file beside the one it guards,
// until the returned function runs, waiting at most five seconds.
func LockFile(lock string) (func(), error) {
	return lockPath(lock, "%s is held by another abhed (waited %s); nothing was changed")
}

func lockPath(lock, busy string) (func(), error) {
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- a lock file beside the user's own file
	if err != nil {
		return nil, err
	}
	for deadline := time.Now().Add(lockWait); ; time.Sleep(20 * time.Millisecond) {
		ok, err := tryLockFile(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", lock, err)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf(busy, lock, lockWait)
		}
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}

func rewriteTrust(change func(map[string]TrustRecord)) error {
	f, path, err := readTrust()
	if err != nil && path == "" {
		return err
	}
	if err != nil && !isParseError(err) {
		return err
	}
	change(f.Workspaces)
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".trust-*.json")
	if err != nil {
		return err
	}
	// Gone after the rename; this only cleans up a failed write.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// isParseError lets a corrupt store be replaced by the next decision.
func isParseError(err error) bool {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	return errors.As(err, &syn) || errors.As(err, &typ)
}

// GrantTrust records that the person trusts the workspace's configuration
// with this content. sha256 is the hash of what they reviewed, not a re-read.
// It decides nothing about agent definitions; GrantReviewed covers both.
func GrantTrust(workspace, sha256 string) error {
	return RecordDecision(workspace, Reviewed{SHA256: sha256}, true, false)
}

// DeclineTrust records that the person chose not to trust this content, so
// they are not asked again until it changes.
func DeclineTrust(workspace, sha256 string) error {
	return RecordDecision(workspace, Reviewed{SHA256: sha256}, false, false)
}

// GrantReviewed trusts the configuration file and the agent definitions with
// exactly the content reviewed.
func GrantReviewed(workspace string, r Reviewed) error {
	return RecordDecision(workspace, r, true, true)
}

// RecordDecision records one answer about the reviewed content: whether the
// configuration file is trusted, and whether the agent definitions are.
func RecordDecision(workspace string, r Reviewed, configTrusted, agentsTrusted bool) error {
	if r.SHA256 == "" && r.AgentsSHA256 == "" {
		return fmt.Errorf("no configuration file or agent definitions to decide on in %s", workspace)
	}
	key := canonical(workspace)
	return updateTrust(func(m map[string]TrustRecord) {
		rec := TrustRecord{SHA256: r.SHA256, Decision: decisionOf(configTrusted), At: time.Now().UTC()}
		agentsSum, agentsDecision := r.AgentsSHA256, decisionOf(agentsTrusted)
		// A decision about the file alone keeps what was decided about the
		// definitions: abhed init must not forget a trust it did not review.
		if agentsSum == "" {
			if old, ok := m[key]; ok && old.AgentsSHA256 != "" {
				agentsSum, agentsDecision = old.AgentsSHA256, orDecision(old.AgentsDecision, old.Decision)
			}
		}
		if agentsSum != "" {
			rec.AgentsSHA256 = agentsSum
			if agentsDecision != rec.Decision {
				rec.AgentsDecision = agentsDecision
			}
		}
		m[key] = rec
	})
}

func decisionOf(trusted bool) string {
	if trusted {
		return decisionTrusted
	}
	return decisionDeclined
}

// RevokeTrust forgets any decision about the workspace and reports whether
// there was one.
func RevokeTrust(workspace string) (bool, error) {
	key := canonical(workspace)
	var had bool
	err := updateTrust(func(m map[string]TrustRecord) {
		_, had = m[key]
		delete(m, key)
	})
	return had, err
}

// TrustRecords lists every recorded decision by workspace.
func TrustRecords() (map[string]TrustRecord, error) {
	f, _, err := readTrust()
	return f.Workspaces, err
}

// InitWorkspace writes the starter configuration and trusts it: the person
// asked for exactly this content.
func InitWorkspace(workspace string) (string, error) {
	path := filepath.Join(workspace, ".abhed", "config.json")
	data, err := defaultConfigJSON()
	if err != nil {
		return path, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, err
	}
	// A config can carry keys. Owner-only, like every other file that can.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return path, err
	}
	return path, GrantTrust(workspace, hashOf(data))
}
