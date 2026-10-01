package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/embedded"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/termline"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// The interactive Abhed terminal (§7.1): one shell, run whole under the
// session's sandbox. Opening it is the person's bash call, judged and
// recorded; each line is put to the deny rules at its Enter and recorded
// as terminal.input by the person, withheld when the terminal did not show
// it, as the workbench's shell is (internal/termline).

const (
	// shellLife bounds an interactive terminal: a working day.
	shellLife = 12 * time.Hour
	// shellTail is the latest output an interactive terminal's record keeps.
	shellTail = 64 << 10
	// shellLinesPerInput bounds the lines one input may enter.
	shellLinesPerInput = 1000
)

// acpShell is the shell behind an interactive terminal.
type acpShell struct {
	id      string // the call that opened it
	cmd     *exec.Cmd
	tty     *os.File
	tier    string
	started time.Time
	capture *termline.Capture
	// local is set when this process holds the shell's own terminal (process
	// and none tiers), so it can ask which process group has the foreground.
	local     bool
	shellPgrp atomic.Int64
	leader    sandbox.Leader
	inputMu   sync.Mutex
	mu        sync.Mutex
	tail      []byte
	pumped    chan struct{}
	// prompt follows whether the shell is back at its prompt.
	prompt *termline.Prompt
}

// startShell opens the shell, refusing where a line-by-line terminal is the
// only one every rule holds for, as the workbench does.
func (c *acpConn) startShell(t *acpTerminal, parts embedded.Parts, bash tools.Tool, iso tools.Isolation, cols, rows uint16) *rpcError {
	b, _ := bash.(tools.Bash)
	pol := parts.Loop.Policy
	switch {
	case parts.Config.Sandbox.Terminal == "lines":
		return refusal(errPolicy, `this configuration's terminal judges each line before it runs (sandbox.terminal is "lines"); open the terminal in lines mode`)
	case b.Shell == nil:
		return refusal(errRefused, "this sandbox cannot host an interactive shell; open the terminal in lines mode")
	case pol.Managed && pol.Screens("bash"):
		// A managed policy is the organisation's word that its rules hold.
		return refusal(errPolicy, "your organisation's policy has deny rules or hooks that only a line-by-line terminal applies to every command; open the terminal in lines mode")
	}
	loop := parts.Loop
	id := "u" + acpID()
	args, _ := json.Marshal(map[string]any{"command": "bash -i", "description": "interactive Abhed terminal in the session sandbox", "interactive": true})
	_, refused, err := loop.ManualAuthorize("bash", id, args)
	if err != nil {
		return refusal(errRecord, "the terminal could not be recorded, so it was not opened: %v", err)
	}
	if refused != nil {
		_ = loop.ManualObserve(id, "bash", *refused, 0)
		return refusal(errPolicy, "%s", refused.Content)
	}
	ctx, cancel := context.WithTimeout(t.ctx, shellLife)
	cmd := b.Shell(ctx, parts.Session.Root)
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	size := &pty.Winsize{Cols: cols, Rows: rows}
	if size.Cols == 0 || size.Rows == 0 {
		size = &pty.Winsize{Cols: 100, Rows: 30}
	}
	tty, err := pty.StartWithSize(cmd, size)
	if err != nil {
		cancel()
		_ = loop.ManualObserve(id, "bash", tools.Result{Content: "Failed to start the shell: " + err.Error(), IsError: true}, 0)
		return refusal(errRefused, "the shell could not start: %v", err)
	}
	sh := &acpShell{id: id, cmd: cmd, tty: tty, tier: iso.Tier, started: time.Now(), pumped: make(chan struct{}),
		local:   iso.Tier == "process" || iso.Tier == "none",
		capture: termline.NewCapture(id, func(in agent.TerminalInput) { _ = loop.ManualTerminalInput(in) }),
		leader:  sandbox.Lead(cmd),
		prompt:  termline.NewPrompt(),
	}
	if sh.local {
		sh.capture.Hidden = func() bool { return termline.Hidden(tty) }
	}
	t.shell = sh
	prev := t.cancel
	t.cancel = func() { cancel(); prev() }
	return nil
}

// pumpShell sends the shell's output to Studio and keeps its latest part.
func (c *acpConn) pumpShell(t *acpTerminal) {
	sh := t.shell
	defer close(sh.pumped)
	buf := make([]byte, 8<<10)
	for {
		n, err := sh.tty.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			sh.capture.Output(chunk)
			// The first output is the shell's prompt; the foreground group
			// then is the shell's own.
			if sh.local && sh.shellPgrp.Load() == 0 {
				if fg, _, ok := ttyNow(sh.tty); ok {
					sh.shellPgrp.Store(int64(fg))
				}
			}
			sh.mu.Lock()
			sh.tail = termline.KeepTail(append(sh.tail, chunk...), shellTail)
			sh.mu.Unlock()
			if sh.local {
				sh.follow(chunk)
			} else {
				sh.prompt.OutputUnasked(chunk)
			}
			c.output(t, chunk)
		}
		if err != nil {
			return
		}
	}
}

// waitShell follows the shell to its end, sweeps what it left in its
// session, and records its exit with the latest output.
func (c *acpConn) waitShell(t *acpTerminal) {
	sh := t.shell
	_, err := sh.leader.Wait(sh.cmd)
	t.cancel()
	select {
	case <-sh.pumped:
	case <-time.After(500 * time.Millisecond):
	}
	_ = sh.tty.Close()
	select {
	case <-sh.pumped:
	case <-time.After(500 * time.Millisecond):
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = tools.ExitStatus(ee)
	} else if err != nil {
		code = -1
	}
	sh.capture.Flush()
	sh.mu.Lock()
	text := termline.PlainText(sh.tail)
	sh.mu.Unlock()
	// A withheld line the terminal echoed anyway, as one typed ahead of
	// read -s, is not kept in the output either.
	text = sh.capture.Scrub(text)
	res := tools.Result{Content: fmt.Sprintf("exit %d · interactive Abhed terminal; the latest output follows\n%s", code, text),
		ExitCode: &code, Tier: sh.tier}
	_ = t.session.parts.Loop.ManualObserve(sh.id, "bash", res, time.Since(sh.started))
	c.termMu.Lock()
	delete(c.terms, t.id)
	c.termMu.Unlock()
	c.notification("_abhed/terminal/exit", map[string]any{"terminalId": t.id, "code": code})
}

// shellInput forwards keys to the shell. Each line is put to the deny rules
// at its Enter, as typed; a refused line never reaches the shell, which is
// sent Ctrl-C instead to discard it.
func (c *acpConn) shellInput(t *acpTerminal, data []byte) *rpcError {
	sh := t.shell
	sh.inputMu.Lock()
	defer sh.inputMu.Unlock()
	keys := sh.capture.Keys(data)
	if len(keys) > shellLinesPerInput {
		return refusal(errParams, "too many lines in one input")
	}
	// Keys a program reads are not the start of the shell's next line.
	defer func() {
		if sh.programHasTerminal() {
			sh.capture.Abandon()
		}
	}()
	loop := t.session.parts.Loop
	for _, k := range keys {
		e := k.Enter
		if e != nil {
			sh.ask(e)
		}
		if e == nil || e.Program || e.Line == "" {
			if e != nil && !e.Program {
				sh.gave()
			}
			// Followed before the shell has it: a pasted line's echo can
			// come back before Write returns, and a line missed it.
			if e != nil {
				sh.capture.Entered(e)
			}
			if _, err := sh.tty.Write(k.Data); err != nil {
				return refusal(errRefused, "the terminal has ended")
			}
			continue
		}
		refused, err := loop.ManualScreen("u"+acpID(), e.Line)
		if err != nil {
			refused = &tools.Result{Content: "Denied: the line could not be recorded"}
		}
		if refused == nil {
			sh.gave()
			sh.capture.Entered(e)
			if _, err := sh.tty.Write(k.Data); err != nil {
				return refusal(errRefused, "the terminal has ended")
			}
			continue
		}
		c.output(t, []byte("\r\n\x1b[31m"+strings.ReplaceAll(refused.Content, "\n", "\r\n")+"\x1b[0m\r\n"))
		// A line pasted whole has not reached the shell at all, and an empty
		// line brings its prompt back. One typed earlier is in its buffer.
		discard := []byte{'\r'}
		if !e.Whole {
			discard = []byte{0x03}
		}
		sh.gave()
		if _, err := sh.tty.Write(discard); err != nil {
			return refusal(errRefused, "the terminal has ended")
		}
	}
	return nil
}

// ask fills in what the terminal says at a line's Enter, where this process
// holds it: a line read in canonical mode is not one typed at the prompt (a
// password, perhaps), and its text is withheld.
//
// Where it cannot ask, it cannot confirm the line was typed with echo on at
// the shell's prompt, so the line counts as typed ahead unless Abhed's own
// prompt is plainly back (container tier), or at all (a failed ask).
func (sh *acpShell) ask(e *termline.Entered) {
	e.Program = e.Alt
	if !sh.local {
		e.Ahead = !sh.prompt.At()
		return
	}
	e.Program = false
	fg, canonical, ok := ttyNow(sh.tty)
	if !ok {
		e.Ahead = true
		return
	}
	e.Known, e.Secret, e.Program = true, canonical, sh.isProgram(fg)
	e.Ahead = !sh.prompt.At()
}

// follow notes when the shell is back at its prompt.
func (sh *acpShell) follow(chunk []byte) {
	fg, canonical, ok := ttyNow(sh.tty)
	sh.prompt.Output(chunk, ok && !sh.isProgram(fg), canonical)
}

// gave notes that the shell was handed a line.
func (sh *acpShell) gave() { sh.prompt.Gave() }

// ttyNow asks the terminal; a variable so a test can make the ask fail.
var ttyNow = termline.TTYNow

func (sh *acpShell) isProgram(fg int) bool {
	shell := int(sh.shellPgrp.Load())
	return shell != 0 && fg != shell
}

func (sh *acpShell) programHasTerminal() bool {
	if !sh.local {
		return false
	}
	fg, _, ok := ttyNow(sh.tty)
	return ok && sh.isProgram(fg)
}
