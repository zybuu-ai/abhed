//go:build unix

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Audit #9 and #11: an edit showed only old and new strings, no context or
// line numbers, and after it was applied only "Edited hello.txt" remained;
// paths were absolute everywhere. In accept-edits mode, where nothing is
// asked, the diff is drawn under the call and stays in the transcript.
func TestTUIEditDiffStaysInTheTranscript(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 80, 30, "-mode", "accept-edits")
	r.send("please edit\r")
	r.waitText("Done.")
	r.quiet(300 * time.Millisecond)
	all := r.term.All()
	t.Log(r.capture("edit in accept-edits mode"))
	for _, want := range []string{
		"● Edit(hello.txt)",
		"Updated hello.txt with 2 additions and 1 removal",
		"1 - hello world",
		"1 + hello, abhed world",
		"2 + second line added",
		"3   the end",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q:\n%s", want, all)
		}
	}
	real, _ := filepath.EvalSymlinks(ws)
	for _, p := range []string{ws, real} {
		if strings.Contains(all, p+"/hello.txt") {
			t.Errorf("an absolute path is shown:\n%s", all)
		}
	}
	data, _ := os.ReadFile(filepath.Join(ws, "hello.txt"))
	if !strings.HasPrefix(string(data), "hello, abhed world") {
		t.Fatalf("the edit was not applied: %q", data)
	}
}

// A write of a new file shows what it creates, with the rest one key away.
func TestTUIWriteShowsTheNewFile(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 80, 40, "-mode", "accept-edits")
	r.send("please write\r")
	r.waitText("Done.")
	r.quiet(300 * time.Millisecond)
	all := r.term.All()
	for _, want := range []string{"● Write(notes/new.md)", "Created notes/new.md with 30 lines", " 1 + line 1", "30 + line 30"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q:\n%s", want, all)
		}
	}
}

// Audit #10: a successful command showed only "exit 0", with no preview and
// no way to see the output. Its first and last lines show, with the count
// between, and Ctrl-O shows all of it.
func TestTUICommandOutputPreviewExpands(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 80, 30, "-allow", "bash(seq *)")
	r.send("please big\r")
	r.waitText("Done.")
	r.quiet(300 * time.Millisecond)
	all := r.term.All()
	t.Log(r.capture("seq 1 3000"))
	for _, want := range []string{"● Bash(seq 1 3000)", "⎿  1", "… +2994 lines (ctrl+o to expand)", "3000"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "\n     1500\n") {
		t.Fatalf("the whole output was printed:\n%s", all)
	}
	r.send("\x0f")
	r.waitScreen("Transcript")
	r.send("g")
	r.waitScreen("please big")
	for i := 0; i < 20; i++ {
		r.send("\x1b[6~")
	}
	r.waitFor("the middle of the output in the transcript view", false, func(s string) bool {
		return strings.Contains(s, "\n     4")
	})
	r.send("q")
	r.waitFor("the session back", false, func(s string) bool { return !strings.Contains(s, "Transcript ·") })
}

// A failing command shows its exit and its output, not "exit 1" alone.
func TestTUIFailureIsReadable(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 80, 30, "-allow", "bash(ls *)")
	r.send("please fail\r")
	r.waitText("Done.")
	r.quiet(300 * time.Millisecond)
	all := r.term.All()
	if !strings.Contains(all, "⎿  Exit ") || !strings.Contains(all, "No such file or directory") {
		t.Fatalf("the failure is not readable:\n%s", all)
	}
}
