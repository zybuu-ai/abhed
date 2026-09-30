//go:build unix

package app

import (
	"strings"
	"testing"
	"time"
)

// A reply drawn only at each newline shows a paragraph as a blank screen
// for as long as it takes to stream. Each fragment is on screen as it
// arrives: while the stub is still writing, what it has written is visible.
func TestTUIStreamsTokenByToken(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 100, 30)
	r.send("please slow\r")
	r.waitScreen("word5")
	mid := r.capture("mid-stream, word5 of 60")
	if strings.Contains(r.term.All(), "word59") {
		t.Fatalf("the reply was only drawn once complete:\n%s", mid)
	}
	t.Log(mid)
	// Steady state: the bytes between word 10 and word 50 on screen, less
	// the words themselves, per second.
	r.waitScreen("word10")
	r.markBytes()
	start := time.Now()
	r.waitText("word50")
	elapsed := time.Since(start)
	text := 0
	for i := 11; i <= 50; i++ {
		text += len(" word") + len(strings.TrimSpace(strings.Repeat(" ", 0))) + 2
	}
	over := float64(r.bytesSinceMark()-text) / elapsed.Seconds()
	t.Logf("streaming overhead: %.0f B/s (%d bytes in %v)", over, r.bytesSinceMark(), elapsed)
	if over > 2048 {
		t.Errorf("streaming overhead %.0f B/s, budget 2 KB/s", over)
	}
	r.waitText("word59")
}

// The first fragment is on screen within 50 ms of the model sending it
// (the latency budget), measured from the stub's write to the pty's read.
func TestTUIFirstTokenLatency(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 100, 30)
	var worst time.Duration
	for i := 0; i < 5; i++ {
		stub.mu.Lock()
		stub.firstText = time.Time{}
		stub.mu.Unlock()
		marker := "Hello from the stub. You said: probe" + string(rune('a'+i))
		r.markBytes()
		r.send("probe" + string(rune('a'+i)) + "\r")
		r.waitText(marker)
		sent := stub.firstTextAt()
		seen := r.seenAfterMark("Hello")
		lat := seen.Sub(sent)
		t.Logf("reply %d: first fragment on screen %v after the model sent it", i+1, lat)
		worst = max(worst, lat)
		r.quiet(100 * time.Millisecond)
	}
	if worst > 50*time.Millisecond {
		t.Fatalf("first fragment took %v to reach the screen; budget 50 ms", worst)
	}
}

// The activity line goes the moment the reply starts and leaves nothing
// behind: no spinner frame or verb in the transcript.
func TestTUISpinnerLeavesNoFrames(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 100, 30)
	r.send("please slow\r")
	r.waitScreen("esc to interrupt")
	r.waitText("word59")
	r.quiet(400 * time.Millisecond)
	all := r.term.All()
	for _, f := range []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏", "Thinking…", "Working…"} {
		if strings.Contains(all, f) {
			t.Fatalf("a spinner frame %q was left behind:\n%s", f, all)
		}
	}
	if strings.Contains(all, " in / ") {
		t.Fatalf("the per-reply usage line is still printed:\n%s", all)
	}
}

// Markdown in a streamed reply renders as markdown: a fence is a block,
// headings keep their case, and prose wraps between words rather than being
// cut mid-word by the terminal.
func TestTUIStreamedMarkdown(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 80, 40)
	r.send("please long\r")
	r.waitText("commodo consequat.")
	r.quiet(300 * time.Millisecond)
	all := r.term.All()
	t.Log(r.capture("markdown at 80 columns"))
	for _, bad := range []string{"SUMMARY OF CHANGES", "# Summary", "**", "```", "|------", "| hello.txt"} {
		if strings.Contains(all, bad) {
			t.Errorf("markup %q survived:\n%s", bad, all)
		}
	}
	for _, good := range []string{"Summary of changes", "what I found", "• nested bullet one", "1. The greeting is fine.",
		"package main", "│ A blockquote line.", "the docs (https://example.com/docs)"} {
		if !strings.Contains(all, good) {
			t.Errorf("missing %q:\n%s", good, all)
		}
	}
	// A table's columns line up.
	var okRow, workRow string
	for _, l := range strings.Split(all, "\n") {
		if strings.Contains(l, "hello.txt") && strings.Contains(l, "ok") {
			okRow = l
		}
		if strings.Contains(l, "main.go") && strings.Contains(l, "needs work") {
			workRow = l
		}
	}
	if okRow == "" || workRow == "" || strings.Index(okRow, "ok") != strings.Index(workRow, "needs work") {
		t.Errorf("table columns do not line up:\n%q\n%q", okRow, workRow)
	}
	// No word of the prose is cut across rows.
	seen := map[string]bool{}
	for _, w := range strings.Fields(all) {
		seen[w] = true
	}
	for _, w := range strings.Fields("Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua.") {
		if !seen[w] {
			t.Errorf("the word %q was broken across rows", w)
		}
	}
}

// A fence split across fragments is still a block, and a line inside it
// that looks like a heading is code.
func TestTUIFenceAcrossFragments(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 80, 30)
	r.send("please code\r")
	r.waitText("After the code.")
	r.quiet(200 * time.Millisecond)
	all := r.term.All()
	if !strings.Contains(all, "# not a heading") || strings.Contains(all, "```") || strings.Contains(all, "``") {
		t.Fatalf("the fence was not a block:\n%s", all)
	}
	if !strings.Contains(all, `fmt.Println("hi") // done`) {
		t.Fatalf("code line lost:\n%s", all)
	}
}

// Reasoning is one line in the transcript; Ctrl-O shows it whole, on the
// alternate screen, and closing it puts the session back.
func TestTUIReasoningCollapsesAndExpands(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 100, 30)
	r.send("please think\r")
	r.waitText("The answer is 42.")
	r.waitText("✻ Thought · 120 words · ctrl+o to expand")
	if strings.Contains(r.term.All(), "consider the question carefully") {
		t.Fatalf("the reasoning was printed in full:\n%s", r.term.All())
	}
	r.quiet(200 * time.Millisecond)
	r.send("\x0f")
	r.waitScreen("Transcript")
	r.waitScreen("consider the question carefully")
	r.send("q")
	r.waitFor("the session back", false, func(s string) bool {
		return !strings.Contains(s, "Transcript ·") && strings.Contains(s, "The answer is 42.")
	})
}
