//go:build unix

package app

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// The todo list is drawn under the call that wrote it; while any of it is
// open a summary sits above the input, Ctrl-T lists it, and /todos draws it.
func TestTUITodoChecklistAndCtrlT(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 90, 30)
	r.send("please todo\r")
	r.waitText("Done.")
	r.quiet(300 * time.Millisecond)
	all := r.term.All()
	for _, want := range []string{"● Todos", "⎿  ☒ read the file", "◼ change the greeting", "☐ run the tests"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q:\n%s", want, all)
		}
	}
	r.waitScreen("Todos 1/3 done · change the greeting · ctrl+t to list")
	r.send("\x14")
	r.waitScreen("ctrl+t to hide")
	t.Log(r.capture("todo list after ctrl+t"))
	r.send("\x14")
	r.waitScreen("ctrl+t to list")
	r.send("/todos\r")
	r.waitText("● Todos · 1/3 done · change the greeting")
}

// A call a configured rule approves says which rule did it.
func TestTUIAutoApprovalNamesTheRule(t *testing.T) {
	stub, ws := tuiWorkspace(t, `,"permissions":{"allow":["bash(echo *)"]}`)
	r := startTUI(t, stub, ws, 90, 30)
	r.send("please allowed\r")
	r.waitText("Done.")
	r.quiet(300 * time.Millisecond)
	all := r.term.All()
	call := strings.Index(all, "● Bash(echo allowed-by-rule)")
	auto := strings.Index(all, "✓ auto: bash(echo *)")
	if call < 0 || auto < call {
		t.Fatalf("no auto line under the call:\n%s", all)
	}
}

// A subagent's start and return are drawn as a tree under the task call.
func TestTUISubagentTreeLines(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 100, 30, "-mode", "accept-edits")
	r.send("please delegate\r")
	r.waitText("Done.")
	r.quiet(300 * time.Millisecond)
	all := r.term.All()
	start := strings.Index(all, "started · look around")
	ret := strings.Index(all, "returned · completed")
	if start < 0 || ret < start || !strings.Contains(all, "├ ") || !strings.Contains(all, "└ ") {
		t.Fatalf("no subagent tree:\n%s", all)
	}
	t.Log(r.capture("subagent tree"))
}

// A mode change made with /mode is one line, from the record.
func TestTUIModeChangeLine(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 90, 30)
	r.send("hello\r")
	r.waitText("Hello from the stub")
	r.send("/mode plan\r")
	r.waitText("◆ mode: default → plan · slash")
	r.quiet(300 * time.Millisecond)
	if n := strings.Count(r.term.All(), "mode: "); n != 1 {
		t.Fatalf("the change was shown %d times:\n%s", n, r.term.All())
	}
}

// The title follows the session; an unfocused terminal is told, in fixed
// words, that an approval waits and that a turn ended, and a focused one
// is not.
func TestTUIAttentionTitleAndNotify(t *testing.T) {
	stub, ws := tuiWorkspace(t, `,"cli":{"notify":"osc9"}`)
	r := startTUI(t, stub, ws, 90, 30)
	r.quiet(200 * time.Millisecond)
	r.mu.Lock()
	boot := string(r.raw)
	r.mu.Unlock()
	if !strings.Contains(boot, "\x1b[?1004h") || !strings.Contains(boot, "\x1b]0;abhed · ready\x1b\\") {
		t.Fatalf("focus reports or title not set at start: %q", boot)
	}

	r.send("\x1b[O") // the person switched away
	r.markBytes()
	r.send("hello\r")
	r.waitText("Hello from the stub")
	r.quiet(300 * time.Millisecond)
	raw := r.rawSinceMark()
	for _, want := range []string{"\x1b]0;abhed · working\x1b\\", "\x1b]0;abhed · ready\x1b\\", "\x1b]9;abhed: turn finished\x1b\\"} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %q in %q", want, raw)
		}
	}
	if strings.Contains(raw, "\x1b]9;Hello") || strings.Contains(raw, "\x1b]0;Hello") {
		t.Fatalf("model text reached a title or notification: %q", raw)
	}

	r.markBytes()
	r.send("please rm\r")
	r.waitScreen("rm -rf build")
	raw = r.rawSinceMark()
	for _, want := range []string{"\x1b]0;abhed · approval needed\x1b\\", "\x1b]9;abhed: approval waiting\x1b\\"} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %q in %q", want, raw)
		}
	}
	time.Sleep(400 * time.Millisecond) // past the dialog's guard against keys typed in haste
	r.send("\x1b")                     // decline
	r.waitText("✕ Declined")
	r.waitFor("the prompt to be idle", false, func(s string) bool { return !strings.Contains(s, "esc to interrupt") })

	r.send("\x1b[I") // back
	r.markBytes()
	r.send("again\r")
	r.waitText("You said: again")
	r.quiet(300 * time.Millisecond)
	if raw := r.rawSinceMark(); strings.Contains(raw, "\x1b]9;") {
		t.Fatalf("a focused terminal was notified: %q", raw)
	}
}

// Each is off when its setting says so.
func TestTUIAttentionOff(t *testing.T) {
	stub, ws := tuiWorkspace(t, `,"cli":{"title":false,"notify":"off","copy":false}`)
	r := startTUI(t, stub, ws, 90, 30)
	r.send("\x1b[O")
	r.send("hello\r")
	r.waitText("Hello from the stub")
	r.send("/copy\r")
	r.waitText("copying is turned off by cli.copy")
	r.quiet(200 * time.Millisecond)
	r.mu.Lock()
	raw := string(r.raw)
	r.mu.Unlock()
	for _, never := range []string{"\x1b]0;", "\x1b]9;", "\x1b]52;", "\x1b[22;0t"} {
		if strings.Contains(raw, never) {
			t.Errorf("%q sent with it turned off", never)
		}
	}
}

// /copy puts the last reply on the clipboard through OSC 52, and only when asked.
func TestTUICopyLastReply(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 90, 30)
	r.send("hello\r")
	r.waitText("Hello from the stub")
	r.quiet(200 * time.Millisecond)
	r.mu.Lock()
	before := string(r.raw)
	r.mu.Unlock()
	if strings.Contains(before, "\x1b]52;") {
		t.Fatal("the clipboard was written before /copy")
	}
	r.markBytes()
	r.send("/copy\r")
	r.waitText("sent the last reply (1 line)")
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("Hello from the stub. You said: hello")) + "\x1b\\"
	if raw := r.rawSinceMark(); !strings.Contains(raw, want) {
		t.Fatalf("no OSC 52 with the reply: %q", raw)
	}
}
