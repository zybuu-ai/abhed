//go:build unix

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fenceMark is left under ~/.abhed/fence while a fenced session runs; one whose
// process is gone marks a session that skipped its last check.
type fenceMark struct {
	PID int `json:"pid"`
	// State is whether the workspace had a .abhed as the session started.
	State bool `json:"state"`
}

// markDir is ~/.abhed/fence, made private.
func markDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, stateDir, "fence")
	return dir, os.MkdirAll(dir, 0o700)
}

// markPrefix names ws's marks.
func markPrefix(ws string) string {
	sum := sha256.Sum256([]byte(RealPath(ws)))
	return hex.EncodeToString(sum[:8]) + "-"
}

// sweepCrashed takes out a .abhed a crashed fenced session left behind and
// removes stale marks; rec hears what was taken out.
func (f *Fence) sweepCrashed(rec func(string, map[string]any) error) {
	dir, err := markDir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := markPrefix(f.policy.Workspace)
	// While another session on the workspace runs, its fences may mount over
	// the .abhed, and moving it would uncover their commands: nothing is taken.
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".mark") {
			continue
		}
		var m fenceMark
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && json.Unmarshal(b, &m) == nil && // #nosec G304 -- a mark under ~/.abhed/fence
			m.PID > 1 && m.PID != os.Getpid() && processAlive(m.PID) {
			return
		}
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".mark") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		b, err := os.ReadFile(p) // #nosec G304 -- a mark under ~/.abhed/fence
		var m fenceMark
		if err != nil || json.Unmarshal(b, &m) != nil || m.PID <= 1 || m.PID == os.Getpid() || processAlive(m.PID) {
			continue
		}
		_ = os.Remove(p)
		if m.State {
			continue
		}
		last := info.ModTime().Add(2 * time.Second)
		found, _ := stateEntries(f.policy.Workspace)
		var taken []map[string]any
		for _, s := range found {
			si, err := os.Lstat(s)
			if err != nil || entryTime(si).After(last) {
				continue
			}
			taken = append(taken, f.takeOut(s, filepath.Dir(s)))
		}
		if len(taken) > 0 && rec != nil {
			_ = rec(EvFenceStatePlanted, map[string]any{"when": "after_unclean_exit", "entries": taken,
				"reason": "a session that ended without closing its fence had no .abhed when it started, and one dating from it was found"})
		}
	}
}

// entryTime is when fi last changed as a command could not fake it: its
// ctime, or else its mtime, a future one counted as now's past.
func entryTime(fi os.FileInfo) time.Time {
	if ns, ok := changeTime(fi); ok {
		return time.Unix(0, ns)
	}
	if t := fi.ModTime(); !t.After(time.Now()) {
		return t
	}
	return time.Time{}
}

// removeMark removes this session's mark at a clean close.
func removeMark(p string) {
	if p != "" {
		_ = os.Remove(p)
	}
}
