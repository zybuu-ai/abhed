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

// fenceMark is what a fenced session leaves while it runs, under
// ~/.abhed/fence: removed at a clean close, so one whose process is gone
// marks a session that ended without its last check.
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

// writeMark leaves this session's mark; "" when it cannot.
func (f *Fence) writeMark() string {
	dir, err := markDir()
	if err != nil {
		return ""
	}
	entries, _ := stateEntries(f.policy.Workspace)
	b, _ := json.Marshal(fenceMark{PID: os.Getpid(), State: len(entries) > 0})
	p := filepath.Join(dir, markPrefix(f.policy.Workspace)+f.id+".mark")
	if os.WriteFile(p, b, 0o600) != nil {
		return ""
	}
	return p
}

// touchMark says the session ran a command now: what appears later is not its doing.
func touchMark(p string) {
	if p != "" {
		now := time.Now()
		_ = os.Chtimes(p, now, now)
	}
}

// sweepCrashed takes out a .abhed that a session which ended without
// closing its fence left behind: one with no .abhed when it started, whose
// .abhed now dates from no later than its last command. Each stale mark is
// removed. rec hears what was taken out.
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
			if err != nil || si.ModTime().After(last) {
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

// removeMark removes this session's mark at a clean close.
func removeMark(p string) {
	if p != "" {
		_ = os.Remove(p)
	}
}

// stateWatch is how often a landlock_only session looks for a planted .abhed
// while its commands run, beside the checks before and after each.
var stateWatch = time.Second

// watchState looks for a planted .abhed every stateWatch until the session
// closes, so one a long command makes is taken out within that time.
func (f *Fence) watchState() {
	f.watchStop = make(chan struct{})
	stop := f.watchStop
	go func() {
		t := time.NewTicker(stateWatch)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if f.planted.Load() == nil && !f.closed.Load() {
					_ = f.checkPlanted(f.sessionRecord(), "", "while_running")
				}
			}
		}
	}()
}
