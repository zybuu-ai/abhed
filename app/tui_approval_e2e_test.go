//go:build unix

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fileHas(t *testing.T, path, prefix string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	return err == nil && strings.HasPrefix(string(data), prefix)
}

// Through the whole program: ↓ then Enter within milliseconds of the dialog
// appearing, as typing meant for the prompt would send, does nothing. A
// deliberate key later approves, and the dialog, its diff and its answer stay
// in the transcript.
func TestTUIApprovalRaceAndRecord(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 80, 34)
	r.send("please edit\r")
	r.waitScreen("Make this edit to hello.txt?")
	r.send("\x1b[B\r")
	r.send("1")
	time.Sleep(time.Second)
	if fileHas(t, filepath.Join(ws, "hello.txt"), "hello, abhed") {
		t.Fatalf("a key pressed as the dialog appeared approved the edit:\n%s", r.term.Dump())
	}
	shown := r.capture("the approval dialog, one second after ↓ Enter 1 were pressed with it")
	for _, want := range []string{"1. Yes", "2. Yes, and don't ask again for edit(hello.txt) this session", "3. No, and tell Abhed", "1 - hello world", "1 + hello, abhed world", "policy step: default"} {
		if !strings.Contains(r.term.Text(), want) {
			t.Fatalf("the dialog lacks %q:\n%s", want, shown)
		}
	}
	t.Log(shown)
	r.send("1")
	r.waitText("Done.")
	r.quiet(300 * time.Millisecond)
	if !fileHas(t, filepath.Join(ws, "hello.txt"), "hello, abhed world") {
		t.Fatalf("the approved edit was not applied")
	}
	all := r.term.All()
	for _, want := range []string{"● Edit(hello.txt)", "1 + hello, abhed world", "⎿ ✓ Approved", "Updated hello.txt with 2 additions and 1 removal"} {
		if !strings.Contains(all, want) {
			t.Fatalf("the transcript lacks %q:\n%s", want, all)
		}
	}
	if strings.Count(all, "1 + hello, abhed world") != 1 {
		t.Fatalf("the diff is drawn twice:\n%s", all)
	}
	t.Log(r.capture("after approving"))
}

// A destructive command offers no "always", and its second question answers
// No to Enter.
func TestTUIDestructiveNeedsASecondYes(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	if err := os.MkdirAll(filepath.Join(ws, "build"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := startTUI(t, stub, ws, 80, 30)
	r.send("please rm\r")
	r.waitScreen("Run this command?")
	if strings.Contains(r.term.Text(), "don't ask again") {
		t.Fatalf("always offered for rm -rf:\n%s", r.term.Dump())
	}
	time.Sleep(400 * time.Millisecond)
	r.send("1")
	r.waitScreen("Really run it?")
	time.Sleep(400 * time.Millisecond)
	r.send("\r")
	r.waitText("Not run")
	r.waitText("Interrupted")
	if _, err := os.Stat(filepath.Join(ws, "build")); err != nil {
		t.Fatalf("build was removed")
	}
}

// No stops the turn, so the person can say what to do instead.
func TestTUIDeclineStopsTheTurn(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 80, 30)
	r.send("please edit\r")
	r.waitScreen("Make this edit")
	time.Sleep(400 * time.Millisecond)
	r.send("\x1b")
	r.waitText("✕ Declined")
	r.waitText("Interrupted · tell Abhed what to do instead")
	r.waitFor("the prompt to be idle", false, func(s string) bool { return !strings.Contains(s, "esc to interrupt") })
}
