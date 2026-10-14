//go:build unix

package ui

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
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
	if os.Getenv("ABHED_UI_PANIC_HELPER") == "title" {
		l.SetAttention(Attention{Title: true})
	}
	if os.Getenv("ABHED_UI_PANIC_HELPER") == "key" {
		// The panic happens on the dock's own key reader, in a hotkey.
		l.Hotkey("ctrl+t", func() { panic("boom from a goroutine") })
		l.Ready()
	} else {
		go func() {
			defer RestoreOnPanic()
			panic("boom from a goroutine")
		}()
	}
	// The panic ends the program long before this; the test stops it if not.
	time.Sleep(time.Minute)
}

// A panic on any goroutine puts the terminal back before the program ends:
// the modes it turned on are turned off.
func TestPanicRestoresTheTerminal(t *testing.T) {
	for _, mode := range []string{"goroutine", "key", "title"} {
		t.Run(mode, func(t *testing.T) { panicRestores(t, mode) })
	}
}

func panicRestores(t *testing.T, mode string) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestPanicHelper$")
	cmd.Env = append(os.Environ(), "ABHED_UI_PANIC_HELPER="+mode, "TERM=xterm-256color", "HOME="+t.TempDir())
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer tty.Close()
	var mu sync.Mutex
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := tty.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				close(done)
				return
			}
		}
	}()
	screen := func() string { mu.Lock(); defer mu.Unlock(); return out.String() }
	// Each step waits for what it answers, not for a time, so a slow machine
	// (an emulated one) gets the same sequence as a fast one.
	waitFor := func(what string) bool {
		for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if strings.Contains(screen(), what) {
				return true
			}
			select {
			case <-done:
				return strings.Contains(screen(), what)
			default:
			}
		}
		return false
	}
	go func() { // answer the startup questions once asked, as a terminal would
		if !waitFor("\x1b]11;?") {
			return
		}
		_, _ = io.WriteString(tty, "\x1b]11;rgb:0000/0000/0000\x07\x1b[?62c")
		// The key goes once the dock is drawn and reading keys.
		if mode == "key" && waitFor(modesOn) && waitFor("? for shortcuts") {
			_, _ = io.WriteString(tty, "\x14") // Ctrl-T
		}
	}()
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(45 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	got := screen()
	on, off := strings.Index(got, modesOn), strings.LastIndex(got, modesOff)
	if on < 0 || off < on {
		t.Fatalf("the terminal was not restored after the panic:\n%q", got)
	}
	if mode == "title" && !strings.Contains(got[off:], "\x1b[23;0t") {
		t.Fatalf("the title pushed was not popped after the panic:\n%q", got)
	}
	if !strings.Contains(got, "boom from a goroutine") {
		t.Fatalf("the panic was swallowed:\n%q", got)
	}
}
