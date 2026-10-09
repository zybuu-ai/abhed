package server

import (
	"context"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/termline"
)

// A shell attached to a sandbox, over :attach or SSH, is followed the way
// the workbench follows its interactive terminal on the container tier,
// where the terminal itself cannot be asked: each line a person enters is
// rebuilt from the keys, put to the deny rules at its Enter, and recorded,
// without its text when the terminal did not show it. A refused line never
// reaches the shell. This is best effort, as the guide says; the sandbox is
// the boundary.

// terminalRecordBytes bounds the output a terminal's end puts in the record.
const terminalRecordBytes = 8 << 10

// sandboxTerminal is one attached shell.
type sandboxTerminal struct {
	s       *Server
	ctx     context.Context
	caller  controlplane.Caller
	sb      controlplane.Sandbox
	pol     *policy.Engine
	id      string
	capture *termline.Capture
	prompt  *termline.Prompt
	// shell takes the keys allowed through; client gets the output.
	shell  io.Writer
	client io.Writer

	inputMu sync.Mutex
	mu      sync.Mutex
	tail    []byte
	clipped bool
	in, out int64
}

func (s *Server) newSandboxTerminal(ctx context.Context, c controlplane.Caller, sb controlplane.Sandbox, pol *policy.Engine,
	client io.Writer) *sandboxTerminal {
	t := &sandboxTerminal{s: s, ctx: ctx, caller: c, sb: sb, pol: pol, id: "t" + newSessionID(), client: client,
		prompt: termline.NewPrompt()}
	t.capture = termline.NewCapture(t.id, t.recordLine)
	return t
}

// recordLine records a line the capture judged.
func (t *sandboxTerminal) recordLine(in agent.TerminalInput) {
	detail := map[string]string{"terminal": t.id}
	if in.Withheld != "" {
		detail["withheld"] = in.Withheld
	} else {
		detail["line"] = in.Line
	}
	if in.Edited {
		detail["edited"] = "true"
	}
	if err := t.s.opts.ControlPlane.Note(context.WithoutCancel(t.ctx), t.caller, t.sb, "attach.input", "", detail, "line"); err != nil {
		t.s.log.Warn("a line typed into a sandbox's terminal could not be recorded", "sandbox", t.sb.ID, "terminal", t.id, "error", err)
	}
}

// Write takes the shell's output: followed for the line capture and the
// prompt, kept for the record, and sent to the client.
func (t *sandboxTerminal) Write(p []byte) (int, error) {
	t.capture.Output(p)
	t.prompt.OutputUnasked(p)
	t.mu.Lock()
	t.out += int64(len(p))
	t.tail = append(t.tail, p...)
	if len(t.tail) > 2*terminalRecordBytes {
		t.tail, t.clipped = termline.KeepTail(t.tail, terminalRecordBytes), true
	}
	t.mu.Unlock()
	return t.client.Write(p)
}

// say shows a line from the server on the terminal, outside the record.
func (t *sandboxTerminal) say(text string) {
	_, _ = t.client.Write([]byte("\r\n\x1b[31m" + strings.ReplaceAll(text, "\n", "\r\n") + "\x1b[0m\r\n"))
}

// input forwards keys to the shell, judging each line at its Enter as the
// workbench's shellInput does where the terminal cannot be asked.
func (t *sandboxTerminal) input(data []byte) error {
	t.inputMu.Lock()
	defer t.inputMu.Unlock()
	t.mu.Lock()
	t.in += int64(len(data))
	t.mu.Unlock()
	keys := t.capture.Keys(data)
	if len(keys) > maxLinesPerInput {
		t.say("abhed: more than a thousand lines at once were not sent; paste them in parts")
		return nil
	}
	write := func(b []byte) func() error {
		return func() error { _, err := t.shell.Write(b); return err }
	}
	for _, k := range keys {
		e := k.Enter
		if e != nil {
			// Where the terminal cannot be asked, the alternate screen is the
			// only sign of a full-screen program, and a line before the
			// prompt is back counts as typed ahead.
			e.Program = e.Alt
			e.Ahead = !t.prompt.At()
		}
		if e == nil || e.Program || e.Line == "" {
			if e != nil && !e.Program {
				t.prompt.Gave()
			}
			if err := t.capture.Send(e, write(k.Data)); err != nil {
				return err
			}
			continue
		}
		why := denied(t.pol, e.Line, true)
		if why == nil {
			t.prompt.Gave()
			if err := t.capture.Send(e, write(k.Data)); err != nil {
				return err
			}
			continue
		}
		t.refused(e.Line, why)
		// A line pasted whole has not reached the shell at all, and an empty
		// line brings its prompt back. One typed earlier is in its buffer.
		discard := []byte{'\r'}
		if !e.Whole {
			discard = []byte{0x03}
		}
		t.prompt.Gave()
		if _, err := t.shell.Write(discard); err != nil {
			return err
		}
	}
	return nil
}

// refused records and shows a line the deny rules refused. One that cannot
// be recorded is refused all the same.
func (t *sandboxTerminal) refused(line string, why error) {
	detail := map[string]string{"terminal": t.id, "line": line, "reason": why.Error()}
	if err := t.s.opts.ControlPlane.Note(context.WithoutCancel(t.ctx), t.caller, t.sb, "attach.refused", controlplane.OutcomeFailed,
		detail, "line"); err != nil {
		t.s.log.Warn("a refused line could not be recorded", "sandbox", t.sb.ID, "terminal", t.id, "error", err)
	}
	t.say("Denied: " + strings.TrimPrefix(why.Error(), controlplane.ErrForbidden.Error()+": "))
}

// run attaches to the sandbox's shell until it ends, taking the client's
// keys from keys (nil at its end) and its window changes from resize, and
// returns what the record keeps of it.
func (t *sandboxTerminal) run(ctx context.Context, d driver.Driver, cwd string, size driver.WinSize,
	keys <-chan []byte, resize <-chan driver.WinSize) (map[string]string, error) {
	start := time.Now()
	pr, pw := io.Pipe()
	t.shell = pw
	go func() {
		defer pw.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case k, ok := <-keys:
				if !ok {
					return
				}
				if t.input(k) != nil {
					return
				}
			}
		}
	}()
	res, err := driver.Attach(ctx, d, t.sb.ID, driver.AttachRequest{Cwd: cwd, Size: size, Stdin: pr, Stdout: t, Resize: resize})
	_ = pr.Close()
	// Every line still waiting is judged before the output is taken, so a
	// withheld line's echo is scrubbed from it.
	t.capture.Flush()
	t.mu.Lock()
	text := termline.PlainText(termline.KeepTail(t.tail, terminalRecordBytes))
	clipped, in, out := t.clipped || len(t.tail) > terminalRecordBytes, t.in, t.out
	t.mu.Unlock()
	text = t.s.opts.ControlPlane.Redact(t.capture.Scrub(text))
	detail := map[string]string{"terminal": t.id, "bytes_in": strconv.FormatInt(in, 10), "bytes_out": strconv.FormatInt(out, 10),
		"duration_ms": strconv.FormatInt(time.Since(start).Milliseconds(), 10), "output": text}
	if clipped {
		detail["output_clipped"] = "true"
	}
	if err == nil {
		detail["exit_code"] = strconv.Itoa(res.ExitCode)
	}
	return detail, err
}
