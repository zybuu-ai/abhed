package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// footer is what the dock's footer shows. The session's state is read on
// the session's goroutine, at refresh, into a snapshot; the dock draws from
// the snapshot and from counters that are safe to read from anywhere (the
// renderer's usage, the background task count). Reading the session itself
// from the dock's goroutine would race the command changing it.
type footer struct {
	editor *ui.LineReader
	r      *ui.Renderer
	root   string

	mu      sync.Mutex
	base    ui.StatusModel
	bg      *agent.Background
	pending policy.Mode
	busy    bool

	// The custom status line: ready judges it on the session's goroutine
	// (where and what runs, or a notice in its place), failed says a failed
	// run once; line is its last output, running whether a run is in flight.
	ready   func() (sandbox.Sandbox, string, string)
	failed  func(error) string
	line    string
	running bool
	lastRun time.Time
}

// startFooter fills the footer and connects it to the dock.
func startFooter(editor *ui.LineReader, r *ui.Renderer, st *cliState, pol *policy.Engine, workspace string) *footer {
	f := &footer{editor: editor, r: r, root: workspace}
	if !editor.Raw() {
		return f
	}
	editor.Hotkey("shift+tab", func() { editor.Send(modeCycleLine) })
	// The statusline command runs as the session's statusline does: under
	// the process sandbox with the network off, judged again as folders change.
	f.ready, f.failed = st.statuslineReady, st.statuslineFailed
	f.refresh(st, pol)
	return f
}

// modeCycleLine is what Shift-Tab hands the session: the mode changes on the
// session's goroutine, like any command, never on the key reader's.
const modeCycleLine = "/\x00mode-cycle"

// refresh reads the session into the snapshot. It runs on the session's
// goroutine: at startup, after each command and around each turn.
func (f *footer) refresh(st *cliState, pol *policy.Engine) {
	if f == nil || !f.editor.Raw() {
		return
	}
	m := ui.StatusModel{
		Provider:    st.appCfg.Model.Default,
		Mode:        string(pol.Mode),
		ModeLocked:  st.appCfg.ManagedSets("permissions.mode"),
		Network:     st.appCfg.Sandbox.AllowNetwork,
		Record:      ui.RecordMemory,
		GitBranch:   gitBranch(f.root),
		Cwd:         homeRel(f.root),
		SessionName: st.sessionID,
	}
	if st.adapter != nil {
		m.Model = st.adapter.Profile().Name
	}
	if st.appCfg.Storage.Driver == "postgres" {
		m.Record = ui.RecordUnverified
	}
	m.SandboxTier, m.Network = st.sandbox.tierNow(m.Network)
	var bg *agent.Background
	if st.loop != nil {
		bg = st.loop.Background
	}
	f.mu.Lock()
	f.base, f.bg = m, bg
	f.mu.Unlock()
	f.editor.SetStatusFunc(f.model)
	f.runStatusLine()
}

// model is the footer as the dock draws it.
func (f *footer) model() ui.StatusModel {
	f.mu.Lock()
	m, bg, pending, line := f.base, f.bg, f.pending, f.line
	f.mu.Unlock()
	u := f.r.Usage()
	if u.Model != "" {
		m.Model = u.Model // what answered last, which a switch changes
	}
	m.ContextTokens, m.TokensIn, m.TokensOut = u.ContextTokens, u.TokensIn, u.TokensOut
	if pct := u.ContextPct(); pct >= 0 {
		m.ContextPercent = pct
	}
	m.BackgroundTasks = bg.Live()
	if pending != "" {
		m.PendingMode = string(pending)
	}
	m.Line = line
	return m
}

// turn marks a turn starting or ending.
func (f *footer) turn(busy bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.busy = busy
	f.mu.Unlock()
}

// cycleMode is Shift-Tab: the next mode in the offered cycle, which never
// holds auto or bypass. While a turn runs the change waits for its end,
// since the running turn reads the policy.
func (f *footer) cycleMode(st *cliState, pol *policy.Engine) {
	modes := &cliModes{st: st, pol: pol}
	offered := modes.Offered()
	if len(offered) == 0 {
		f.editor.Notify(ui.Toast{Text: "the managed configuration fixes the mode", Warn: true})
		return
	}
	f.mu.Lock()
	busy, cur := f.busy, f.pending
	f.mu.Unlock()
	if cur == "" {
		cur = pol.Mode
	}
	next := offered[0]
	for i, m := range offered {
		if m == cur {
			next = offered[(i+1)%len(offered)]
		}
	}
	if busy {
		f.mu.Lock()
		f.pending = next
		if next == pol.Mode {
			f.pending = ""
		}
		f.mu.Unlock()
		f.refresh(st, pol)
		return
	}
	if err := modes.Set(context.Background(), next, agent.ViaShiftTab); err != nil {
		f.editor.Notify(ui.Toast{Text: err.Error(), Warn: true})
	}
	f.refresh(st, pol)
}

// applyPending makes the mode chosen during a turn take effect, as it ends.
func (f *footer) applyPending(st *cliState, pol *policy.Engine) {
	if f == nil {
		return
	}
	f.mu.Lock()
	next := f.pending
	f.pending = ""
	f.mu.Unlock()
	if next != "" && next != pol.Mode {
		if err := (&cliModes{st: st, pol: pol}).Set(context.Background(), next, agent.ViaShiftTab); err != nil {
			f.editor.Notify(ui.Toast{Text: err.Error(), Warn: true})
		}
	}
	f.refresh(st, pol)
}

// runStatusLine runs the status line command in the background, at most
// once a second, with the footer's model as JSON on stdin and the first line
// of its output shown, styling kept. It is judged here, on the session's
// goroutine; a refusal or a failure shows in its place.
func (f *footer) runStatusLine() {
	if f.ready == nil {
		return
	}
	f.mu.Lock()
	if f.running || time.Since(f.lastRun) < time.Second {
		f.mu.Unlock()
		return
	}
	f.mu.Unlock()
	sb, command, notice := f.ready()
	f.mu.Lock()
	if sb == nil {
		if notice != "" {
			f.line = notice // a notice said once stays in the footer
		}
		f.mu.Unlock()
		return
	}
	f.running, f.lastRun = true, time.Now()
	f.mu.Unlock()
	m := f.model()
	go func() {
		defer ui.RestoreOnPanic()
		line, err := runStatusline(context.Background(), sb, f.root, command, m)
		if err != nil && f.failed != nil {
			line = f.failed(err)
		}
		f.mu.Lock()
		f.running = false
		if err == nil || line != "" {
			f.line = line
		}
		f.mu.Unlock()
		f.editor.SetStatusFunc(f.model)
	}()
}

// gitBranch is the branch checked out in dir's repository, read from its
// HEAD without running git; "" when there is none.
func gitBranch(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		git := filepath.Join(d, ".git")
		if info, err := os.Stat(git); err == nil {
			head := filepath.Join(git, "HEAD")
			if !info.IsDir() {
				// A worktree: .git names the real git directory.
				data, err := os.ReadFile(git) // #nosec G304 G703 -- the workspace's own .git, read to show its branch
				if err != nil {
					return ""
				}
				p := strings.TrimSpace(strings.TrimPrefix(string(data), "gitdir:"))
				if !filepath.IsAbs(p) {
					p = filepath.Join(d, p)
				}
				head = filepath.Join(p, "HEAD")
			}
			data, err := os.ReadFile(head) // #nosec G304 G703 -- the workspace's own git HEAD, read to show its branch
			if err != nil {
				return ""
			}
			ref := strings.TrimSpace(string(data))
			if b, ok := strings.CutPrefix(ref, "ref: refs/heads/"); ok {
				return b
			}
			if len(ref) >= 7 {
				return ref[:7] // detached
			}
			return ""
		}
		if parent := filepath.Dir(d); parent == d {
			return ""
		}
	}
}

// homeRel shows p with ~ for the home folder.
func homeRel(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			return "~"
		}
		if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
			return "~" + string(filepath.Separator) + rest
		}
	}
	return p
}
