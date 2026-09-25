package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// The workbench terminal runs in one of two ways. By default each tab is one
// long-lived interactive shell in the session's sandbox (terminal.go says what
// that can and cannot check). In "lines" mode each line is a bash call of its
// own on a pseudo-terminal: the same policy decision, sandbox and record as
// the agent's, with no shell state from one line to the next.

const (
	// ptyRecordBytes bounds what one command's output puts in the record.
	ptyRecordBytes = 64 << 10
	// ptyIdle ends a command nobody has read from, so a closed tab does not
	// leave a process behind.
	ptyIdle = 2 * time.Minute
	ptyMax  = 4 * time.Hour
	// shellMax is a working day: a shell is where a person stays.
	shellMax = 12 * time.Hour
	// ptyLinger is how long a finished command stays readable.
	ptyLinger = time.Minute
	// shellIdle is longer than ptyIdle: a shell outlives a laptop's sleep or a
	// dropped connection, and a reloaded page reattaches to it.
	shellIdle = 30 * time.Minute
	// maxTerminals bounds the shells and commands one session runs at once.
	maxTerminals = 8
	// maxLinesPerInput bounds the lines one write to a shell may enter.
	maxLinesPerInput = 1000
)

var errTooManyLines = errors.New("too many lines in one input")

// shellInfo is what launch needs to know about an interactive shell.
type shellInfo struct {
	capture *lineCapture
	local   bool
	idle    time.Duration
}

// ptyRun is one command, or one interactive shell, on a terminal.
type ptyRun struct {
	id      string
	command string
	cmd     *exec.Cmd
	tty     *os.File
	cancel  context.CancelFunc
	started time.Time
	// capture is set for an interactive shell; inputMu keeps its keys in order.
	capture *lineCapture
	inputMu sync.Mutex
	// local is set when the server holds the shell's own terminal (process
	// and none tiers), so it can ask which process group has the foreground;
	// shellPgrp is the shell's, taken at its first prompt.
	local     bool
	shellPgrp atomic.Int64
	// idle is how long the run may go unwatched.
	idle time.Duration
	// leader names a shell, so what it leaves in its session can be ended safely.
	leader sandbox.Leader
	// endedBy says who ended the run when the server did, for its record.
	endedBy atomic.Pointer[string]

	mu      sync.Mutex
	subs    map[chan []byte]struct{}
	record  []byte // output kept for the record, clipped
	clipped bool
	// pumped closes when the last byte of output has been read.
	pumped   chan struct{}
	done     chan struct{}
	exit     int
	lastRead time.Time
}

type ptyStartRequest struct {
	Command string `json:"command"`
	Cols    uint16 `json:"cols"`
	Rows    uint16 `json:"rows"`
	// Interactive asks for a long-lived shell instead of one command.
	Interactive bool `json:"interactive,omitempty"`
	// Confirmed and Declined answer a Confirm response for the same command.
	Confirmed bool `json:"confirmed,omitempty"`
	Declined  bool `json:"declined,omitempty"`
}

type ptyStartResponse struct {
	ID string `json:"id"`
	// Denied carries the refusal when policy stopped the command before it ran.
	Denied string `json:"denied,omitempty"`
	// Confirm is why the command waits to be confirmed; nothing ran or was recorded.
	Confirm string `json:"confirm,omitempty"`
	Cwd     string `json:"cwd"`
	// Interactive is set when ID is a shell. Lines, when a shell was asked
	// for, says why the terminal judges each line instead.
	Interactive bool   `json:"interactive,omitempty"`
	Lines       string `json:"lines,omitempty"`
	// Isolation and Workspace are what the terminal's banner states.
	Isolation *tools.Isolation `json:"isolation,omitempty"`
	Workspace string           `json:"workspace,omitempty"`
}

// startPTY starts an interactive shell, or judges one command line and, if
// allowed, runs it on a terminal.
func (s *Server) startPTY(w http.ResponseWriter, r *http.Request) {
	live, sess, ok := s.manualSession(w, r)
	if !ok {
		return
	}
	capBody(w, r)
	var req ptyStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (!req.Interactive && strings.TrimSpace(req.Command) == "") {
		WriteError(w, http.StatusBadRequest, "command is required")
		return
	}
	if req.Confirmed && req.Declined {
		WriteError(w, http.StatusBadRequest, "a command cannot be both confirmed and declined")
		return
	}
	if len(req.Command) > maxManualCommand {
		WriteError(w, http.StatusBadRequest, "command is too long")
		return
	}
	// A shell does not share the person's working directory, so it does not
	// wait on manualMu, which an explorer operation may hold for minutes.
	if req.Interactive {
		live.shellMu.Lock()
		defer live.shellMu.Unlock()
		if !terminalsFull(w, live) {
			s.startShell(w, live, sess, req)
		}
		return
	}
	live.manualMu.Lock()
	defer live.manualMu.Unlock()
	if terminalsFull(w, live) {
		return
	}

	id := "u" + newSessionID()
	args, _ := json.Marshal(map[string]string{"command": req.Command, "description": "typed into the workbench terminal"})
	answer := agent.Unanswered
	switch {
	case req.Confirmed:
		answer = agent.Confirmed
	case req.Declined:
		answer = agent.Declined
	}
	tool, refused, confirm, err := live.Loop.ManualAuthorizeTyped(id, args, answer)
	if errors.Is(err, agent.ErrNothingToDecline) {
		WriteJSON(w, http.StatusOK, ptyStartResponse{Denied: "Not run: declined", Cwd: sess.Rel(sess.Cwd)})
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "the command could not be recorded")
		return
	}
	if confirm != "" {
		WriteJSON(w, http.StatusOK, ptyStartResponse{Confirm: confirm, Cwd: sess.Rel(sess.Cwd)})
		return
	}
	if refused != nil {
		_ = live.Loop.ManualObserve(id, "bash", *refused, 0)
		WriteJSON(w, http.StatusOK, ptyStartResponse{ID: id, Denied: refused.Content, Cwd: sess.Rel(sess.Cwd)})
		return
	}

	// A bare cd never needs a terminal, and this is where the directory follows.
	if isPlainCd(req.Command) {
		sess.FollowCd(req.Command)
		res := tools.Result{Content: sess.Rel(sess.Cwd)}
		_ = live.Loop.ManualObserve(id, "bash", res, 0)
		WriteJSON(w, http.StatusOK, ptyStartResponse{ID: id, Cwd: sess.Rel(sess.Cwd)})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), ptyMax)
	var cmd *exec.Cmd
	if b, ok := tool.(tools.Bash); ok && b.Sandbox != nil {
		cmd = b.Sandbox(ctx, sess.Cwd, req.Command)
	} else {
		cmd = exec.CommandContext(ctx, "bash", "-c", req.Command)
		cmd.Dir = sess.Cwd
		cmd.Env = append(os.Environ(), "ABHED_SESSION=1")
	}
	cmd.Env = withTerm(cmd.Env)
	run, err := s.launch(live, sess, id, req.Command, cmd, cancel, req.Cols, req.Rows, nil)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, ptyStartResponse{ID: run.id, Cwd: sess.Rel(sess.Cwd)})
}

// terminalsFull refuses another terminal when the session runs maxTerminals.
func terminalsFull(w http.ResponseWriter, live *liveSession) bool {
	live.mu.Lock()
	if live.ptys == nil {
		live.ptys = map[string]*ptyRun{}
	}
	running := 0
	for _, r := range live.ptys {
		select {
		case <-r.done:
		default:
			running++
		}
	}
	live.mu.Unlock()
	if running >= maxTerminals {
		WriteError(w, http.StatusTooManyRequests, "eight terminals or commands are already running in this session")
		return true
	}
	return false
}

// withTerm names the terminal. A nil Env means the host's environment, which
// appending to directly would have replaced with TERM alone.
func withTerm(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	return append(env, "TERM=xterm-256color")
}

// launch starts cmd on a new terminal and follows it to the end.
func (s *Server) launch(live *liveSession, sess *tools.Session, id, command string, cmd *exec.Cmd,
	cancel context.CancelFunc, cols, rows uint16, shell *shellInfo) (*ptyRun, error) {
	size := &pty.Winsize{Cols: cols, Rows: rows}
	if size.Cols == 0 || size.Rows == 0 {
		size = &pty.Winsize{Cols: 100, Rows: 30}
	}
	tty, err := pty.StartWithSize(cmd, size)
	if err != nil {
		cancel()
		res := tools.Result{Content: "Failed to run command: " + err.Error(), IsError: true}
		_ = live.Loop.ManualObserve(id, "bash", res, 0)
		return nil, errors.New(res.Content)
	}
	run := &ptyRun{id: id, command: command, cmd: cmd, tty: tty, cancel: cancel, started: time.Now(), idle: ptyIdle,
		subs: map[chan []byte]struct{}{}, pumped: make(chan struct{}), done: make(chan struct{}), lastRead: time.Now()}
	if shell != nil {
		run.capture, run.local, run.idle = shell.capture, shell.local, shell.idle
		run.leader = sandbox.Lead(cmd) // named now, while its pid is certainly its own
	}
	live.mu.Lock()
	live.ptys[id] = run
	live.mu.Unlock()
	go run.pump()
	go s.finishPTY(live, sess, run)
	return run, nil
}

// isolationOf says what contains the terminal, for its banner.
func isolationOf(t tools.Tool) *tools.Isolation {
	b, ok := t.(tools.Bash)
	if !ok {
		return nil
	}
	in := b.Isolation
	if b.Sandbox == nil {
		in = tools.Isolation{Tier: "none", Backend: "host", Network: true}
	}
	return &in
}

// startShell starts one interactive shell in the session's sandbox, at the
// workspace root. Starting it is the person's bash call: judged, and recorded
// with the shell's exit and the tail of its output when it ends.
func (s *Server) startShell(w http.ResponseWriter, live *liveSession, sess *tools.Session, req ptyStartRequest) {
	resp := ptyStartResponse{Cwd: sess.Rel(sess.Root), Workspace: sess.Root}
	tool, _ := live.Loop.Tools.Get("bash")
	resp.Isolation = isolationOf(tool)
	b, _ := tool.(tools.Bash)
	_, _, cfg := s.state.snapshot()
	switch pol := live.Loop.Policy; {
	case cfg.Sandbox.Terminal == "lines":
		resp.Lines = "this server's terminal judges each line before it runs (sandbox.terminal is \"lines\")"
	case b.Shell == nil:
		resp.Lines = "this sandbox cannot host an interactive shell, so each line runs as its own command"
	case pol.Managed && pol.Screens("bash"):
		// A managed policy is the organisation's word that its rules hold.
		resp.Lines = "your organisation's policy has deny rules or hooks that could refuse a command, which only a line-by-line terminal applies to every command"
	}
	if resp.Lines != "" {
		WriteJSON(w, http.StatusOK, resp)
		return
	}

	id := "u" + newSessionID()
	args, _ := json.Marshal(map[string]any{"command": "bash -i", "description": "interactive terminal in the session sandbox", "interactive": true})
	if _, refused, err := live.Loop.ManualAuthorize("bash", id, args); err != nil {
		WriteError(w, http.StatusInternalServerError, "the terminal could not be recorded, so it was not opened")
		return
	} else if refused != nil {
		_ = live.Loop.ManualObserve(id, "bash", *refused, 0)
		resp.ID, resp.Denied = id, refused.Content
		WriteJSON(w, http.StatusOK, resp)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), shellMax)
	cmd := b.Shell(ctx, sess.Root)
	cmd.Env = withTerm(cmd.Env)
	shell := &shellInfo{
		capture: newLineCapture(id, func(in agent.TerminalInput) { _ = live.Loop.ManualTerminalInput(in) }),
		local:   resp.Isolation != nil && (resp.Isolation.Tier == "process" || resp.Isolation.Tier == "none"),
		idle:    shellIdle,
	}
	if cfg.Sandbox.TerminalIdleMinutes > 0 {
		shell.idle = time.Duration(cfg.Sandbox.TerminalIdleMinutes) * time.Minute
	}
	run, err := s.launch(live, sess, id, "bash -i", cmd, cancel, req.Cols, req.Rows, shell)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp.ID, resp.Interactive = run.id, true
	WriteJSON(w, http.StatusOK, resp)
}

// isPlainCd reports a command that only changes directory.
func isPlainCd(command string) bool {
	c := strings.TrimSpace(command)
	return c == "cd" || strings.HasPrefix(c, "cd ") && !strings.ContainsAny(c, "&|;`$(")
}

// pump copies terminal output to every reader and to the record.
func (p *ptyRun) pump() {
	defer close(p.pumped)
	buf := make([]byte, 8<<10)
	for {
		n, err := p.tty.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if p.capture != nil {
				p.capture.output(chunk)
				// The first output is the shell's prompt, and the foreground
				// group then is the shell's own.
				if p.local && p.shellPgrp.Load() == 0 {
					if fg, _, ok := ttyNow(p.tty); ok {
						p.shellPgrp.Store(int64(fg))
					}
				}
			}
			p.mu.Lock()
			switch {
			case p.capture != nil:
				// A shell's record keeps its latest output, which is also what
				// a reader that reconnects is shown.
				p.record = append(p.record, chunk...)
				if len(p.record) > ptyRecordBytes {
					p.record, p.clipped = keepTail(p.record, ptyRecordBytes), true
				}
			case len(p.record) < ptyRecordBytes:
				p.record = append(p.record, chunk...)
				if len(p.record) > ptyRecordBytes {
					p.record, p.clipped = p.record[:ptyRecordBytes], true
				}
			default:
				p.clipped = true
			}
			for ch := range p.subs {
				select {
				case ch <- chunk:
				default: // a reader that has fallen behind drops output, not the run
				}
			}
			p.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// noteSweep tells the operator when a shell's session was not swept, so that
// containment not running is never silent.
func (s *Server) noteSweep(live *liveSession, run *ptyRun, refused string) {
	if refused != "" {
		s.log.Warn("the shell's session was not swept; what it left running may still run",
			"session", live.ID, "terminal", run.id, "reason", refused)
	}
}

// finishPTY waits for the command, records its output and clears it away.
func (s *Server) finishPTY(live *liveSession, sess *tools.Session, run *ptyRun) {
	idle := time.NewTicker(15 * time.Second)
	defer idle.Stop()
	waited := make(chan error, 1)
	go func() {
		if run.capture != nil {
			// A shell takes what it left running in its session with it.
			refused, err := run.leader.Wait(run.cmd)
			s.noteSweep(live, run, refused)
			waited <- err
			return
		}
		waited <- run.cmd.Wait()
	}()
	var err error
loop:
	for {
		select {
		case err = <-waited:
			break loop
		case <-idle.C:
			run.mu.Lock()
			abandoned := len(run.subs) == 0 && time.Since(run.lastRead) > run.idle
			run.mu.Unlock()
			if abandoned {
				run.stop("ended after going unwatched")
			}
		}
	}
	run.cancel()
	// The last output can still be in flight after the process has gone.
	// Give the pump a moment to read it before the terminal is closed, then
	// a moment more for the read to return.
	select {
	case <-run.pumped:
	case <-time.After(500 * time.Millisecond):
	}
	_ = run.tty.Close()
	select {
	case <-run.pumped:
	case <-time.After(500 * time.Millisecond):
	}

	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = tools.ExitStatus(ee)
	} else if err != nil {
		code = -1
	}
	run.mu.Lock()
	run.exit = code
	text := plainText(run.record)
	clipped := run.clipped
	run.mu.Unlock()

	// The record is written before readers are told the command ended, so
	// "exit" on the stream means the observation is already there.
	how := "on a terminal"
	if run.capture != nil {
		run.capture.flush()
		how = "interactive terminal"
		if by := run.endedBy.Load(); by != nil {
			how += ", " + *by
		}
		how += "; the latest output follows"
	} else {
		live.manualMu.Lock()
		sess.FollowCd(run.command)
		live.manualMu.Unlock()
	}
	// A shell's end is the person's doing, whatever its status; only a
	// failure to start one is an error.
	res := tools.Result{Content: fmt.Sprintf("exit %d · %s\n%s", code, how, text), ExitCode: &code,
		IsError: code != 0 && run.capture == nil, Truncated: clipped}
	_ = live.Loop.ManualObserve(run.id, "bash", res, time.Since(run.started))
	close(run.done)

	// A short command can end before its reader connects. The run stays
	// findable for a minute so a late reader still gets the output and the exit.
	time.AfterFunc(ptyLinger, func() {
		live.mu.Lock()
		delete(live.ptys, run.id)
		live.mu.Unlock()
	})
}

// ansiSeq matches CSI, OSC and DCS sequences, charset selections, the
// single-character escapes, and the control bytes that only move a cursor.
var ansiSeq = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]|\x1b[P\\]^_][^\x1b\x07]*(\x07|\x1b\\\\)|\x1b[()*+][A-Za-z0-9]|\x1b[=>78cMDEH]|[\r\x00-\x08\x0b-\x0c\x0e-\x1a\x1c-\x1f]")

// plainText strips terminal control sequences so the record reads as text.
func plainText(b []byte) string {
	return string(ansiSeq.ReplaceAll(b, nil))
}

// ptyFor finds one of the caller's running commands. A terminal lives in this
// process, so a session that is not live here has none to find.
func (s *Server) ptyFor(w http.ResponseWriter, r *http.Request) (*liveSession, *ptyRun, bool) {
	id := r.PathValue("id")
	live, found := s.session(id, TenantOf(r.Context()), UserOf(r.Context()))
	if !validSessionID(id) || !found {
		WriteError(w, http.StatusNotFound, "that command is not running")
		return nil, nil, false
	}
	live.mu.Lock()
	run := live.ptys[r.PathValue("pty")]
	live.mu.Unlock()
	if run == nil {
		WriteError(w, http.StatusNotFound, "that command is not running")
		return nil, nil, false
	}
	return live, run, true
}

// say shows a line from the server on the terminal, outside the record.
func (p *ptyRun) say(text string) {
	msg := []byte("\r\n\x1b[31m" + strings.ReplaceAll(text, "\n", "\r\n") + "\x1b[0m\r\n")
	p.mu.Lock()
	defer p.mu.Unlock()
	for ch := range p.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

// streamPTY sends a command's output as it happens, then its exit code.
func (s *Server) streamPTY(w http.ResponseWriter, r *http.Request) {
	_, run, ok := s.ptyFor(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan []byte, 256)
	run.mu.Lock()
	// What was written before this reader arrived comes first.
	backlog := append([]byte(nil), run.record...)
	run.subs[ch] = struct{}{}
	run.lastRead = time.Now()
	run.mu.Unlock()
	defer func() {
		run.mu.Lock()
		delete(run.subs, ch)
		run.lastRead = time.Now()
		run.mu.Unlock()
	}()

	send := func(event string, data []byte) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}
	if len(backlog) > 0 {
		send("out", []byte(base64.StdEncoding.EncodeToString(backlog)))
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case chunk := <-ch:
			send("out", []byte(base64.StdEncoding.EncodeToString(chunk)))
		case <-run.done:
			// Drain what arrived between the last read and the exit.
			for {
				select {
				case chunk := <-ch:
					send("out", []byte(base64.StdEncoding.EncodeToString(chunk)))
					continue
				default:
				}
				break
			}
			run.mu.Lock()
			code := run.exit
			run.mu.Unlock()
			send("exit", []byte(fmt.Sprint(code)))
			return
		}
	}
}

// writePTY forwards keystrokes to the command.
func (s *Server) writePTY(w http.ResponseWriter, r *http.Request) {
	live, run, ok := s.ptyFor(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		WriteError(w, http.StatusBadRequest, "input is too large")
		return
	}
	select {
	case <-run.done:
		WriteError(w, http.StatusGone, "the command has ended")
		return
	default:
	}
	// A single command's input is not recorded: on a terminal it echoes into
	// the output unless the program turned echo off, which is exactly when it
	// should not. A shell's lines are, by the capture.
	if run.capture == nil {
		if _, err := run.tty.Write(data); err != nil {
			WriteError(w, http.StatusGone, "the command has ended")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.shellInput(live, run, data); errors.Is(err, errTooManyLines) {
		WriteError(w, http.StatusRequestEntityTooLarge, "more than a thousand lines at once; paste them in parts")
		return
	} else if err != nil {
		WriteError(w, http.StatusGone, "the terminal has ended")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// shellInput forwards keys to a shell. Each line is put to the deny rules at
// its Enter, as typed; a refused line never reaches the shell, which is sent
// Ctrl-C instead to discard it.
func (s *Server) shellInput(live *liveSession, run *ptyRun, data []byte) error {
	run.inputMu.Lock()
	defer run.inputMu.Unlock()
	keys := run.capture.keys(data)
	if len(keys) > maxLinesPerInput {
		return errTooManyLines
	}
	// Keys a program reads are not the start of the shell's next line.
	defer func() {
		if run.programHasTerminal() {
			run.capture.abandon()
		}
	}()
	for _, k := range keys {
		e := k.enter
		if e != nil {
			run.ask(e)
		}
		if e == nil || e.program || e.line == "" {
			if _, err := run.tty.Write(k.data); err != nil {
				return err
			}
			if e != nil {
				run.capture.entered(e)
			}
			continue
		}
		refused, err := live.Loop.ManualScreen("u"+newSessionID(), e.line)
		if err != nil {
			refused = &tools.Result{Content: "Denied: the line could not be recorded"}
		}
		if refused == nil {
			if _, err := run.tty.Write(k.data); err != nil {
				return err
			}
			run.capture.entered(e)
			continue
		}
		run.say(refused.Content)
		// A line pasted whole has not reached the shell at all, and an empty
		// line brings its prompt back. One typed earlier is in its buffer.
		discard := []byte{'\r'}
		if !e.whole {
			discard = []byte{0x03}
		}
		if _, err := run.tty.Write(discard); err != nil {
			return err
		}
	}
	return nil
}

// ask fills in what the terminal says at a line's Enter. On the process and
// none tiers the server holds the shell's own terminal and can ask it; on a
// container's, the engine's CLI holds it raw, and the alternate screen is the
// only sign of a full-screen program.
func (p *ptyRun) ask(e *enteredLine) {
	e.program = e.alt
	if !p.local {
		return
	}
	e.program = false
	fg, canonical, ok := ttyNow(p.tty)
	if !ok {
		return
	}
	// A line read in canonical mode is not one typed at bash's prompt: a
	// password prompt, or keys typed ahead while a builtin ran. Its text is
	// withheld.
	e.known, e.secret, e.program = true, canonical, p.isProgram(fg)
}

// isProgram reports whether fg, the foreground process group, is not the shell's.
func (p *ptyRun) isProgram(fg int) bool {
	shell := int(p.shellPgrp.Load())
	return shell != 0 && fg != shell
}

// programHasTerminal reports, where the terminal can be asked, whether a
// program other than the shell is reading the keys.
func (p *ptyRun) programHasTerminal() bool {
	if !p.local {
		return false
	}
	fg, _, ok := ttyNow(p.tty)
	return ok && p.isProgram(fg)
}

// resizePTY tells the command its terminal changed size.
func (s *Server) resizePTY(w http.ResponseWriter, r *http.Request) {
	_, run, ok := s.ptyFor(w, r)
	if !ok {
		return
	}
	capBody(w, r)
	var size struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&size); err != nil || size.Cols == 0 || size.Rows == 0 {
		WriteError(w, http.StatusBadRequest, "cols and rows are required")
		return
	}
	_ = pty.Setsize(run.tty, &pty.Winsize{Cols: size.Cols, Rows: size.Rows})
	w.WriteHeader(http.StatusNoContent)
}

// killPTY ends the command.
func (s *Server) killPTY(w http.ResponseWriter, r *http.Request) {
	_, run, ok := s.ptyFor(w, r)
	if !ok {
		return
	}
	run.stop("closed from the workbench")
	w.WriteHeader(http.StatusNoContent)
}

// stop ends the run, noting why for its record unless a reason is already set.
func (p *ptyRun) stop(why string) {
	p.endedBy.CompareAndSwap(nil, &why)
	p.cancel()
}

// closeTerminals ends every shell and command the session has on a terminal.
func (l *liveSession) closeTerminals() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.ptys {
		r.stop("closed with the session")
	}
}
