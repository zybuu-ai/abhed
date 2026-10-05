package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Background shells are bash run_in_background commands, kept as tasks of kind
// "shell" in the session's Background so they list, stop and notify as tasks do.

// Task kinds, as TaskInfo.Kind names them.
const (
	KindTask  = "task"
	KindShell = "shell"
)

// Shell states, as TaskInfo.Status names them for a shell.
const (
	ShellRunning = "running"
	ShellExited  = "exited"
	ShellKilled  = "killed"
)

// Event types for background shells; see ShellStarted and ShellEnded.
const (
	EvShellStarted EventType = "shell.started"
	EvShellEnded   EventType = "shell.ended"
)

// ShellStarted is the payload of shell.started.
type ShellStarted struct {
	ShellID     string   `json:"shell_id"`
	CallID      string   `json:"call_id"`
	Command     string   `json:"command"`
	Description string   `json:"description,omitempty"`
	Sandbox     string   `json:"sandbox,omitempty"`
	Secrets     []string `json:"secrets,omitempty"`
	LifetimeMS  int64    `json:"lifetime_ms"`
	// FromForeground marks a command the person moved to the background while
	// it ran in the foreground (Ctrl-B); it started under the same call.
	FromForeground bool `json:"from_foreground,omitempty"`
}

// ShellEnded is the payload of shell.ended.
type ShellEnded struct {
	ShellID     string `json:"shell_id"`
	CallID      string `json:"call_id"`
	State       string `json:"state"`
	ExitCode    int    `json:"exit_code"`
	Reason      string `json:"reason,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
	OutputBytes int64  `json:"output_bytes"`
	Truncated   bool   `json:"truncated"`
}

// DefaultMaxShells is limits.background_shells when nothing sets it.
const DefaultMaxShells = 4

// shellLastLine bounds the last output line a listing shows.
const shellLastLine = 200

// shellState is what a background shell task holds beyond a task's fields.
type shellState struct {
	proc    *tools.ShellProc
	command string
	callID  string
	// quiet: the agent killed it itself and its kill result said so; no notice.
	quiet bool
	state string
	exit  int
	// readMu orders reads; carry is read output held back from the model
	// because a stored secret may continue in what comes next.
	readMu sync.Mutex
	carry  string
}

// shellHost is the ShellHost a tool call gets: the session's Background,
// with the call that starts the shell.
type shellHost struct {
	b      *Background
	callID string
}

// withShellHost gives a tool call the session's background shells. Only the
// top-level loop of a session has a Background; a subagent's calls get a host that refuses.
func (l *Loop) withShellHost(ctx context.Context, callID string) context.Context {
	if l.depth > 0 {
		return tools.WithShellHost(ctx, subagentShells{})
	}
	if l.Background == nil {
		return ctx
	}
	return tools.WithShellHost(ctx, shellHost{b: l.Background, callID: callID})
}

// withDetach lets the person move this call's foreground command to the
// background (Ctrl-B) while it runs; done forgets it when the call ends.
func (l *Loop) withDetach(ctx context.Context) (context.Context, func()) {
	if l.depth > 0 || l.Background == nil || !l.Movable {
		return ctx, func() {}
	}
	d := tools.NewDetach()
	l.detachMu.Lock()
	if l.detaches == nil {
		l.detaches = map[*tools.Detach]bool{}
	}
	l.detaches[d] = true
	l.detachMu.Unlock()
	return tools.WithDetach(ctx, d), func() {
		l.detachMu.Lock()
		delete(l.detaches, d)
		l.detachMu.Unlock()
	}
}

// MoveToBackground moves every foreground command of this conversation's
// running calls to the background, as Ctrl-B asks, and reports how many it
// moved; a call waiting on approval, or not a command, is not moved.
func (l *Loop) MoveToBackground() int {
	l.detachMu.Lock()
	ds := make([]*tools.Detach, 0, len(l.detaches))
	for d := range l.detaches {
		ds = append(ds, d)
	}
	l.detachMu.Unlock()
	n := 0
	for _, d := range ds {
		if d.Ask() {
			n++
		}
	}
	return n
}

// noBackground is why a bash call asking to run in the background would be
// refused when it ran, or "": said before anyone is asked, not after an approval.
func (l *Loop) noBackground(call model.ToolCall) string {
	if call.Name != "bash" {
		return ""
	}
	var a struct {
		Background bool `json:"run_in_background"`
	}
	if json.Unmarshal(call.Args, &a) != nil || !a.Background {
		return ""
	}
	if l.depth > 0 {
		return "Could not start the command in the background: a subagent cannot start a background command; run it without run_in_background"
	}
	return ""
}

// subagentShells replaces the parent's host in a subagent's calls, which
// would otherwise inherit it through the task call's context.
type subagentShells struct{}

func (subagentShells) StartShell(context.Context, tools.ShellRequest) (string, error) {
	return "", errors.New("a subagent cannot start a background command; run it without run_in_background")
}

func (p BackgroundPolicy) maxShells() int { return max(p.MaxShells, 0) }

func (b *Background) liveShellsLocked() int {
	n := 0
	for _, t := range b.tasks {
		if !t.ended && t.Kind == KindShell {
			n++
		}
	}
	return n
}

// StartShell starts req as a background shell and records shell.started.
func (h shellHost) StartShell(ctx context.Context, req tools.ShellRequest) (string, error) {
	b := h.b
	l := b.loop
	if l != nil && l.Policy != nil && l.Policy.Mode == policy.ModePlan {
		return "", errors.New("plan mode is read-only; no command starts in the background")
	}
	b.mu.Lock()
	switch {
	case b.closed:
		b.mu.Unlock()
		return "", errors.New("the session is closing; no background command can start")
	case b.liveShellsLocked()+b.shellsReserved >= b.policy.maxShells():
		n, most := b.liveShellsLocked(), b.policy.maxShells()
		b.mu.Unlock()
		return "", fmt.Errorf("background shell limit reached (%d of %d running). Wait for one to end or stop one with shell_kill", n, most)
	}
	b.shellsReserved++
	joined := b.policy.Wake == WakeOff
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.shellsReserved--
		b.mu.Unlock()
	}()

	life := b.policy.lifetime()
	if req.Timeout > 0 && req.Timeout < life {
		life = req.Timeout
	}
	// The session's context, not the call's: only a kill, a stop or the session's end ends it.
	sctx, cancel := context.WithCancelCause(b.ctx)
	sctx, stopDeadline := context.WithDeadlineCause(sctx, b.policy.now().Add(life), ErrBackgroundLifetime)
	var proc *tools.ShellProc
	if req.Proc != nil {
		// Moved from the foreground: the shell's context ends it from now on.
		proc = req.Proc
		go func() {
			select {
			case <-sctx.Done():
				proc.Stop()
			case <-proc.Done():
			}
		}()
	} else {
		cmd, err := req.Build(sctx)
		if err != nil {
			stopDeadline()
			cancel(nil)
			return "", err
		}
		if req.RanUnder != nil {
			req.Tier = req.RanUnder()
		}
		if proc, err = tools.StartShellProc(cmd, func() { cancel(StopCause{TermShutdown}) }, b.policy.ShellOutputCap); err != nil {
			stopDeadline()
			cancel(nil)
			return "", err
		}
	}
	id := shellID()
	sh := &shellState{proc: proc, command: req.Command, callID: h.callID, state: ShellRunning}
	t := &bgTask{ID: id, Kind: KindShell, Description: req.Description, Started: b.policy.now(), joined: joined,
		cancel: cancel, done: make(chan struct{}), shell: sh}
	b.mu.Lock()
	b.tasks[id] = t
	b.order = append(b.order, id)
	b.mu.Unlock()
	if l != nil {
		if _, err := l.Recorder.Record(EvShellStarted, ActorSystem, Trusted, ShellStarted{ShellID: id, CallID: h.callID,
			Command: req.Command, Description: req.Description, Sandbox: req.Tier, Secrets: req.Secrets,
			LifetimeMS: life.Milliseconds(), FromForeground: req.Proc != nil}); err != nil {
			// Not recorded, not run: the record must account for every process.
			cancel(StopCause{TermError})
			<-proc.Done()
			stopDeadline()
			b.mu.Lock()
			t.ended = true
			b.mu.Unlock()
			close(t.done)
			b.forget(id)
			return "", err
		}
	}
	go b.watchShell(sctx, t, stopDeadline)
	return id, nil
}

// shellID is a short id for a background shell, unique within a process.
func shellID() string {
	id := strings.ToLower(newID())
	return "sh_" + id[len(id)-10:]
}

// watchShell waits for a shell to end, records shell.ended and hands over its
// notice the way a background subagent's result is handed over.
func (b *Background) watchShell(sctx context.Context, t *bgTask, stopDeadline func()) {
	defer close(t.done)
	defer stopDeadline()
	sh := t.shell
	<-sh.proc.Done()
	code, _, ran := sh.proc.Exit()
	total, dropped := sh.proc.Size()
	state, reason := ShellExited, ""
	if cause := context.Cause(sctx); sctx.Err() != nil {
		state = ShellKilled
		var sc StopCause
		switch {
		case errors.As(cause, &sc):
			reason = string(sc.Reason)
		case errors.Is(cause, ErrBackgroundLifetime):
			reason = "lifetime"
		default:
			reason = "stopped"
		}
	}
	end := ShellEnded{ShellID: t.ID, CallID: sh.callID, State: state, ExitCode: code, Reason: reason,
		DurationMS: ran.Milliseconds(), OutputBytes: total, Truncated: dropped > 0}
	// Its state first, so a surface told of shell.ended reads the end.
	b.mu.Lock()
	sh.state, sh.exit = state, code
	b.mu.Unlock()
	if b.loop != nil {
		b.loop.record(EvShellEnded, ActorSystem, end)
	}
	n := Notice{TaskID: t.ID, Session: b.sessionID(), Description: t.Description, Kind: KindShell,
		Status: state, Reason: reason, CallID: "bgn_" + newID(), Content: b.redacted(shellEndText(t, end, sh.proc))}
	b.mu.Lock()
	quiet := sh.quiet || b.closed
	b.mu.Unlock()
	var notice *Notice
	if !quiet {
		notice = &n
	}
	b.settleTask(notice, func() { t.ended, t.reason = true, TerminalReason(reason) })
}

// forget removes a shell that never ran.
func (b *Background) forget(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.tasks, id)
	for i, o := range b.order {
		if o == id {
			b.order = append(b.order[:i], b.order[i+1:]...)
			break
		}
	}
}

// shellEndText is what the conversation is told when a shell ends.
func shellEndText(t *bgTask, end ShellEnded, p *tools.ShellProc) string {
	how := fmt.Sprintf("exited with code %d", end.ExitCode)
	if end.State == ShellKilled {
		how = "was killed (" + end.Reason + ")"
	}
	s := fmt.Sprintf("Background shell %s (%s) %s after %s; %d bytes of output, %d not yet read.",
		t.ID, t.Description, how, (time.Duration(end.DurationMS) * time.Millisecond).String(), end.OutputBytes, p.Unread())
	if line := p.LastLine(shellLastLine); line != "" {
		s += "\nLast line: " + line
	}
	return s + "\nRead its output with shell_output."
}

func (b *Background) sessionID() string {
	if b.loop == nil {
		return ""
	}
	return b.loop.sessionID()
}

// partials finds text that may be part of a stored value; secrets.Redactor has it.
type partials interface {
	Pending(s string) int
	Partial(s string) int
}

// redactRead holds back output that may start a secret until the next read;
// at a gap, text a cut secret could leave a part in is skipped. Holds readMu.
func (sh *shellState) redactRead(b *Background, r tools.ShellRead, final bool) (string, int64) {
	var red Redactor
	if b.loop != nil {
		red = b.loop.Recorder.redactor()
	}
	if red == nil || red.Span() == 0 {
		return r.Text, r.Skipped
	}
	text, skipped := r.Text, r.Skipped
	pt, precise := red.(partials)
	if r.Dropped > 0 || r.Skipped > 0 {
		skipped += int64(len(sh.carry))
		sh.carry = ""
		n := red.Span() - 1
		if precise {
			n = pt.Partial(text)
		}
		n = min(n, len(text))
		for n < len(text) && !utf8.RuneStart(text[n]) {
			n++
		}
		text, skipped = text[n:], skipped+int64(n)
	}
	if !precise {
		fb := fragmentBuffer{redact: red.Redact, span: red.Span(), carry: sh.carry}
		out := fb.push(text)
		if final {
			out += fb.flush()
		}
		sh.carry = fb.carry
		return out, skipped
	}
	raw := sh.carry + text
	sh.carry = ""
	if final {
		return redactedText(red.Redact, raw), skipped
	}
	// Cut before a possible secret's start, and never inside a whole one.
	whole := redactedText(red.Redact, raw)
	cut := len(raw) - pt.Pending(raw)
	for ; cut > 0; cut-- {
		if cut < len(raw) && !utf8.RuneStart(raw[cut]) {
			continue
		}
		if head := redactedText(red.Redact, raw[:cut]); cut == len(raw) || head+redactedText(red.Redact, raw[cut:]) == whole {
			sh.carry = raw[cut:]
			return head, skipped
		}
	}
	sh.carry = raw
	return "", skipped
}

// redacted is text as the session's record would keep it.
func (b *Background) redacted(text string) string {
	if b.loop != nil {
		if red := b.loop.Recorder.redactor(); red != nil {
			return redactedText(red.Redact, text)
		}
	}
	return text
}

// shell is one of this session's background shells.
func (b *Background) shell(id string) (*bgTask, bool) {
	if b == nil {
		return nil, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tasks[id]
	if !ok || t.Kind != KindShell {
		return nil, false
	}
	return t, true
}

// KillShell stops one running background shell with reason. quiet leaves out
// the notice, for a kill whose caller reports the end itself.
func (b *Background) KillShell(id string, reason TerminalReason, quiet bool) bool {
	t, ok := b.shell(id)
	if !ok {
		return false
	}
	b.mu.Lock()
	running := !t.ended
	if running && quiet {
		t.shell.quiet = true
	}
	b.mu.Unlock()
	if !running {
		return false
	}
	t.cancel(StopCause{reason})
	waitDone([]*bgTask{t})
	return true
}

// shellInfo fills a shell's fields of TaskInfo. The caller holds b.mu.
func (t *bgTask) shellInfo(ti *TaskInfo) {
	sh := t.shell
	ti.Status = sh.state
	ti.Command = sh.command
	if sh.state != ShellRunning {
		code := sh.exit
		ti.ExitCode = &code
	}
	ti.OutputBytes, _ = sh.proc.Size()
	ti.LastLine = sh.proc.LastLine(shellLastLine)
}

// maxShellRead bounds one shell_output result, as a foreground command's is.
const maxShellRead = 30_000

// maxShellWait bounds how long shell_output waits for a shell to end.
const maxShellWait = 10 * time.Minute

// ShellOutput reads a background shell's new output.
type ShellOutput struct{}

func (ShellOutput) Name() string  { return "shell_output" }
func (ShellOutput) Mutates() bool { return false }
func (ShellOutput) FixedArgs()    {}
func (ShellOutput) Description() string {
	return "Read the output a background shell (started by bash with run_in_background) wrote since your last read, " +
		"with its state: running, or exited with its exit code. Set wait_ms to wait up to that long for it to end first. " +
		"You are told when a shell ends; do not call this in a loop to wait."
}
func (ShellOutput) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"shell_id":{"type":"string","description":"The id bash returned."},` +
		`"wait_ms":{"type":"integer","description":"Wait up to this long for the shell to end before reading. Default 0, max 600000."}` +
		`},"required":["shell_id"]}`)
}

func (ShellOutput) Run(ctx context.Context, _ *tools.Session, raw json.RawMessage) tools.Result {
	var a struct {
		ShellID string `json:"shell_id"`
		WaitMS  int    `json:"wait_ms"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.ShellID == "" {
		return tools.Result{Content: "shell_output needs a shell_id.", IsError: true}
	}
	b, _ := managerOf(ctx)
	t, ok := b.shell(a.ShellID)
	if !ok {
		return tools.Result{Content: fmt.Sprintf("No background shell %q in this session.", a.ShellID), IsError: true}
	}
	if a.WaitMS > 0 {
		wait := min(time.Duration(a.WaitMS)*time.Millisecond, maxShellWait)
		timer := time.NewTimer(wait)
		select {
		case <-t.done:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}
	return shellReadResult(b, t)
}

// shellReadResult is a shell's state and the output it wrote since the last read.
func shellReadResult(b *Background, t *bgTask) tools.Result {
	p := t.shell.proc
	// State before the read: output read as exited is then complete.
	code, ended, ran := p.Exit()
	if ended {
		// Its end is being recorded; the state read below is the one it records.
		select {
		case <-t.done:
		case <-time.After(TurnEndWait):
		}
	}
	t.shell.readMu.Lock()
	r := p.ReadNew(maxShellRead)
	r.Text, r.Skipped = t.shell.redactRead(b, r, ended)
	t.shell.readMu.Unlock()
	var sb strings.Builder
	var exit *int
	b.mu.Lock()
	state := t.shell.state
	b.mu.Unlock()
	switch {
	case !ended:
		fmt.Fprintf(&sb, "shell %s: running · %s", t.ID, ran.Round(time.Millisecond))
	case state == ShellKilled:
		exit = &code
		fmt.Fprintf(&sb, "shell %s: killed, exit %d · %s", t.ID, code, ran.Round(time.Millisecond))
	default:
		exit = &code
		fmt.Fprintf(&sb, "shell %s: exited %d · %s", t.ID, code, ran.Round(time.Millisecond))
	}
	if r.Dropped > 0 {
		fmt.Fprintf(&sb, "\n[... %d bytes of earlier output were dropped: a shell keeps its last %d bytes ...]", r.Dropped, ringCap(b))
	}
	if r.Skipped > 0 {
		fmt.Fprintf(&sb, "\n[... %d bytes not shown: one read returns the last %d ...]", r.Skipped, maxShellRead)
	}
	switch {
	case r.Text != "":
		sb.WriteString("\n" + r.Text)
	case ended:
		sb.WriteString("\n[no new output]")
	default:
		sb.WriteString("\n[no new output yet]")
	}
	if !ended {
		// Models left servers running at the end of the work; say how to stop one.
		fmt.Fprintf(&sb, "\n\nIt is still running. When you no longer need it, stop it with shell_kill (shell_id %s).", t.ID)
	}
	return tools.Result{Content: sb.String(), ExitCode: exit, Truncated: r.Dropped > 0 || r.Skipped > 0}
}

func ringCap(b *Background) int {
	if b.policy.ShellOutputCap > 0 {
		return b.policy.ShellOutputCap
	}
	return tools.ShellOutputCap
}

// ShellKill stops one of this session's background shells.
type ShellKill struct{}

func (ShellKill) Name() string  { return "shell_kill" }
func (ShellKill) Mutates() bool { return false }
func (ShellKill) FixedArgs()    {}
func (ShellKill) Description() string {
	return "Stop a background shell and every process it started, and return the output it wrote since your last read."
}
func (ShellKill) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"shell_id":{"type":"string","description":"The id bash returned."}},"required":["shell_id"]}`)
}

func (ShellKill) Run(ctx context.Context, _ *tools.Session, raw json.RawMessage) tools.Result {
	var a struct {
		ShellID string `json:"shell_id"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.ShellID == "" {
		return tools.Result{Content: "shell_kill needs a shell_id.", IsError: true}
	}
	b, _ := managerOf(ctx)
	t, ok := b.shell(a.ShellID)
	if !ok {
		return tools.Result{Content: fmt.Sprintf("No background shell %q in this session.", a.ShellID), IsError: true}
	}
	b.KillShell(a.ShellID, TermCancelledByParent, true)
	return shellReadResult(b, t)
}
