package ui

import (
	"bufio"
	"io"
	"strings"
	"testing"
	"time"
)

// The decoder turns every sequence terminals send into one key.
func TestKeyDecoderTable(t *testing.T) {
	type want struct {
		r     rune
		code  keyCode
		alt   bool
		ctrl  bool
		shift bool
	}
	cases := map[string]want{
		"a":             {r: 'a'},
		"é":             {r: 'é'},
		"你":             {r: '你'},
		"🙂":             {r: '🙂'},
		"\r":            {r: '\r'},
		"\n":            {r: '\n'},
		"\t":            {r: '\t'},
		"\x7f":          {r: 0x7f},
		"\x08":          {r: 0x08},
		"\x01":          {r: 1},
		"\x03":          {r: 3},
		"\x12":          {r: 18},
		"\x1f":          {r: 31},
		"\x1b[A":        {code: kUp},
		"\x1b[B":        {code: kDown},
		"\x1b[C":        {code: kRight},
		"\x1b[D":        {code: kLeft},
		"\x1bOA":        {code: kUp},
		"\x1bOB":        {code: kDown},
		"\x1bOC":        {code: kRight},
		"\x1bOD":        {code: kLeft},
		"\x1bOH":        {code: kHome},
		"\x1bOF":        {code: kEnd},
		"\x1b[H":        {code: kHome},
		"\x1b[F":        {code: kEnd},
		"\x1b[1~":       {code: kHome},
		"\x1b[7~":       {code: kHome},
		"\x1b[4~":       {code: kEnd},
		"\x1b[8~":       {code: kEnd},
		"\x1b[2~":       {code: kInsert},
		"\x1b[3~":       {code: kDelete},
		"\x1b[5~":       {code: kPgUp},
		"\x1b[6~":       {code: kPgDn},
		"\x1b[Z":        {code: kBackTab, shift: true},
		"\x1b[1;5D":     {code: kLeft, ctrl: true},
		"\x1b[1;5C":     {code: kRight, ctrl: true},
		"\x1b[1;3D":     {code: kLeft, alt: true},
		"\x1b[1;2A":     {code: kUp, shift: true},
		"\x1b[1;9C":     {code: kRight, alt: true},
		"\x1b[3;5~":     {code: kDelete, ctrl: true},
		"\x1b[13;2u":    {code: kEnter, shift: true},
		"\x1b[13;3u":    {code: kEnter, alt: true},
		"\x1b[13;5u":    {code: kEnter, ctrl: true},
		"\x1b[13u":      {r: '\r'},
		"\x1b[27u":      {code: kEsc},
		"\x1b[9;2u":     {code: kBackTab, shift: true},
		"\x1b[99;5u":    {r: 3},
		"\x1b[97;5u":    {r: 1},
		"\x1b[127u":     {r: 0x7f},
		"\x1b[97;3u":    {r: 'a', alt: true},
		"\x1b[233u":     {r: 'é'},
		"\x1b[57399u":   {code: kUnknown},
		"\x1b[27;2;13~": {code: kEnter, shift: true},
		"\x1b[27;5;99~": {r: 3},
		"\x1b\r":        {code: kEnter, alt: true},
		"\x1bb":         {r: 'b', alt: true},
		"\x1bf":         {r: 'f', alt: true},
		"\x1b\x7f":      {r: 0x7f, alt: true},
		"\x1b[99X":      {code: kUnknown},
		"\x1bOM":        {code: kEnter, shift: true},
	}
	for seq, w := range cases {
		kr := newKeyReader(bufio.NewReader(strings.NewReader(seq)))
		k, err := kr.read()
		if err != nil {
			t.Errorf("%q: %v", seq, err)
			continue
		}
		got := want{r: k.r, code: k.code, alt: k.alt, ctrl: k.ctrl, shift: k.shift}
		if got != w {
			t.Errorf("%q: got %+v, want %+v", seq, got, w)
		}
		if _, err := kr.read(); err == nil {
			t.Errorf("%q decoded into more than one key", seq)
		}
	}
}

// A bracketed paste is one key carrying the text, line endings normalised.
func TestPasteDecodes(t *testing.T) {
	kr := newKeyReader(bufio.NewReader(strings.NewReader("\x1b[200~a\r\nb\rc\x1b[201~x")))
	k, _ := kr.read()
	if k.code != kPaste || k.paste != "a\nb\nc" {
		t.Fatalf("got %+v", k)
	}
	if k, _ := kr.read(); k.r != 'x' {
		t.Fatalf("after the paste: %+v", k)
	}
}

// A terminal's answer is one reply, not keys, whatever its terminator, and
// the key after it is its own.
func TestTerminalRepliesAreNotKeys(t *testing.T) {
	for _, seq := range []string{
		"\x1b]11;rgb:1e1e/1e1e/1e1e\x07",
		"\x1b]11;rgb:ffff/ffff/ffff\x1b\\",
		"\x1b]52;c;U1BPT0Y=\x07",
		"\x1bP1$r0m\x1b\\",
		"\x1b_Gi=1;OK\x1b\\",
		"\x1b[?62;22c",
		"\x1b[?2026;2$y",
	} {
		kr := newKeyReader(bufio.NewReader(strings.NewReader(seq + "a")))
		k, _ := kr.read()
		if k.code != kReply {
			t.Errorf("%q: got %+v, want a reply", seq, k)
		}
		if k, _ := kr.read(); k.r != 'a' || k.code != kNone {
			t.Errorf("%q: the key after it was %+v", seq, k)
		}
	}
}

// chunked hands out one chunk per Read, as bytes split by a slow link arrive.
type chunked struct{ parts []string }

func (c *chunked) Read(p []byte) (int, error) {
	if len(c.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.parts[0])
	c.parts[0] = c.parts[0][n:]
	if c.parts[0] == "" {
		c.parts = c.parts[1:]
	}
	return n, nil
}

// A reply split right after its introducer is still a reply when the rest
// begins as an answer does; Alt+] followed by typing stays Alt+] and the
// typing.
func TestSplitReplyAfterIntroducer(t *testing.T) {
	for _, c := range []struct {
		parts []string
		reply bool
	}{
		{[]string{"\x1b]", "11;rgb:0000/0000/0000\x07a"}, true},
		{[]string{"\x1b]", "4;1;rgb:ffff/0000/0000\x1b\\a"}, true},
		{[]string{"\x1bP", "1$r0m\x1b\\a"}, true},
		{[]string{"\x1bP", ">|xterm(390)\x1b\\a"}, true},
		{[]string{"\x1b]", "a"}, false},
		{[]string{"\x1b]", "11a"}, false},
		{[]string{"\x1b]", ";a"}, false},
		{[]string{"\x1bP", "1a"}, false},
		{[]string{"\x1b_", "Gi=1;OK\x1b\\a"}, false},
	} {
		kr := newKeyReader(bufio.NewReader(&chunked{parts: append([]string(nil), c.parts...)}))
		kr.ready = func(time.Duration) bool { return true }
		k, _ := kr.read()
		if got := k.code == kReply; got != c.reply {
			t.Errorf("%q: got %+v, want reply=%v", c.parts, k, c.reply)
			continue
		}
		if !c.reply {
			if !k.alt {
				t.Errorf("%q: want an Alt key, got %+v", c.parts, k)
			}
			continue
		}
		if k, _ := kr.read(); k.r != 'a' || k.code != kNone {
			t.Errorf("%q: the key after it was %+v", c.parts, k)
		}
	}
	// Nothing more arrives: Alt+] at once, with no reply read.
	kr := newKeyReader(bufio.NewReader(&chunked{parts: []string{"\x1b]"}}))
	kr.ready = func(time.Duration) bool { return false }
	if k, _ := kr.read(); k.code == kReply || !k.alt || k.r != ']' {
		t.Errorf("a lone Alt+]: %+v", k)
	}
}
