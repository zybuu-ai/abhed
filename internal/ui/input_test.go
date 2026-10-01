package ui

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Non-ASCII arrived at the model as one garbage character per byte:
// "héllo wörld 你好" became "hÃ©llo wÃ¶rld ä½å¥½".
func TestUnicodeRoundTrips(t *testing.T) {
	g := newRig(t, 80, 24)
	want := "héllo wörld 你好 — “quotes” 🙂 👩\u200d👩\u200d👧 🇮🇳 e\u0301"
	g.keys(want + "\r")
	got, err := g.line()
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
}

// Backspace removes what a person sees as one character: a family emoji,
// a flag, a letter with a combining accent.
func TestBackspaceRemovesWholeCharacters(t *testing.T) {
	for in, want := range map[string]string{
		"café\x7f":             "caf",
		"a👩\u200d👩\u200d👧\x7f": "a",
		"x🇮🇳\x7f":              "x",
		"ne\u0301\x7f":         "n",
		"日本\x7f":               "日",
	} {
		g := newRig(t, 80, 24)
		g.keys(in)
		g.settle()
		g.keys("\r")
		if got, _ := g.line(); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// The cursor sits after wide characters, not in the middle of them.
func TestCursorAfterWideCharacters(t *testing.T) {
	g := newRig(t, 80, 24)
	g.keys("你好")
	g.settle()
	x, y := g.term.Cursor()
	row := g.term.Lines()[y]
	if !strings.Contains(row, "你好") {
		t.Fatalf("cursor row %d is %q:\n%s", y, row, g.term.Dump())
	}
	if want := displayWidth(g.lr.d.prompt) + 4; x != want {
		t.Errorf("cursor at column %d, want %d:\n%s", x, want, g.term.Dump())
	}
}

// Every way to start a new line without submitting.
func TestMultilineInput(t *testing.T) {
	for name, nl := range map[string]string{
		"shift-enter csi-u": "\x1b[13;2u",
		"shift-enter xterm": "\x1b[27;2;13~",
		"alt-enter":         "\x1b\r",
		"ctrl-j":            "\n",
	} {
		g := newRig(t, 80, 24)
		g.keys("one")
		g.keys(nl)
		g.keys("two")
		g.settle()
		g.keys("\r")
		if got, _ := g.line(); got != "one\ntwo" {
			t.Errorf("%s: got %q", name, got)
		}
	}
	// A backslash then Enter, which works in any terminal.
	g := newRig(t, 80, 24)
	g.keys("one\\")
	g.settle()
	g.keys("\r")
	g.settle()
	g.keys("two")
	g.settle()
	g.keys("\r")
	if got, _ := g.line(); got != "one\ntwo" {
		t.Errorf("backslash: got %q", got)
	}
}

// A bracketed paste is one prompt, shown as a placeholder, sent whole:
// never one prompt followed by a steering message per line.
func TestBracketedPasteIsOnePrompt(t *testing.T) {
	g := newRig(t, 80, 24)
	var lines []string
	for i := 1; i <= 25; i++ {
		lines = append(lines, "pasted line "+strings.Repeat("x", i))
	}
	body := strings.Join(lines, "\r\n")
	g.keys("see: \x1b[200~" + body + "\x1b[201~")
	g.waitText("[Pasted text #1 +25 lines]")
	g.keys("\r")
	got, err := g.line()
	if err != nil || got != "see: "+strings.Join(lines, "\n") {
		t.Fatalf("got %q, %v", got, err)
	}
	select {
	case r := <-g.lr.d.results:
		t.Fatalf("a second line was submitted: %q", r.line)
	case <-time.After(100 * time.Millisecond):
	}
}

// A terminal that does not bracket pastes sends the text as typing, Enters
// and all, within a millisecond. It is still one prompt.
func TestRawPasteIsOnePrompt(t *testing.T) {
	g := newRig(t, 80, 24)
	var lines []string
	for i := 1; i <= 25; i++ {
		lines = append(lines, "raw line "+strings.Repeat("y", i))
	}
	g.keys(strings.Join(lines, "\r"))
	g.waitText("[Pasted text #1 +25 lines]")
	g.out.mark()
	g.keys("\r")
	got, _ := g.line()
	if got != strings.Join(lines, "\n") {
		t.Fatalf("got %q", got)
	}
	select {
	case r := <-g.lr.d.results:
		t.Fatalf("the paste was shredded: a second line %q", r.line)
	case <-time.After(100 * time.Millisecond):
	}
}

// Tab after a placeholder opens the paste for editing.
func TestTabOpensAPaste(t *testing.T) {
	g := newRig(t, 80, 24)
	g.keys("\x1b[200~a\nb\nc\nd\x1b[201~")
	g.waitText("[Pasted text #1 +4 lines]")
	g.keys("\t")
	g.settle()
	if strings.Contains(g.term.Text(), "[Pasted text") {
		t.Fatalf("placeholder still shown:\n%s", g.term.Dump())
	}
	g.keys("\x7f")
	g.settle()
	g.keys("\r")
	if got, _ := g.line(); got != "a\nb\nc\n" {
		t.Fatalf("got %q", got)
	}
}

// History is kept per workspace, on disk, readable only by its owner, and
// Up recalls it in the next session.
func TestHistoryPersistsPerWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := HistoryPath("/some/workspace")
	if !strings.HasPrefix(path, filepath.Join(home, ".abhed", "history")) {
		t.Fatalf("history at %s", path)
	}
	if HistoryPath("/other") == path {
		t.Fatal("two workspaces share a history")
	}
	g := newRig(t, 80, 24)
	g.lr.SetHistory(LoadHistory(path))
	g.keys("first prompt\r")
	g.line()
	g.keys("second\nline\r")
	g.line()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("history mode %v", info.Mode().Perm())
	}

	g2 := newRig(t, 80, 24)
	g2.lr.SetHistory(LoadHistory(path))
	// Up recalls the two-line entry; Up again moves within it, to its
	// first line; the third reaches the entry before.
	for i := 0; i < 3; i++ {
		g2.keys("\x1b[A")
		g2.settle()
	}
	g2.keys("\r")
	if got, _ := g2.line(); got != "first prompt" {
		t.Fatalf("Up ×3 recalled %q", got)
	}
}

// Ctrl-R finds an older prompt by what it contains.
func TestReverseSearch(t *testing.T) {
	g := newRig(t, 80, 24)
	h := &History{}
	h.Add("build the docs", false)
	h.Add("run the tests", false)
	h.Add("deploy", false)
	g.lr.SetHistory(h)
	g.keys("\x12")
	g.typed("test")
	g.waitText("run the tests")
	g.keys("\r") // take the match onto the line
	g.settle()
	g.keys("\r")
	if got, _ := g.line(); got != "run the tests" {
		t.Fatalf("got %q", got)
	}
}

// Esc stops a running turn, and the key after it is its own key: "/cost"
// after Esc must not reach the model as "cost".
func TestEscInterruptsAndNeverEatsTheNextKey(t *testing.T) {
	g := newRig(t, 80, 24)
	g.lr.Quiet(true)
	g.keys("\x1b")
	select {
	case <-g.lr.Stops():
	case <-time.After(2 * time.Second):
		t.Fatal("Esc did not stop the turn")
	}
	time.Sleep(20 * time.Millisecond)
	g.keys("/cost\r")
	if got, _ := g.line(); got != "/cost" {
		t.Fatalf("after Esc the line was %q", got)
	}
	// At an idle prompt, too: Esc then a letter is not Alt+letter.
	g.lr.Quiet(false)
	g.keys("\x1b")
	time.Sleep(60 * time.Millisecond)
	g.keys("please slow\r")
	if got, _ := g.line(); got != "please slow" {
		t.Fatalf("after Esc the line was %q", got)
	}
}

// Ctrl-C clears a typed line; on an empty line it stops a turn; at an idle
// empty prompt it warns, and a second press exits.
func TestCtrlC(t *testing.T) {
	g := newRig(t, 80, 24)
	g.keys("half a thought")
	g.settle()
	g.keys("\x03")
	g.settle()
	if g.lr.Typing() {
		t.Fatal("Ctrl-C left the line")
	}
	select {
	case r := <-g.lr.d.results:
		t.Fatalf("Ctrl-C on a typed line reached the session: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	// Cleared, not lost.
	g.keys("\x1b[A")
	g.settle()
	g.keys("\r")
	if got, _ := g.line(); got != "half a thought" {
		t.Fatalf("Up after Ctrl-C gave %q", got)
	}

	g.lr.Quiet(true)
	g.keys("\x03")
	if _, err := g.line(); !ErrInterrupted(err) {
		t.Fatalf("Ctrl-C in a turn: %v", err)
	}
	g.lr.Quiet(false)

	g.keys("\x03")
	if _, err := g.line(); !ErrInterrupted(err) {
		t.Fatalf("Ctrl-C at the prompt: %v", err)
	}
	g.waitText("Press Ctrl-C again to exit")
	g.keys("\x03")
	g.line() // the interrupt
	if _, err := g.line(); !errors.Is(err, io.EOF) {
		t.Fatalf("second Ctrl-C: %v, want EOF", err)
	}
}

// The editing keys a shell user reaches for.
func TestEditingKeys(t *testing.T) {
	cases := []struct{ name, keys, want string }{
		{"ctrl-a then type", "world\x01hello ", "hello world"},
		{"ctrl-e", "ab\x01\x05c", "abc"},
		{"ctrl-u", "drop this\x15keep", "keep"},
		{"ctrl-k", "keep this\x01\x1bf\x0b", "keep"},
		{"ctrl-w", "one two/three\x17", "one "},
		{"ctrl-y", "cut me\x17\x17\x19", "cut me"},
		{"alt-b alt-f", "one two three\x1bb\x1bbX", "one Xtwo three"},
		{"ctrl-left", "one two three\x1b[1;5D\x1b[1;5DX", "one Xtwo three"},
		{"ctrl-right", "one two\x01\x1b[1;5CX", "oneX two"},
		{"alt-d", "one two\x01\x1bd", " two"},
		{"undo", "abc def\x17\x1f", "abc def"},
		{"home end tilde", "ab\x1b[1~X\x1b[4~Y", "XabY"},
		{"delete", "abc\x01\x1b[3~", "bc"},
	}
	for _, c := range cases {
		g := newRig(t, 80, 24)
		for _, part := range splitKeys(c.keys) {
			g.keys(part)
			time.Sleep(3 * time.Millisecond)
		}
		g.settle()
		g.keys("\r")
		if got, _ := g.line(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// splitKeys cuts a key script into keys: each escape sequence whole, every
// other character alone.
func splitKeys(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) {
			j := i + 2
			if s[i+1] == '[' {
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				j++
			}
			out = append(out, s[i:min(j, len(s))])
			i = j
			continue
		}
		_, size := firstRune(s[i:])
		out = append(out, s[i:i+size])
		i += size
	}
	return out
}

func firstRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 0
}

// Vim mode: Esc leaves insert mode, and normal-mode keys edit.
func TestVimMode(t *testing.T) {
	g := newRig(t, 80, 24)
	g.lr.SetVim(true)
	for _, k := range []string{"hello world", "\x1b", "0", "d", "w", "A", "!"} {
		g.keys(k)
		time.Sleep(40 * time.Millisecond)
	}
	g.settle()
	g.keys("\r")
	if got, _ := g.line(); got != "world!" {
		t.Fatalf("got %q", got)
	}
}

// A prompt longer than the terminal is wide wraps in place: at 40 columns
// the same line must not be printed again down the screen on every key.
func TestLongPromptWrapsOnce(t *testing.T) {
	g := newRig(t, 40, 20)
	text := "type a fairly long line of input text " + strings.Repeat("abc ", 40) + "END"
	g.typed(text)
	g.settle()
	all := g.term.All()
	if n := strings.Count(all, "type a fairly"); n != 1 {
		t.Fatalf("the prompt is on screen %d times:\n%s", n, g.term.Dump())
	}
	if !strings.Contains(g.term.Text(), "END") {
		t.Fatalf("the end of the prompt is not shown:\n%s", g.term.Dump())
	}
	g.keys("\r")
	if got, _ := g.line(); got != text {
		t.Fatalf("got %q", got)
	}
}

// Messages typed while a turn runs are visible, as typed and once queued.
func TestTypingDuringATurnIsVisible(t *testing.T) {
	g := newRig(t, 80, 24)
	g.lr.Quiet(true)
	g.typed("use the other file")
	g.waitText("use the other file")
	g.keys("\r")
	if got, _ := g.line(); got != "use the other file" {
		t.Fatalf("got %q", got)
	}
	g.waitText("↳ use the other file")
	g.lr.Quiet(false)
	g.waitScreen("the queue cleared", func(s string) bool { return !strings.Contains(s, "↳") })
}

// A background-colour answer that arrives late — after startup stopped
// waiting, as over a slow link — is never typed into the prompt and its BEL
// never opens the editor; when the theme is automatic it sets the theme.
func TestLateTerminalReplyIsNotTyped(t *testing.T) {
	defer func() { _ = SetTheme("dark") }()
	g := newRig(t, 80, 24)
	edited := false
	g.lr.d.mu.Lock()
	g.lr.d.extEdit = func(string) (string, error) { edited = true; return "", nil }
	g.lr.d.autoTheme = true
	g.lr.d.mu.Unlock()
	g.keys("hi")
	g.settle()
	time.Sleep(300 * time.Millisecond)
	g.keys("\x1b]11;rgb:ffff/ffff/ffff\x07")
	g.settle()
	g.keys(" there\r")
	if got, _ := g.line(); got != "hi there" {
		t.Fatalf("the prompt was %q", got)
	}
	if edited {
		t.Fatal("the reply's BEL opened the editor")
	}
	if Theme() != "light" {
		t.Fatalf("the late answer did not set the theme: %s", Theme())
	}
	// A chosen theme is not overridden.
	g.lr.SetAutoTheme(false)
	g.keys("\x1b]11;rgb:0000/0000/0000\x07")
	g.settle()
	if Theme() != "light" {
		t.Fatalf("a late answer overrode a chosen theme: %s", Theme())
	}
}

// Synchronized output is used only once the terminal has said it knows it.
func TestSyncOnlyWhenTheTerminalSaysSo(t *testing.T) {
	g := newRig(t, 60, 20)
	g.lr.Append(Block{Kind: BlockNotice, Text: "one"})
	g.settle()
	g.out.mu.Lock()
	before := g.out.log.String()
	g.out.mu.Unlock()
	if strings.Contains(before, "\x1b[?2026h") {
		t.Fatal("synchronized output was sent before the terminal said it knows it")
	}
	g.keys("\x1b[?2026;2$y")
	g.settle()
	g.out.mark()
	g.lr.Append(Block{Kind: BlockNotice, Text: "two"})
	g.settle()
	g.out.mu.Lock()
	after := g.out.log.String()[len(before):]
	g.out.mu.Unlock()
	if !strings.Contains(after, "\x1b[?2026h") {
		t.Fatal("synchronized output was not used once the terminal said it knows it")
	}
}

// A paste is expanded by its number, not by matching its label: a paste
// that holds another paste's label text is sent as it was pasted.
func TestPastesExpandByNumber(t *testing.T) {
	var b inputBuf
	first := "one\ntwo\nthree\nfour"
	second := "quoting [Pasted text #1 +4 lines] from before\nb\nc\nd"
	b.paste(first)
	b.insert([]rune(" and "))
	b.paste(second)
	if got, want := b.expanded(), first+" and "+second; got != want {
		t.Fatalf("expanded %q, want %q", got, want)
	}
	if got := b.shown(); got != "[Pasted text #1 +4 lines] and [Pasted text #2 +4 lines]" {
		t.Fatalf("shown %q", got)
	}
	// Backspace takes a placeholder whole.
	b.backspace()
	if got := b.expanded(); got != first+" and " {
		t.Fatalf("after backspace %q", got)
	}
}

// A collapsed paste is filtered as typed text is: opening it with Tab puts
// no escape on the line or on the screen.
func TestPasteKeepsNoControls(t *testing.T) {
	g := newRig(t, 80, 24)
	g.keys("\x1b[200~a\x1b]0;TITLE\x07\nb\x1b]52;c;eA==\x07\nc\u202e\nd\x1b[2J\x1b[201~")
	g.waitText("[Pasted text #1 +4 lines]")
	g.keys("\t")
	g.settle()
	g.out.mu.Lock()
	wire := g.out.log.String()
	g.out.mu.Unlock()
	assertClean(t, "an opened paste", wire)
	g.keys("\r")
	got, _ := g.line()
	if strings.ContainsAny(got, "\x1b\x07\u202e") {
		t.Fatalf("the sent line kept controls: %q", got)
	}
}

// Alt+] and Alt+Shift+P, X, _ and ^ pressed by a person are keys, not the
// start of a terminal reply that would swallow what is typed after them.
func TestAltBracketDoesNotSwallowTyping(t *testing.T) {
	for _, alt := range []string{"\x1b]", "\x1bP", "\x1bX", "\x1b_", "\x1b^"} {
		g := newRig(t, 80, 24)
		g.keys(alt)
		time.Sleep(60 * time.Millisecond)
		g.typed("hello")
		g.settle()
		g.keys("\r")
		if got, _ := g.line(); got != "hello" {
			t.Errorf("after %q the line was %q", alt, got)
		}
	}
}

// A file name from @ completion goes on the line as typed text would: no
// escape, no control, no paste character.
func TestMentionCompletionIsFiltered(t *testing.T) {
	g := newRig(t, 80, 24)
	g.lr.SetFiles(func() []string { return []string{"evil\U0010FF00\x1b]0;T\x07name\u202e.go"} })
	g.typed("@evil")
	g.settle()
	g.keys("\t")
	g.settle()
	g.out.mu.Lock()
	wire := g.out.log.String()
	g.out.mu.Unlock()
	assertClean(t, "a completed file name", wire)
	g.keys("\r")
	got, _ := g.line()
	if strings.ContainsAny(got, "\x1b\x07\u202e\U0010FF00") || !strings.Contains(got, "@evil") {
		t.Fatalf("the line was %q", got)
	}
}

// Up then Down brings back a line with a collapsed paste as it was, not its
// label as text.
func TestHistoryKeepsACollapsedPaste(t *testing.T) {
	g := newRig(t, 80, 24)
	h := &History{}
	h.Add("an older prompt", false)
	g.lr.SetHistory(h)
	body := "p1\np2\np3\np4\np5"
	g.keys("see \x1b[200~" + body + "\x1b[201~")
	g.waitText("[Pasted text #1 +5 lines]")
	g.keys("\x1b[A")
	g.waitText("an older prompt")
	g.keys("\x1b[B")
	g.waitText("[Pasted text #1 +5 lines]")
	g.keys("\r")
	if got, _ := g.line(); got != "see "+body {
		t.Fatalf("sent %q", got)
	}
}

// The character that stands for a paste can only be put on the line by a
// paste: insert refuses it, whatever calls insert.
func TestInsertRefusesPasteCharacters(t *testing.T) {
	var b inputBuf
	b.insert([]rune{'a', pasteRuneFirst, 'b', pasteRuneFirst + 5})
	if got := string(b.line); got != "ab" {
		t.Fatalf("line %q", got)
	}
}

// A recalled history entry is filtered as a paste is: a history file on disk
// is not trusted text, so no escape, bidi override or paste placeholder
// comes back onto the line or into what is sent.
func TestHistoryRecallIsFiltered(t *testing.T) {
	g := newRig(t, 80, 24)
	h := &History{}
	h.Add("say \x1b]52;c;aGk=\x07hi \u202eevil\u202c \U0010FF00end\u009b2J", false)
	g.lr.SetHistory(h)
	g.keys("\x1b[A")
	g.settle()
	g.keys("\r")
	got, _ := g.line()
	for _, r := range got {
		if hiddenRune(r) || isPasteRune(r) {
			t.Fatalf("recall kept %U: %q", r, got)
		}
	}
	if !strings.Contains(got, "say") || !strings.Contains(got, "end") {
		t.Fatalf("recall lost the text: %q", got)
	}
}
