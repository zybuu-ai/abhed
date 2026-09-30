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
