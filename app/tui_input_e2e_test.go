//go:build unix

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Non-ASCII reaches the model intact, not one garbage character per byte.
func TestTUIUnicodeReachesTheModelIntact(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 100, 30)
	want := "héllo wörld 你好 — “quotes” 🙂"
	r.send(want + "\r")
	r.waitText("You said: " + want)
	if got := stub.prompt(); got != want {
		t.Fatalf("the model got %q, want %q", got, want)
	}
}

// At 40 columns a long prompt wraps in place: it is not printed again on
// every key, copy after copy down the screen.
func TestTUILongPromptAtFortyColumns(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 40, 20)
	text := "type a fairly long line of input text " + strings.Repeat("more words ", 15) + "END"
	r.typed(text)
	r.quiet(100 * time.Millisecond)
	before := r.capture("200-character prompt at 40 columns")
	if n := strings.Count(r.term.All(), "type a fairly"); n != 1 {
		t.Fatalf("the prompt appears %d times:\n%s", n, before)
	}
	t.Log(before)
	r.send("\r")
	r.waitText("Hello from the stub")
	if got := stub.prompt(); got != text {
		t.Fatalf("the model got %q", got)
	}
}

// A 25-line paste is one prompt, bracketed or not: never one prompt plus a
// steering message per line.
func TestTUIPasteIsOnePrompt(t *testing.T) {
	var lines []string
	for i := 1; i <= 25; i++ {
		lines = append(lines, "pasted line "+strings.Repeat("x", i%7+1))
	}
	for _, c := range []struct{ name, open, close, sep string }{
		{"bracketed", "\x1b[200~", "\x1b[201~", "\r\n"},
		{"raw", "", "", "\r"},
	} {
		t.Run(c.name, func(t *testing.T) {
			stub, ws := tuiWorkspace(t, "")
			r := startTUI(t, stub, ws, 100, 30)
			r.markBytes()
			r.send(c.open + strings.Join(lines, c.sep) + c.close)
			r.waitScreen("[Pasted text #1 +25 lines]")
			if n := r.bytesSinceMark(); n > 20_000 {
				t.Errorf("the paste took %d bytes to draw, budget 20 KB", n)
			}
			t.Log(r.capture(c.name + " paste, before sending"))
			time.Sleep(50 * time.Millisecond)
			r.send("\r")
			r.waitText("Hello from the stub")
			r.quiet(300 * time.Millisecond)
			if got := stub.prompt(); got != strings.Join(lines, "\n") {
				t.Fatalf("the model got %q", got)
			}
			if strings.Contains(r.term.All(), "steering") {
				t.Fatalf("the paste was split into steering messages:\n%s", r.term.All())
			}
			stub.mu.Lock()
			n := stub.requests
			stub.mu.Unlock()
			if n != 1 {
				t.Fatalf("%d model requests, want 1", n)
			}
		})
	}
}

// Esc interrupts and does not eat the next key: "/cost" after it must not
// reach the model as "cost".
func TestTUIEscInterruptsAndTheNextKeyCounts(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 100, 30)
	r.send("please slow\r")
	for deadline := time.Now().Add(5 * time.Second); stub.firstTextAt().IsZero(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the stub never started its reply")
		}
	}
	time.Sleep(100 * time.Millisecond)
	r.send("\x1b")
	r.waitScreen("● default mode") // the turn ended: the dock is idle again
	r.waitFor("the turn to stop", false, func(s string) bool { return !strings.Contains(s, "esc to interrupt") })
	time.Sleep(100 * time.Millisecond)
	r.send("/cost\r")
	r.waitFor("/cost to run", true, func(s string) bool {
		return strings.Contains(s, "tokens in") || strings.Contains(s, "no usage yet")
	})
	if p := stub.prompt(); p == "cost" || p == "ost" {
		t.Fatalf("the model was sent %q", p)
	}
	if strings.Contains(r.term.All(), "word59") {
		t.Fatalf("the reply ran to the end despite Esc:\n%s", r.term.All())
	}
}

// Typing during a turn is visible.
func TestTUITypingDuringATurnIsVisible(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 100, 30)
	r.send("please slow\r")
	r.waitText("word2")
	r.typed("use the other file")
	r.waitScreen("use the other file")
	t.Log(r.capture("typing while the agent streams"))
}

// The keystroke budget at an idle prompt, through the whole program.
func TestTUIKeystrokeBudget(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 100, 30)
	r.send("x")
	r.quiet(150 * time.Millisecond)
	worst, total := 0, 0
	const text = "abcdefghijklmnopqrst"
	for _, c := range text {
		r.markBytes()
		r.send(string(c))
		r.drawn(40 * time.Millisecond)
		n := r.bytesSinceMark()
		worst = max(worst, n)
		total += n
	}
	t.Logf("idle typing: %d bytes over %d keys, worst %d", total, len(text), worst)
	if worst > 64 {
		t.Fatalf("a key cost %d bytes; budget 64: %q", worst, r.rawSinceMark())
	}
}

// Esc ends the turn and keeps background shells; Ctrl-C stops them too. The
// end says which, on screen and in session.ended's detail.
func TestTUIEscAndCtrlCSayWhatTheyLeft(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 120, 30)
	for i, tc := range []struct{ key, want string }{
		{"\x1b", "background shells kept"},
		{"\x03", "background shells stopped"},
	} {
		r.send("please slow\r")
		r.waitFor("the reply to start", true, func(s string) bool { return strings.Count(s, "word2") > i })
		r.send(tc.key)
		r.waitText("Interrupted · " + tc.want)
		r.waitFor("the turn to stop", false, func(s string) bool { return !strings.Contains(s, "esc to interrupt") })
		time.Sleep(200 * time.Millisecond)
	}
}

// Ctrl-B on an empty line during a turn moves the running command to the
// background, and the turn goes on; with something typed it is still the
// cursor key.
func TestTUICtrlBMovesACommandToTheBackground(t *testing.T) {
	stub, ws := tuiWorkspace(t, `,"permissions":{"allow":["bash(*)"]}`)
	r := startTUI(t, stub, ws, 120, 30)
	r.send("please sleep\r")
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(ws, "started.txt")); err == nil {
			break // the command has started
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command never started:\n%s", r.term.All())
		}
	}
	r.send("ab\x02c") // typed: Ctrl-B moves the cursor
	r.waitFor("the cursor move", false, func(s string) bool { return strings.Contains(s, "acb") })
	r.send("\x05\x15") // to the end, then clear the line
	time.Sleep(100 * time.Millisecond)
	r.send("\x02")
	// The call's line says the command went on in the background.
	r.waitFor("the call to say it moved", true, func(s string) bool {
		return strings.Contains(strings.ToLower(s), "started in background: sh_")
	})
	r.waitFor("the turn to end", false, func(s string) bool { return !strings.Contains(s, "esc to interrupt") })
}
