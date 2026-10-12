//go:build unix

package sandbox

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// The workbench terminal tells a line typed into a program from one typed at
// the prompt by the terminal's foreground group, so the shell must run each
// program in a group of its own (job control) under every local tier.
func TestShellGivesEachProgramItsOwnProcessGroup(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		ws := workspace(t)
		programTakesTheTerminal(t, NewNone(DefaultPolicy(ws)), ws)
	})
	t.Run("process", func(t *testing.T) {
		requireNetNS(t)
		ws := workspace(t)
		programTakesTheTerminal(t, processSandbox(t, ws, false).(Interactive), ws)
	})
}

func programTakesTheTerminal(t *testing.T, s Interactive, ws string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := s.Shell(ctx, ws)
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = tty.Write([]byte("exit\r")); _ = cmd.Wait(); _ = tty.Close() }()
	go func() { _, _ = io.Copy(io.Discard, tty) }()
	fg := func() int {
		g, err := unix.IoctlGetInt(int(tty.Fd()), unix.TIOCGPGRP) // #nosec G115 -- a file descriptor fits an int
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	// The shell is at its prompt once it has written a marker file.
	_, _ = tty.Write([]byte("touch ready\r"))
	waitFile(t, ws+"/ready")
	shell := fg()
	_, _ = tty.Write([]byte("sleep 5\r"))
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if g := fg(); g != shell {
			_, _ = tty.Write([]byte{0x03})
			return
		}
	}
	t.Fatalf("sleep ran in the shell's own process group %d: the shell has no job control", shell)
}

func waitFile(t *testing.T, p string) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(p); err == nil {
			return
		}
	}
	t.Fatalf("%s did not appear", p)
}
