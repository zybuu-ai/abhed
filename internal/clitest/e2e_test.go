//go:build unix

package clitest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A session on a terminal: the prompt, a task, the stub's answer on the
// screen, the request the stub saw, and a clean exit.
func TestHelloOnPty(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{Script: `text "Hello from the stub."`})
	h.WaitText("Type a task")
	h.Type("fix it")
	h.Key(Enter)
	h.WaitText("Hello from the stub.")
	reqs := h.Requests()
	if len(reqs) != 1 || !strings.Contains(string(reqs[0].Body), "fix it") {
		t.Fatalf("requests: %d", len(reqs))
	}
	h.Exit(0)
}

// The golden transcript of a short session holds.
func TestGoldenHello(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{Script: "text \"Hello from the stub.\"\nusage in=1200 cached=900 out=8"})
	h.WaitText("Type a task")
	h.Type("hi")
	h.Key(Enter)
	h.WaitText("Hello from the stub.")
	waitIdle(h)
	h.WaitQuiet(150*time.Millisecond, 2*time.Second)
	h.AssertGoldens("hello", nil)
	h.Exit(0)
}

// Piped stdin with no terminal: every line is read and the run ends at EOF.
func TestPipedSession(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{Piped: true, Stdin: "hi\n", Script: `text "piped answer"`})
	if code := h.Wait(20 * time.Second); code != 0 {
		t.Fatalf("exit %d:\n%s", code, h.Output())
	}
	if !strings.Contains(h.Stdout(), "piped answer") {
		t.Fatalf("stdout:\n%s", h.Stdout())
	}
}

// Ported from app's trust tests: the prompt, viewing the file, trusting it.
func TestTrustPromptOnPty(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{Setup: func(home, ws string) {
		writeFile(t, filepath.Join(ws, ".abhed", "config.json"),
			`{"model":{"default":"nope"},"permissions":{"mode":"bypass","deny":["bash(curl*)"]}}`)
	}})
	h.WaitText("Trust this file?")
	h.Type("3\n")
	h.WaitOutput(`"nope"`)
	h.Type("2\n")
	h.WaitOutput(`model "nope" is not defined`)
	if code := h.Wait(10 * time.Second); code != 1 {
		t.Fatalf("exit %d", code)
	}
}

// A resize reaches the emulator and the binary alike, and the session goes on.
func TestResize(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{Cols: 120, Rows: 40, Script: `text "after resize"`})
	h.WaitText("Type a task")
	h.Resize(40, 20)
	if s := h.Screen(); s.Cols() != 40 || s.Rows() != 20 {
		t.Fatalf("size %dx%d", s.Cols(), s.Rows())
	}
	h.Type("go")
	h.Key(Enter)
	h.WaitText("after resize")
	h.Exit(0)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The harness is usable through the interface alone, and the terminal
// modes the binary sets are visible.
func TestInterfaceOnly(t *testing.T) {
	t.Parallel()
	h := Start(t, Opts{Script: `text "ok"`})
	h.WaitText("Type a task")
	if h.Screen().Modes().AltScreen {
		t.Fatal("the line UI is not on the alternate screen")
	}
	if !strings.Contains(h.ScreenGolden(), "‹ws›") {
		t.Fatalf("golden not normalized:\n%s", h.ScreenGolden())
	}
	h.Exit(0)
}

// Keys typed while the binary writes: the dock takes keys and output on
// separate goroutines, which the -race build checks.
func TestTypingWhileOutputArrives(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{Script: "delay 100ms\ntext \"line one\\n\"\ndelay 100ms\ntext \"line two\\n\"\ndelay 100ms\ntext \"line three\\n\""})
	h.WaitText("Type a task")
	h.Type("go")
	h.Key(Enter)
	// Keys within a few milliseconds of the Enter make it a pasted newline:
	// typing starts once the turn has.
	h.WaitScreen(func(s Screen) bool { return s.Contains("esc to interrupt") }, DefaultTimeout)
	for _, c := range "steering while it writes" {
		h.Type(string(c))
		time.Sleep(10 * time.Millisecond)
	}
	h.WaitOutput("line three")
	h.Key(Enter)
	h.Settle()
	h.Exit(0)
}

// waitIdle waits for the prompt to be idle again after a turn: the footer
// shows its shortcuts hint, not the turn's "esc to interrupt".
func waitIdle(h Harness) {
	h.WaitScreen(func(s Screen) bool {
		return s.Contains("? for shortcuts") && !s.Contains("esc to interrupt")
	}, DefaultTimeout)
}

// closePanel closes the full-screen view a command opened, if one is open,
// and waits for the prompt to come back.
func closePanel(h Harness) {
	h.Settle()
	if !h.Screen().Modes().AltScreen {
		return
	}
	h.Key(Esc)
	h.WaitScreen(func(s Screen) bool { return !s.Modes().AltScreen }, DefaultTimeout)
}
