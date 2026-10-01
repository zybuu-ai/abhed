//go:build darwin || linux

package server

import (
	"os"
	"os/exec"
	"testing"

	"github.com/creack/pty"

	"github.com/zybuu-ai/abhed/internal/termline"
)

// A line that arrives before the shell is back at its prompt is typed ahead,
// and the workbench withholds its text as Studio does.
func TestWorkbenchLineTypedAheadOfThePrompt(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "stty raw -echo; echo up; sleep 30")
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Skip("no pty:", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = tty.Close() })
	if _, err := tty.Read(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	fg, canonical, ok := ttyNow(tty)
	if !ok || canonical {
		t.Skipf("the terminal cannot be asked here (ok=%v canonical=%v)", ok, canonical)
	}
	run := &ptyRun{tty: tty, local: true, prompt: termline.NewPrompt()}
	run.shellPgrp.Store(int64(fg))
	asked := func() bool {
		e := &enteredLine{Line: "hunter22", Whole: true}
		run.ask(e)
		return e.Ahead && !e.Echoed()
	}
	if !asked() {
		t.Fatal("a line before the first prompt was taken as typed at it")
	}
	run.prompt.Output([]byte("$ "), true, false)
	if asked() {
		t.Fatal("a line at the prompt was taken as typed ahead")
	}
	run.gave()
	run.prompt.Output([]byte("\r\n"), true, false)
	if !asked() {
		t.Fatal("a line before the prompt came back was taken as typed at it")
	}
}

// Where asking the terminal fails, a line cannot be confirmed as typed at the
// prompt with echo on, so it counts as typed ahead.
func TestWorkbenchLineIsAheadWhenTheAskFails(t *testing.T) {
	ttyNow = func(*os.File) (int, bool, bool) { return 0, false, false }
	t.Cleanup(func() { ttyNow = termline.TTYNow })
	run := &ptyRun{local: true, prompt: termline.NewPrompt()}
	run.prompt.Output([]byte("$ "), true, false)
	e := &enteredLine{Line: "hunter22", Whole: true}
	run.ask(e)
	if !e.Ahead || e.Echoed() {
		t.Fatalf("a line was trusted although the terminal could not be asked: %+v", e)
	}
}

// On the container tier the server cannot ask the terminal: a line is typed
// ahead, and its text withheld, unless Abhed's prompt is back.
func TestWorkbenchLineUnaskedIsAheadUntilThePrompt(t *testing.T) {
	run := &ptyRun{local: false, prompt: termline.NewPrompt()}
	asked := func() bool {
		e := &enteredLine{Line: "hunter44", Whole: true}
		run.ask(e)
		return e.Ahead
	}
	if !asked() {
		t.Fatal("a line before the first prompt was taken as typed at it")
	}
	run.prompt.OutputUnasked([]byte("(sandbox: container) ws $ "))
	if asked() {
		t.Fatal("a line at the prompt was taken as typed ahead")
	}
	run.gave()
	run.prompt.OutputUnasked([]byte("echo BUSY; sleep 2; read -s pw\r\nBUSY\r\n"))
	if !asked() {
		t.Fatal("a line typed while the command ran was taken as typed at the prompt")
	}
}
