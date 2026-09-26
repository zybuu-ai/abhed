package ui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// drive feeds keystrokes to an editor and returns the line it produced plus
// everything it painted.
func drive(t *testing.T, keys string) (string, string) {
	t.Helper()
	var out bytes.Buffer
	e := newEditor(strings.NewReader(keys), &out, "> ")
	line, err := e.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	return line, out.String()
}

// TestBackspaceClosesTheMenu pins the bug that prompted this editor.
//
// x/term's AutoCompleteCallback never sees Backspace, and the line it passes
// is the one BEFORE the key was applied, so erasing a slash command left its
// menu on screen with nothing to select.
func TestBackspaceClosesTheMenu(t *testing.T) {
	// "/co", then three backspaces to empty, then Enter.
	line, painted := drive(t, "/co\x7f\x7f\x7f\r")
	if line != "" {
		t.Fatalf("line should be empty, got %q", line)
	}
	// The final paint must not contain a command row.
	last := painted[strings.LastIndex(painted, "\r\033[J"):]
	for _, c := range []string{"/compact", "/cost"} {
		if strings.Contains(last, c) {
			t.Errorf("menu still shown after erasing the line: %q appears", c)
		}
	}
}

// TestArrowKeysMoveThroughTheMenu pins the second bug: arrows were consumed by
// x/term for history and never reached the suggestions.
func TestArrowKeysMoveThroughTheMenu(t *testing.T) {
	matches := MatchCommands("/c")
	if len(matches) < 1 {
		t.Skip("no /c commands")
	}
	// One Down highlights the first candidate; Enter takes it.
	line, _ := drive(t, "/c\x1b[B\r")
	want := matches[0].Name
	if !strings.HasPrefix(line, want) {
		t.Errorf("Down then Enter gave %q, want the first candidate %q", line, want)
	}
}

// TestChoosingACommandThatTakesAnArgumentWaits: Enter on such a command fills
// the line and stops, because submitting "/mode" with no mode is not what
// choosing it meant. A command with no argument submits immediately.
func TestChoosingACommandThatTakesAnArgumentWaits(t *testing.T) {
	// /compact takes [hint]: Enter fills it, then "now" and Enter submit.
	line, _ := drive(t, "/comp\x1b[B\rnow\r")
	if line != "/compact now" {
		t.Errorf("got %q, want %q", line, "/compact now")
	}
	// /cwd takes nothing: one Enter is the whole gesture.
	line2, _ := drive(t, "/cwd\x1b[B\r")
	if strings.TrimSpace(line2) != "/cwd" {
		t.Errorf("got %q, want /cwd", line2)
	}
}

// TestSelectionWrapsAndWrapsBack keeps the list navigable from either end.
func TestSelectionWrapsAndWrapsBack(t *testing.T) {
	// "/c", Up once — selects the LAST candidate, not nothing.
	line, _ := drive(t, "/c\x1b[A\r")
	matches := MatchCommands("/c")
	want := matches[len(matches)-1].Name
	if !strings.HasPrefix(line, want) {
		t.Errorf("Up from no selection gave %q, want the last candidate %q", line, want)
	}
}

// TestTabCompletesToTheSharedStem keeps Tab useful when the choice is
// ambiguous rather than doing nothing.
func TestTabCompletesToTheSharedStem(t *testing.T) {
	line, _ := drive(t, "/co\t\r")
	if line != "/co" && !strings.HasPrefix(line, "/co") {
		t.Errorf("Tab on an ambiguous stem gave %q", line)
	}
	// Unambiguous: completes the whole command.
	line2, _ := drive(t, "/quit\t\r")
	if !strings.HasPrefix(line2, "/quit") {
		t.Errorf("Tab on a unique match gave %q, want /quit", line2)
	}
}

// TestOrdinaryTextNeverOpensTheMenu: a slash in a sentence or a path is not a
// command.
func TestOrdinaryTextNeverOpensTheMenu(t *testing.T) {
	_, painted := drive(t, "ls /tmp\r")
	if strings.Contains(painted, "/compact") {
		t.Error("a path opened the command menu")
	}
}

// TestEditingKeysBehave covers the ordinary line editing the prompt needs.
func TestEditingKeysBehave(t *testing.T) {
	// "abc", Left, "X" -> "abXc"
	if line, _ := drive(t, "abc\x1b[DX\r"); line != "abXc" {
		t.Errorf("Left then insert gave %q, want abXc", line)
	}
	// Ctrl-A then "X" -> prepends
	if line, _ := drive(t, "abc\x01X\r"); line != "Xabc" {
		t.Errorf("Ctrl-A then insert gave %q, want Xabc", line)
	}
	// Ctrl-U clears to start
	if line, _ := drive(t, "abc\x15z\r"); line != "z" {
		t.Errorf("Ctrl-U gave %q, want z", line)
	}
	// Ctrl-W deletes a word
	if line, _ := drive(t, "go test ./...\x17\r"); line != "go test " {
		t.Errorf("Ctrl-W gave %q, want 'go test '", line)
	}
}

// TestQuietSuppressesThePrompt pins the stacked-glyph bug.
//
// The reader goroutine keeps reading during a turn so a steering message can
// be typed. It must not paint a prompt while doing so: every write during a
// turn repainted one, which stacked a column of prompt glyphs under each
// answer and painted over the thinking indicator.
func TestQuietSuppressesThePrompt(t *testing.T) {
	var out bytes.Buffer
	e := newEditor(strings.NewReader(""), &out, "PROMPT> ")
	e.reading = true

	e.setQuiet(true)
	out.Reset()
	e.redraw()
	if strings.Contains(out.String(), "PROMPT>") {
		t.Error("the prompt was painted while a turn owns the screen")
	}

	e.setQuiet(false)
	if !strings.Contains(out.String(), "PROMPT>") {
		t.Error("the prompt did not come back when the turn finished")
	}
}

// TestWritePassesThroughOutsideAnEdit: while no line is being edited the
// editor must not erase and repaint anything, or it overwrites the output it
// was asked to print.
func TestWritePassesThroughOutsideAnEdit(t *testing.T) {
	var out bytes.Buffer
	e := newEditor(strings.NewReader(""), &out, "> ")
	e.reading = false // no line in progress, as during a turn

	if _, err := e.write([]byte("spinner frame")); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "\033[J") {
		t.Error("write erased the screen while no line was being edited")
	}
	if !strings.Contains(got, "spinner frame") {
		t.Error("write dropped its output")
	}
}

// timedKeys feeds bytes one at a time on a fake clock, moving it forward by
// each key's delay and firing any timer that falls due, as time would.
type timedKeys struct {
	mu     sync.Mutex
	now    time.Time
	keys   []timedKey
	timers []fakeTimer
	start  chan struct{} // when set, the first Read waits for it to close
}

type timedKey struct {
	after time.Duration
	b     byte
}

type fakeTimer struct {
	at time.Time
	f  func()
}

func (t *timedKeys) clock() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.now
}

func (t *timedKeys) afterFunc(d time.Duration, f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.timers = append(t.timers, fakeTimer{t.now.Add(d), f})
}

func (t *timedKeys) advance(d time.Duration) {
	t.mu.Lock()
	t.now = t.now.Add(d)
	var due, left []fakeTimer
	for _, tm := range t.timers {
		if !tm.at.After(t.now) {
			due = append(due, tm)
		} else {
			left = append(left, tm)
		}
	}
	t.timers = left
	t.mu.Unlock()
	for _, tm := range due {
		tm.f()
	}
}

func (t *timedKeys) Read(p []byte) (int, error) {
	if t.start != nil {
		<-t.start
		t.start = nil
	}
	if len(t.keys) == 0 {
		t.advance(10 * time.Second) // silence after the last key
		return 0, io.EOF
	}
	k := t.keys[0]
	t.keys = t.keys[1:]
	t.advance(k.after)
	p[0] = k.b
	return 1, nil
}

// typed spaces the bytes of s every gap, the first one after first.
func typed(first, gap time.Duration, s string) []timedKey {
	var out []timedKey
	for i := 0; i < len(s); i++ {
		d := gap
		if i == 0 {
			d = first
		}
		out = append(out, timedKey{d, s[i]})
	}
	return out
}

// approving arms an approval, reads lines until input ends, and returns the
// lines submitted, the runes the approval received and the ending error.
func approving(t *testing.T, keys ...[]timedKey) ([]string, []rune, error) {
	t.Helper()
	in := &timedKeys{now: time.Unix(1000, 0)}
	for _, k := range keys {
		in.keys = append(in.keys, k...)
	}
	e := newEditor(in, io.Discard, "> ")
	e.now = in.clock
	e.after = in.afterFunc
	e.quiet = true // a turn is running, as it is whenever an approval shows
	ch := e.beginApproval()
	e.armApproval()
	var lines []string
	var err error
	for {
		var line string
		line, err = e.readLine()
		if err != nil {
			break
		}
		lines = append(lines, line)
	}
	e.endApproval()
	var got []rune
	for {
		select {
		case k := <-ch:
			got = append(got, k)
		default:
			return lines, got, err
		}
	}
}

// answers keeps only the decision keys, dropping notices and Enter.
func answers(rs []rune) string {
	var b strings.Builder
	for _, r := range rs {
		if isDecisionKey(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Typing a sentence while an approval shows must not answer it, even when the
// sentence starts with a decision key: "Actually no" once sent A, always-allow.
func TestApprovalTypingIsNotAnAnswer(t *testing.T) {
	for _, c := range []struct {
		name  string
		keys  []timedKey
		lines []string
	}{
		{"typed as the prompt appears", typed(50*time.Millisecond, 90*time.Millisecond, "Wait, stop\r"), []string{"Wait, stop"}},
		{"starts with A", typed(time.Second, 80*time.Millisecond, "Actually no\r"), []string{"Actually no"}},
		{"starts with y", typed(time.Second, 80*time.Millisecond, "yes but not that\r"), []string{"yes but not that"}},
		{"starts with a", typed(time.Second, 80*time.Millisecond, "also this\r"), []string{"also this"}},
		{"key autorepeat", typed(time.Second, 30*time.Millisecond, "aaaa"), nil},
		{"a line, then a key while still typing", typed(time.Second, 80*time.Millisecond, "stop\ra"), []string{"stop"}},
	} {
		lines, keys, err := approving(t, c.keys)
		if !errors.Is(err, io.EOF) {
			t.Errorf("%s: err = %v", c.name, err)
		}
		if a := answers(keys); a != "" {
			t.Errorf("%s: answered %q", c.name, a)
		}
		if strings.Join(lines, "|") != strings.Join(c.lines, "|") {
			t.Errorf("%s: steering lines %q, want %q", c.name, lines, c.lines)
		}
		if len(keys) == 0 || keys[0] != approvalHeld {
			t.Errorf("%s: the held notice was not sent: %q", c.name, string(keys))
		}
	}
}

// Only a decision key alone, after a pause and followed by quiet, answers; not
// one too soon, in a burst or on a line holding text, nor Enter alone.
func TestApprovalNeedsADeliberateKey(t *testing.T) {
	ctrlU := string(rune(keyCtrlU))
	cases := []struct {
		name string
		keys []timedKey
		want string
	}{
		{"lone key after a pause", typed(time.Second, 0, "a"), "a"},
		{"lone reject", typed(time.Second, 0, "r"), "r"},
		{"always", typed(time.Second, 0, "A"), "A"},
		{"always needs a longer quiet", append(typed(time.Second, 0, "A"), timedKey{400 * time.Millisecond, 'x'}), ""},
		{"a shorter quiet is enough for a", append(typed(time.Second, 0, "a"), timedKey{400 * time.Millisecond, 'x'}), "a"},
		{"a key then Enter", typed(time.Second, 100*time.Millisecond, "a\r"), ""},
		{"Enter alone never answers", typed(time.Second, time.Second, "\r\r"), ""},
		{"Enter, then a key", typed(time.Second, time.Second, "\ry"), "y"},
		{"too soon after the prompt", typed(100*time.Millisecond, 0, "a"), ""},
		{"type-ahead burst", typed(0, 0, "yyy"), ""},
		{"key inside a burst", typed(time.Second, 50*time.Millisecond, "xa"), ""},
		{"key after a pause on a line with text", typed(time.Second, 400*time.Millisecond, "Wa"), ""},
		{"cleared line, key too soon", append(typed(time.Second, 80*time.Millisecond, "xyz"+ctrlU), timedKey{50 * time.Millisecond, 'a'}), ""},
		{"cleared line, key after a pause", append(typed(time.Second, 80*time.Millisecond, "xyz"+ctrlU), timedKey{time.Second, 'a'}), "a"},
		{"arrow keys", typed(time.Second, 0, "\x1b[A"), ""},
	}
	for _, c := range cases {
		_, keys, err := approving(t, c.keys)
		if !errors.Is(err, io.EOF) {
			t.Errorf("%s: err = %v, want EOF", c.name, err)
		}
		if got := answers(keys); got != c.want {
			t.Errorf("%s: approval got %q, want %q", c.name, got, c.want)
		}
	}
}

// Enter alone, and a decision key on a line holding text, each tell the
// approval so the prompt can say why nothing happened.
func TestApprovalExplainsIgnoredKeys(t *testing.T) {
	_, keys, _ := approving(t, typed(time.Second, 0, "\r"))
	if string(keys) != "\r" {
		t.Errorf("Enter alone: approval got %q, want the re-show signal", string(keys))
	}
	_, keys, _ = approving(t, typed(time.Second, 500*time.Millisecond, "xa"))
	if !strings.ContainsRune(string(keys), approvalBusy) {
		t.Errorf("key on a busy line: approval got %q, want the busy notice", string(keys))
	}
}

// Nothing answers before the choices are drawn, however long the wait.
func TestApprovalWaitsUntilArmed(t *testing.T) {
	in := &timedKeys{now: time.Unix(1000, 0), keys: typed(time.Second, 0, "a")}
	e := newEditor(in, io.Discard, "> ")
	e.now = in.clock
	e.after = in.afterFunc
	e.quiet = true
	ch := e.beginApproval()
	_, _ = e.readLine()
	select {
	case k := <-ch:
		if isDecisionKey(k) {
			t.Errorf("answered %q before the choices were drawn", k)
		}
	default:
	}
}

// Ctrl-C at an approval ends the read as an interrupt, so the session can
// stop the turn. It used to be swallowed as not being a decision letter.
func TestApprovalCtrlCInterrupts(t *testing.T) {
	_, keys, err := approving(t, typed(time.Second, 0, "\x03"))
	if !ErrInterrupted(err) {
		t.Fatalf("err = %v, want an interrupt", err)
	}
	if len(keys) != 0 {
		t.Errorf("Ctrl-C answered the approval: %q", string(keys))
	}
}

// A key then Enter re-shows the choices, and sends nothing as steering.
func TestApprovalKeyThenEnterIsNotSteering(t *testing.T) {
	lines, keys, _ := approving(t, typed(time.Second, 100*time.Millisecond, "a\r"))
	if len(lines) != 0 {
		t.Errorf("sent %q as steering", lines)
	}
	if !strings.ContainsRune(string(keys), keyEnter) {
		t.Errorf("the choices were not re-shown: %q", string(keys))
	}
}

// The held notice is sent once however much is typed.
func TestApprovalHeldNoticeOnce(t *testing.T) {
	_, keys, _ := approving(t, typed(time.Second, 80*time.Millisecond, "Wait, stop"))
	if n := strings.Count(string(keys), string(approvalHeld)); n != 1 {
		t.Errorf("held notice sent %d times, want 1", n)
	}
}

// A timer left from an earlier approval cannot answer a later one, and ending
// an approval drops a held key.
func TestApprovalStaleTimer(t *testing.T) {
	var fire []func()
	e := newEditor(strings.NewReader(""), io.Discard, "> ")
	e.after = func(_ time.Duration, f func()) { fire = append(fire, f) }
	first := e.beginApproval()
	e.hold(e.approveCh, 'a')
	e.endApproval()
	if e.pending != 0 {
		t.Errorf("endApproval kept the held key %q", e.pending)
	}
	second := e.beginApproval()
	e.approveMu.Lock()
	e.pending = 'y' // a key held for the new approval, by no timer of its own
	e.approveMu.Unlock()
	for _, f := range fire {
		f()
	}
	for name, ch := range map[string]<-chan rune{"first": first, "second": second} {
		select {
		case k := <-ch:
			t.Errorf("%s approval answered %q by a stale timer", name, k)
		default:
		}
	}
}

// The line reader arms the guard when the approver waits for a key, so a lone
// key after a pause answers through it.
func TestLineReaderApprovalKeysArm(t *testing.T) {
	in := &timedKeys{now: time.Unix(1000, 0), keys: typed(time.Second, 0, "a"), start: make(chan struct{})}
	e := newEditor(in, io.Discard, "> ")
	e.now = in.clock
	e.after = in.afterFunc
	e.quiet = true
	l := &LineReader{ed: e, raw: true}
	read, end := l.ApprovalKeys(context.Background())
	defer end()
	go func() { _, _ = e.readLine() }()
	got := make(chan string, 1)
	go func() {
		k, _ := read()
		got <- k
	}()
	for deadline := time.Now().Add(2 * time.Second); ; {
		if _, armed := e.approvalChan(); !armed.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("read did not arm the approval")
		}
		time.Sleep(time.Millisecond)
	}
	close(in.start)
	select {
	case k := <-got:
		if k != "a" {
			t.Errorf("answer %q, want a", k)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no answer through the line reader")
	}
}
