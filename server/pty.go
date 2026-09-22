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
	"time"

	"github.com/creack/pty"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// The workbench terminal runs one command line at a time on a pseudo-terminal,
// so vim, top and a prompt that waits for an answer all work. Each line is a
// bash call: the same policy decision, the same sandbox and the same record as
// the agent's, which is what a live shell could not give — a shell has no
// call to judge until it is too late.

const (
	// ptyRecordBytes bounds what one command's output puts in the record.
	ptyRecordBytes = 64 << 10
	// ptyIdle ends a command nobody has read from, so a closed tab does not
	// leave a process behind.
	ptyIdle = 2 * time.Minute
	ptyMax  = 4 * time.Hour
	// ptyLinger is how long a finished command stays readable.
	ptyLinger = time.Minute
)

// ptyRun is one command on a terminal.
type ptyRun struct {
	id      string
	command string
	cmd     *exec.Cmd
	tty     *os.File
	cancel  context.CancelFunc
	started time.Time

	mu       sync.Mutex
	subs     map[chan []byte]struct{}
	record   []byte // output kept for the record, clipped
	clipped  bool
	done     chan struct{}
	exit     int
	lastRead time.Time
}

type ptyStartRequest struct {
	Command string `json:"command"`
	Cols    uint16 `json:"cols"`
	Rows    uint16 `json:"rows"`
}

type ptyStartResponse struct {
	ID string `json:"id"`
	// Denied carries the refusal when policy stopped the command before it ran.
	Denied string `json:"denied,omitempty"`
	Cwd    string `json:"cwd"`
}

// startPTY judges one command line and, if allowed, runs it on a terminal.
func (s *Server) startPTY(w http.ResponseWriter, r *http.Request) {
	live, sess, ok := s.manualSession(w, r)
	if !ok {
		return
	}
	capBody(w, r)
	var req ptyStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Command) == "" {
		WriteError(w, http.StatusBadRequest, "command is required")
		return
	}
	if len(req.Command) > maxManualCommand {
		WriteError(w, http.StatusBadRequest, "command is too long")
		return
	}
	live.manualMu.Lock()
	defer live.manualMu.Unlock()
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
	if running >= 4 {
		WriteError(w, http.StatusTooManyRequests, "four commands are already running in this session's terminal")
		return
	}

	id := "u" + newSessionID()
	args, _ := json.Marshal(map[string]string{"command": req.Command, "description": "typed into the workbench terminal"})
	tool, refused, err := live.Loop.ManualAuthorize("bash", id, args)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "the command could not be recorded")
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
	cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	size := &pty.Winsize{Cols: req.Cols, Rows: req.Rows}
	if size.Cols == 0 || size.Rows == 0 {
		size = &pty.Winsize{Cols: 100, Rows: 30}
	}
	tty, err := pty.StartWithSize(cmd, size)
	if err != nil {
		cancel()
		res := tools.Result{Content: "Failed to run command: " + err.Error(), IsError: true}
		_ = live.Loop.ManualObserve(id, "bash", res, 0)
		WriteError(w, http.StatusInternalServerError, res.Content)
		return
	}
	run := &ptyRun{id: id, command: req.Command, cmd: cmd, tty: tty, cancel: cancel, started: time.Now(),
		subs: map[chan []byte]struct{}{}, done: make(chan struct{}), lastRead: time.Now()}
	live.mu.Lock()
	live.ptys[id] = run
	live.mu.Unlock()
	go run.pump()
	go s.finishPTY(live, sess, run)
	WriteJSON(w, http.StatusOK, ptyStartResponse{ID: id, Cwd: sess.Rel(sess.Cwd)})
}

// isPlainCd reports a command that only changes directory.
func isPlainCd(command string) bool {
	c := strings.TrimSpace(command)
	return c == "cd" || strings.HasPrefix(c, "cd ") && !strings.ContainsAny(c, "&|;`$(")
}

// pump copies terminal output to every reader and to the record.
func (p *ptyRun) pump() {
	buf := make([]byte, 8<<10)
	for {
		n, err := p.tty.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			p.mu.Lock()
			if len(p.record) < ptyRecordBytes {
				p.record = append(p.record, chunk...)
				if len(p.record) > ptyRecordBytes {
					p.record, p.clipped = p.record[:ptyRecordBytes], true
				}
			} else {
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

// finishPTY waits for the command, records its output and clears it away.
func (s *Server) finishPTY(live *liveSession, sess *tools.Session, run *ptyRun) {
	idle := time.NewTicker(15 * time.Second)
	defer idle.Stop()
	waited := make(chan error, 1)
	go func() { waited <- run.cmd.Wait() }()
	var err error
loop:
	for {
		select {
		case err = <-waited:
			break loop
		case <-idle.C:
			run.mu.Lock()
			abandoned := len(run.subs) == 0 && time.Since(run.lastRead) > ptyIdle
			run.mu.Unlock()
			if abandoned {
				run.cancel()
			}
		}
	}
	run.cancel()
	_ = run.tty.Close()

	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	run.mu.Lock()
	run.exit = code
	close(run.done)
	text := plainText(run.record)
	clipped := run.clipped
	run.mu.Unlock()

	live.manualMu.Lock()
	sess.FollowCd(run.command)
	live.manualMu.Unlock()
	res := tools.Result{Content: fmt.Sprintf("exit %d · on a terminal\n%s", code, text), ExitCode: &code,
		IsError: code != 0, Truncated: clipped}
	_ = live.Loop.ManualObserve(run.id, "bash", res, time.Since(run.started))

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

// ptyFor finds one of the caller's running commands.
func (s *Server) ptyFor(w http.ResponseWriter, r *http.Request) (*ptyRun, bool) {
	live, _, ok := s.manualSession(w, r)
	if !ok {
		return nil, false
	}
	live.mu.Lock()
	run := live.ptys[r.PathValue("pty")]
	live.mu.Unlock()
	if run == nil {
		WriteError(w, http.StatusNotFound, "that command is not running")
		return nil, false
	}
	return run, true
}

// streamPTY sends a command's output as it happens, then its exit code.
func (s *Server) streamPTY(w http.ResponseWriter, r *http.Request) {
	run, ok := s.ptyFor(w, r)
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
	run, ok := s.ptyFor(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		WriteError(w, http.StatusBadRequest, "input is too large")
		return
	}
	// Typed input is not recorded: on a terminal it echoes into the output
	// unless the program turned echo off, which is exactly when it should not.
	select {
	case <-run.done:
		WriteError(w, http.StatusGone, "the command has ended")
		return
	default:
	}
	if _, err := run.tty.Write(data); err != nil {
		WriteError(w, http.StatusGone, "the command has ended")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resizePTY tells the command its terminal changed size.
func (s *Server) resizePTY(w http.ResponseWriter, r *http.Request) {
	run, ok := s.ptyFor(w, r)
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
	run, ok := s.ptyFor(w, r)
	if !ok {
		return
	}
	run.cancel()
	w.WriteHeader(http.StatusNoContent)
}
