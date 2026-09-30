//go:build unix

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// On a terminal with a white background the light theme is chosen at
// startup, from the terminal's own answer; keys typed while it answers
// reach the prompt.
func TestTUIThemeFromTheTerminal(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUIWith(t, stub, ws, 80, 24, "ffff/ffff/ffff")
	r.waitText("Type a task")
	r.mu.Lock()
	raw := string(r.raw)
	r.mu.Unlock()
	if !strings.Contains(raw, "38;5;166") || strings.Contains(raw, "38;5;202") {
		t.Fatalf("the light theme was not chosen for a white background")
	}
}

// /theme switches, redraws in the new colours, and is kept for the next
// session.
func TestTUIThemeCommand(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 80, 24)
	r.send("/theme high-contrast\r")
	r.waitText("theme: high-contrast")
	data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".abhed", "ui.json"))
	if err != nil || !strings.Contains(string(data), `"high-contrast"`) {
		t.Fatalf("the theme was not saved: %s %v", data, err)
	}
	r.exit()
	r2 := startTUI(t, stub, ws, 80, 24)
	r2.markBytes()
	r2.send("hello\r")
	r2.waitText("You said: hello")
	time.Sleep(100 * time.Millisecond)
	if !strings.Contains(r2.rawSinceMark(), "1;38;5;214") {
		t.Fatalf("the next session is not in the saved theme")
	}
	r2.send("/theme nope\r")
	r2.waitText("unknown theme")
}

// Over a slow link the terminal answers after startup has stopped waiting:
// the answer is not typed into the prompt and does not open the editor.
func TestTUILateReplyIsNotTyped(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	marker := filepath.Join(ws, "editor-ran")
	t.Setenv("VISUAL", "touch "+marker)
	r := startTUIDelayed(t, stub, ws, 80, 24, "ffff/ffff/ffff", 300*time.Millisecond)
	time.Sleep(600 * time.Millisecond)
	r.send("hello\r")
	r.waitText("You said: hello")
	if p := stub.prompt(); p != "hello" {
		t.Fatalf("the model got %q", p)
	}
	if strings.Contains(r.term.All(), "11;rgb") {
		t.Fatalf("the answer was typed:\n%s", r.term.All())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the answer's BEL opened the editor")
	}
}

// A late answer split by the link right after "ESC ]" is still an answer:
// neither half is typed and its BEL does not open the editor.
func TestTUISplitLateReplyIsNotTyped(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	marker := filepath.Join(ws, "editor-ran")
	t.Setenv("VISUAL", "touch "+marker)
	r := startTUI(t, stub, ws, 80, 24)
	r.quiet(100 * time.Millisecond)
	r.send("\x1b]")
	time.Sleep(80 * time.Millisecond)
	r.send("11;rgb:ffff/ffff/ffff\x07")
	time.Sleep(300 * time.Millisecond)
	r.send("hello\r")
	r.waitText("You said: hello")
	if p := stub.prompt(); p != "hello" {
		t.Fatalf("the model got %q", p)
	}
	if strings.Contains(r.term.All(), "rgb:") {
		t.Fatalf("the answer was typed:\n%s", r.term.All())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the answer's BEL opened the editor")
	}
}
