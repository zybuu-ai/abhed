//go:build unix

package ui

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestPanicHelper is the program the panic test runs on a terminal.
func TestPanicHelper(t *testing.T) {
	if os.Getenv("ABHED_UI_PANIC_HELPER") == "" {
		t.Skip("run by TestPanicRestoresTheTerminal")
	}
	l := NewLineReader("> ")
	if !l.Raw() {
		os.Exit(3)
	}
	go func() {
		defer RestoreOnPanic()
		panic("boom from a goroutine")
	}()
	time.Sleep(5 * time.Second)
}

// A panic on any goroutine puts the terminal back before the program ends:
// the modes it turned on are turned off.
func TestPanicRestoresTheTerminal(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestPanicHelper$")
	cmd.Env = append(os.Environ(), "ABHED_UI_PANIC_HELPER=1", "TERM=xterm-256color", "HOME="+t.TempDir())
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer tty.Close()
	go func() { // answer the startup questions, as a terminal would
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(tty, "\x1b]11;rgb:0000/0000/0000\x07\x1b[?62c")
	}()
	var out bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&out, tty); close(done) }()
	_ = cmd.Wait()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	got := out.String()
	on, off := strings.Index(got, modesOn), strings.LastIndex(got, modesOff)
	if on < 0 || off < on {
		t.Fatalf("the terminal was not restored after the panic:\n%q", got)
	}
	if !strings.Contains(got, "boom from a goroutine") {
		t.Fatalf("the panic was swallowed:\n%q", got)
	}
}
