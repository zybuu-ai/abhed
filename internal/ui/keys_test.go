package ui

import (
	"bufio"
	"strings"
	"testing"
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
