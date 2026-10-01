//go:build darwin || linux

package app

import (
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
	sh := &acpShell{tty: tty, local: true, since: []byte("\n")}
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
