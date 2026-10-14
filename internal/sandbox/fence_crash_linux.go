package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

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
