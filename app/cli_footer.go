package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	sb     sandbox.Sandbox // runs the status line command, when there is one
	sbox   sandbox.Sandbox
	tier   string
	root   string

	mu      sync.Mutex
	base    ui.StatusModel
	bg      *agent.Background
	pending policy.Mode
	busy    bool

	// The custom status line: its command, its last output, and whether a
	// run is in flight.
	command string
	line    string
	running bool
	lastRun time.Time
}

// statusLineTimeout bounds a status line command's run, and statusLineMax
// what of its output is read.
const (
	statusLineTimeout = 2 * time.Second
	statusLineMax     = 4096
)

// startFooter fills the footer and connects it to the dock.
// sb is the session's sandbox, built once for the banner: probing it again
// can take seconds where a container runtime is installed.
func startFooter(editor *ui.LineReader, r *ui.Renderer, st *cliState, pol *policy.Engine, workspace string, sb sandbox.Sandbox) *footer {
	f := &footer{editor: editor, r: r, root: workspace, sbox: sb}
	if !editor.Raw() {
		return f
	}
	editor.Hotkey("shift+tab", func() { editor.Send(modeCycleLine) })
	if sb != nil {
		f.tier = string(sb.Tier())
	}
	// A status line command comes from the user's or a trusted workspace's
	// configuration: an untrusted workspace's is never loaded (config's
	// trust rules). It runs under the session's sandbox.
	f.command, f.sb, f.line = statusLineSetup(st.appCfg.Statusline.Command, f.sbox)
	f.refresh(st, pol)
	return f
}

// statusLineSetup decides how a status line command runs: under the
// session's sandbox, and not at all without one — it is a process the
// configuration names, and on the host it would run as the person with
// nothing between it and their files. The notice says why nothing shows.
func statusLineSetup(command string, sb sandbox.Sandbox) (run string, under sandbox.Sandbox, notice string) {
	command = strings.TrimSpace(command)
	switch {
	case command == "":
		return "", nil, ""
	case sb == nil || sb.Tier() == sandbox.TierNone:
		return "", nil, "statusline.command not run: it runs only under a sandbox, and this session has none"
	}
	return command, sb, ""
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
	m.SandboxTier = f.tier
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
// of its output shown. Its styling is kept; anything else it prints that
// could move the cursor is dropped when it is drawn.
func (f *footer) runStatusLine() {
	f.mu.Lock()
	if f.command == "" || f.running || time.Since(f.lastRun) < time.Second {
		f.mu.Unlock()
		return
	}
	f.running, f.lastRun = true, time.Now()
	f.mu.Unlock()
	input, _ := json.Marshal(f.model())
	go func() {
		defer ui.RestoreOnPanic()
		ctx, cancel := context.WithTimeout(context.Background(), statusLineTimeout)
		defer cancel()
		cmd := f.sb.Command(ctx, f.root, f.command)
		cmd.Stdin = bytes.NewReader(input)
		var out bytes.Buffer
		cmd.Stdout = &limitWriter{w: &out, n: statusLineMax}
		err := cmd.Run()
		line := ""
		if sc := bufio.NewScanner(&out); sc.Scan() {
			// Kept: text and colour. Dropped: anything that could move the
			// cursor, write the clipboard or retitle the window.
			line = strings.TrimSpace(ui.CleanText(sc.Text(), true))
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

// limitWriter keeps the first n bytes written to it and discards the rest.
type limitWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if room := l.n - l.w.Len(); room > 0 {
		l.w.Write(p[:min(room, len(p))])
	}
	return len(p), nil
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
