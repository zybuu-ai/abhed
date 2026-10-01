package vt

import (
	"strings"
	"testing"
)

func TestPrintWrapAndScroll(t *testing.T) {
	term := New(5, 3)
	term.Write([]byte("abcdefgh\r\nij\r\nkl\r\nmn"))
	if got := term.Text(); got != "ij\nkl\nmn" {
		t.Fatalf("screen %q", got)
	}
	if sb := term.Scrollback(); strings.Join(sb, "|") != "abcde|fgh" {
		t.Fatalf("scrollback %q", sb)
	}
}

func TestPendingWrapAndErase(t *testing.T) {
	term := New(4, 2)
	term.Write([]byte("abcd\rX\x1b[K"))
	if got := term.Lines()[0]; got != "X" {
		t.Fatalf("row %q", got)
	}
}

func TestCursorMovesAndWide(t *testing.T) {
	term := New(10, 3)
	term.Write([]byte("你好\x1b[2Dx\x1b[1;1Hy"))
	if got := term.Lines()[0]; got != "y x" { // each overwrite clears the wide character it lands on
		t.Fatalf("row %q", got)
	}
	x, y := term.Cursor()
	if x != 1 || y != 0 {
		t.Fatalf("cursor %d,%d", x, y)
	}
}

func TestAltScreenRestores(t *testing.T) {
	term := New(10, 3)
	term.Write([]byte("main\x1b[?1049hALT\x1b[?1049l"))
	if got := term.Text(); got != "main" {
		t.Fatalf("screen %q", got)
	}
}

func TestSplitSequencesAcrossWrites(t *testing.T) {
	term := New(10, 2)
	term.Write([]byte("a\x1b["))
	term.Write([]byte("31mb\xe4\xbd"))
	term.Write([]byte("\xa0"))
	if got := term.Lines()[0]; got != "ab你" {
		t.Fatalf("row %q", got)
	}
	if c := term.CellAt(1, 0); c.Attr.FG != "31" {
		t.Fatalf("attr %+v", c.Attr)
	}
}
