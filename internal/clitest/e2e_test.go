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
	h.WaitText("turns")
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
	h.Type("v\n")
	h.WaitOutput(`"nope"`)
	h.Type("t\n")
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
