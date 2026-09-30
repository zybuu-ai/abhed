//go:build unix

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A model-chosen command cannot show one thing in the approval and run
// another: "touch pwned #<ZWJ><CR>│ $ ls -la" used to draw as "$ ls -la"
// alone. The dialog shows the whole command, with the hidden runes marked.
func TestTUIApprovalShowsTheRealCommand(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 100, 30)
	r.send("please spoof\r")
	r.waitScreen("Run this command?")
	screen := r.term.Text()
	t.Log(r.capture("the approval for a spoofing command"))
	for _, want := range []string{"touch pwned #⟨U+200D⟩⟨\\r⟩│ $ ls -la", "Bash(touch pwned"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("the dialog does not show %q:\n%s", want, r.term.Dump())
		}
	}
	time.Sleep(400 * time.Millisecond)
	r.send("\x1b")
	r.waitText("✕ Declined")
	if _, err := os.Stat(filepath.Join(ws, "pwned")); err == nil {
		t.Fatal("the command ran")
	}
}

// A model-chosen path cannot write a clipboard, set the title or erase the
// screen through the dialog, its question, its "always" label or the result.
func TestTUIPathEscapesNeverReachTheTerminal(t *testing.T) {
	for _, mode := range []string{"default", "accept-edits"} {
		t.Run(mode, func(t *testing.T) {
			stub, ws := tuiWorkspace(t, "")
			r := startTUI(t, stub, ws, 100, 30, "-mode", mode)
			r.markBytes()
			r.send("please escape\r")
			if mode == "default" {
				r.waitScreen("?")
				r.waitFor("the dialog", false, func(s string) bool { return strings.Contains(s, "Create ") })
				time.Sleep(400 * time.Millisecond)
				r.send("\x1b")
				r.waitText("✕ Declined")
			} else {
				r.waitText("Done.")
			}
			r.quiet(200 * time.Millisecond)
			wire := r.rawSinceMark()
			for _, f := range []string{"\x1b]52", "\x1b]0;TITLE", "\x1b[2J", "\a"} {
				if strings.Contains(wire, f) {
					t.Fatalf("%q reached the terminal", f)
				}
			}
			if !strings.Contains(r.term.All(), "⟨\\e⟩]52") && mode == "default" {
				t.Fatalf("the hidden escape is not shown in the dialog:\n%s", r.term.All())
			}
		})
	}
}
