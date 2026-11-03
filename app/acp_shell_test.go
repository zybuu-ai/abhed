//go:build darwin || linux

package app

import (
	"os"
	"os/exec"
	"testing"

	"github.com/creack/pty"

	"github.com/zybuu-ai/abhed/internal/termline"
)

// A line that arrives before the shell is back at its prompt is typed ahead:
// what reads it may turn echo off after it came, so its text is withheld.
func TestShellLineTypedAheadOfThePrompt(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "stty raw -echo; echo up; sleep 30")
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Skip("no pty:", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = tty.Close() })
	buf := make([]byte, 64)
	if _, err := tty.Read(buf); err != nil {
		t.Fatal(err)
	}
	sh := &acpShell{tty: tty, local: true, prompt: termline.NewPrompt()}
	fg, canonical, ok := termline.TTYNow(tty)
	if !ok || canonical {
		t.Skipf("the terminal cannot be asked here (ok=%v canonical=%v)", ok, canonical)
	}
	sh.shellPgrp.Store(int64(fg))
	asked := func() bool {
		e := &termline.Entered{Line: "hunter22", Whole: true}
		sh.ask(e)
		return e.Ahead
	}
	if !asked() {
		t.Fatal("a line before the first prompt was taken as typed at it")
	}
	sh.follow([]byte("$ "))
	if asked() {
		t.Fatal("a line at the prompt was taken as typed ahead")
	}
	sh.gave()
	sh.follow([]byte("hi")) // the echo of keys typed before the Enter
	if !asked() {
		t.Fatal("a late echo was taken for the prompt")
	}
	sh.follow([]byte("\r\n")) // the line's own newline is not the prompt
	if !asked() {
		t.Fatal("a line before the prompt came back was taken as typed at it")
	}
	sh.follow([]byte("got 8\r\n$ "))
	if asked() {
		t.Fatal("the prompt's return was missed")
	}
}

// Where asking the terminal fails, a line cannot be confirmed as typed at the
// prompt with echo on, so it counts as typed ahead.
func TestShellLineIsAheadWhenTheAskFails(t *testing.T) {
	fakeTTY(t, func(*os.File) (int, bool, bool) { return 0, false, false })
	sh := &acpShell{local: true, prompt: termline.NewPrompt()}
	sh.prompt.Output([]byte("$ "), true, false)
	e := &termline.Entered{Line: "hunter22", Whole: true}
	sh.ask(e)
	if !e.Ahead || e.Echoed() {
		t.Fatalf("a line was trusted although the terminal could not be asked: %+v", e)
	}
}

// Where the terminal cannot be asked, as on the container tier, a line is
// typed ahead unless Abhed's prompt is back: a password typed while a command
// runs is withheld, however slow the machine.
func TestShellLineUnaskedIsAheadUntilThePrompt(t *testing.T) {
	sh := &acpShell{local: false, prompt: termline.NewPrompt()}
	asked := func() bool {
		e := &termline.Entered{Line: "hunter44", Whole: true}
		sh.ask(e)
		return e.Ahead
	}
	if !asked() {
		t.Fatal("a line before the first prompt was taken as typed at it")
	}
	sh.prompt.OutputUnasked([]byte("(sandbox: container) ws $ "))
	if asked() {
		t.Fatal("a line at the prompt was taken as typed ahead")
	}
	sh.gave()
	sh.prompt.OutputUnasked([]byte("echo BUSY; sleep 2; read -s pw\r\nBUSY\r\n"))
	if !asked() {
		t.Fatal("a line typed while the command ran was taken as typed at the prompt")
	}
}

// bash says when it could not take the terminal; the engine notes it, since
// programs then share the shell's group and are never seen as programs.
func TestShellNoticesNoJobControl(t *testing.T) {
	if !jobControlOff([]byte("bash: cannot set terminal process group (-1): Inappropriate ioctl for device\r\nbash: no job control in this shell\r\n")) {
		t.Fatal("bash's no-job-control notice was missed")
	}
	if jobControlOff([]byte("(sandbox: process) ws $ ")) {
		t.Fatal("a prompt was taken for the no-job-control notice")
	}
}

// The shell's group is learned only from a shell leading the group in front
// at its line editor that Abhed started, directly or through bubblewrap and
// the relay: a starter in front, a wrapper in canonical mode, a member that is
// not the leader, or a shell under sh -c or a nested shell is never taken.
func TestShellGroupIsTheShellsOwn(t *testing.T) {
	var fg int
	var canonical bool
	fakeTTY(t, func(*os.File) (int, bool, bool) { return fg, canonical, true })
	type proc struct {
		name       string
		ppid, pgid int
	}
	procs := map[int]proc{
		1:  {"abhed", 0, 1},
		5:  {"bwrap", 1, 5},  // what Abhed started
		6:  {"bwrap", 5, 5},  // bubblewrap's child
		10: {"relay", 6, 10}, // the egress relay, printing first
		20: {"bash", 10, 20}, // the shell
		21: {"bash", 20, 20}, // a member of its group, not its leader
		30: {"zsh", 20, 30},  // a shell the shell started
		40: {"sh", 10, 40},   // a wrapper
		41: {"bash", 40, 41}, // a shell under it
	}
	fake := func(pid int) (string, int, int, bool) { p, ok := procs[pid]; return p.name, p.ppid, p.pgid, ok }
	testProcess.Store(&fake)
	t.Cleanup(func() { testProcess.Store(nil) })
	sh := &acpShell{local: true, root: 5, prompt: termline.NewPrompt()}
	for _, c := range []struct {
		fg        int
		canonical bool
	}{{10, false}, {99, false}, {20, true}, {21, false}, {30, false}, {41, false}} {
		fg, canonical = c.fg, c.canonical
		sh.settle()
		if got := sh.shellPgrp.Load(); got != 0 {
			t.Fatalf("group %d (canonical %v) taken for the shell's", c.fg, c.canonical)
		}
		e := &termline.Entered{Line: "x"}
		if sh.ask(e); e.Program {
			t.Fatal("a line was taken for a program's while the shell's group is unknown")
		}
	}
	fg, canonical = 20, false
	sh.settle()
	if got := sh.shellPgrp.Load(); got != 20 {
		t.Fatalf("shell group %d, want 20", got)
	}
	if !sh.isProgram(30) {
		t.Fatal("a nested shell was taken for the shell")
	}
	// Where Abhed starts the shell itself (none tier, sandbox-exec), it is the shell.
	direct := &acpShell{local: true, root: 20, prompt: termline.NewPrompt()}
	direct.settle()
	if got := direct.shellPgrp.Load(); got != 20 {
		t.Fatalf("a shell Abhed started itself: group %d, want 20", got)
	}
}
