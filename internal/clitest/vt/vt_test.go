package vt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The recorded audit sessions end on the screen an independent emulator drew
// from the same bytes. Paths were replaced by ones of the same width, and the
// reference was drawn without CSI < u, which that emulator prints as "u".
func TestRecordedSessions(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.bin")
	if len(files) == 0 {
		t.Fatal("no recordings")
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".bin")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(strings.TrimSuffix(f, ".bin") + ".screen")
			if err != nil {
				t.Fatal(err)
			}
			head, body, _ := strings.Cut(string(want), "\n")
			var cols, rows, cy, cx int
			if _, err := fmt.Sscanf(head, "size %d %d cursor %d %d", &cols, &rows, &cy, &cx); err != nil {
				t.Fatal(err)
			}
			term := New(cols, rows, nil)
			// Fed in odd-sized pieces, so sequences and UTF-8 split across writes.
			for i := 0; i < len(raw); i += 7 {
				_, _ = term.Write(raw[i:min(i+7, len(raw))])
			}
			s := term.Snapshot()
			lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
			for i := 0; i < rows; i++ {
				w := ""
				if i < len(lines) {
					w = lines[i]
				}
				if got := s.Line(i); got != w {
					t.Errorf("row %d:\n got %q\nwant %q", i, got, w)
				}
			}
			if r, c := s.Cursor(); r != cy || c != cx {
				t.Errorf("cursor (%d,%d), want (%d,%d)", r, c, cy, cx)
			}
		})
	}
}

func feed(t *Terminal, s string) *Snapshot {
	_, _ = t.Write([]byte(s))
	return t.Snapshot()
}

func TestCursorAndErase(t *testing.T) {
	term := New(10, 4, nil)
	s := feed(term, "hello\r\nworld\x1b[1;3HX\x1b[2;2H\x1b[K")
	if s.Line(0) != "heXlo" || s.Line(1) != "w" {
		t.Fatalf("%q", s.Text())
	}
	s = feed(term, "\x1b[H\x1b[J")
	if strings.TrimSpace(s.Text()) != "" {
		t.Fatalf("not cleared: %q", s.Text())
	}
	s = feed(term, "abc\x1b[2D\x1b[P")
	if s.Line(0) != "ac" {
		t.Fatalf("delete char: %q", s.Line(0))
	}
	s = feed(term, "\x1b[1G\x1b[2@")
	if s.Line(0) != "  ac" {
		t.Fatalf("insert char: %q", s.Line(0))
	}
}

func TestPendingWrapAndScrollback(t *testing.T) {
	term := New(5, 2, nil)
	s := feed(term, "abcde")
	if r, c := s.Cursor(); r != 0 || c != 4 {
		t.Fatalf("cursor after a full line (%d,%d)", r, c)
	}
	s = feed(term, "f\r\ng\r\nh")
	if s.Line(0) != "g" || s.Line(1) != "h" {
		t.Fatalf("%q", s.Text())
	}
	if strings.Join(s.Scrollback, "|") != "abcde|f" {
		t.Fatalf("scrollback %q", s.Scrollback)
	}
}

func TestScrollRegion(t *testing.T) {
	term := New(4, 4, nil)
	s := feed(term, "1\r\n2\r\n3\r\n4\x1b[2;3r\x1b[3;1H\nX")
	if got := strings.Join([]string{s.Line(0), s.Line(1), s.Line(2), s.Line(3)}, ","); got != "1,3,X,4" {
		t.Fatalf("region scroll: %s", got)
	}
	if len(s.Scrollback) != 0 {
		t.Fatalf("a region scroll reached the scrollback: %q", s.Scrollback)
	}
}

func TestSGR(t *testing.T) {
	term := New(20, 1, nil)
	s := feed(term, "\x1b[1;2;38;5;202mA\x1b[0;7;48;2;255;0;16mB\x1b[22;27;91mC\x1b[0mD")
	a, b, c, d := s.Cell(0, 0).Attrs, s.Cell(0, 1).Attrs, s.Cell(0, 2).Attrs, s.Cell(0, 3).Attrs
	if !a.Bold || !a.Dim || a.FG != "202" {
		t.Errorf("A %+v", a)
	}
	if !b.Reverse || b.BG != "#ff0010" || b.Bold {
		t.Errorf("B %+v", b)
	}
	if c.FG != "9" || c.Reverse || c.BG != "#ff0010" {
		t.Errorf("C %+v", c)
	}
	if d != (Attrs{}) {
		t.Errorf("D %+v", d)
	}
}

func TestWideCharacters(t *testing.T) {
	term := New(5, 2, nil)
	s := feed(term, "a你b")
	if s.Line(0) != "a你b" || s.Cell(0, 1).Width != 2 || s.Cell(0, 2).Width != 0 {
		t.Fatalf("%q %+v", s.Line(0), s.Cell(0, 1))
	}
	if r, c := s.Cursor(); r != 0 || c != 4 {
		t.Fatalf("cursor (%d,%d)", r, c)
	}
	// No room in the last column: it wraps whole.
	s = feed(term, "好")
	if s.Line(1) != "好" {
		t.Fatalf("%q", s.Text())
	}
}

func TestAltScreen(t *testing.T) {
	term := New(6, 2, nil)
	s := feed(term, "main\x1b[?1049h\x1b[Halt")
	if s.Line(0) != "alt" || !s.Modes.AltScreen {
		t.Fatalf("%q", s.Text())
	}
	s = feed(term, "\x1b[?1049l")
	if s.Line(0) != "main" {
		t.Fatalf("main screen not restored: %q", s.Text())
	}
	if r, c := s.Cursor(); r != 0 || c != 4 {
		t.Fatalf("cursor not restored (%d,%d)", r, c)
	}
}

func TestRepliesAndOSC(t *testing.T) {
	var replies []string
	term := New(10, 3, func(b []byte) { replies = append(replies, string(b)) })
	feed(term, "ab\x1b[6n\x1b[?2026$p\x1b[?2026h\x1b[?2026$p\x1b]11;?\x07\x1b]0;my title\x07\x1b]52;c;aGk=\x1b\\\x1b]9;done\x07")
	want := []string{"\x1b[1;3R", "\x1b[?2026;2$y", "\x1b[?2026;1$y", "\x1b]11;rgb:0000/0000/0000\x1b\\"}
	if strings.Join(replies, "|") != strings.Join(want, "|") {
		t.Fatalf("replies %q", replies)
	}
	s := term.Snapshot()
	if s.Title() != "my title" || len(s.Clipboard) != 1 || s.Clipboard[0] != "aGk=" || s.Notifications[0] != "done" {
		t.Fatalf("osc: %q %q %q", s.Title(), s.Clipboard, s.Notifications)
	}
	if !s.Modes.SyncOutput {
		t.Fatal("2026 not set")
	}

	// A terminal without synchronized output says so.
	replies = nil
	noSync := New(10, 3, func(b []byte) { replies = append(replies, string(b)) })
	noSync.NoSync = true
	feed(noSync, "\x1b[?2026h\x1b[?2026$p\x1b[?2004h\x1b[?2004$p")
	if strings.Join(replies, "|") != "\x1b[?2026;0$y|\x1b[?2004;1$y" {
		t.Fatalf("no-sync replies %q", replies)
	}
}

func TestResizeKeepsCursorRow(t *testing.T) {
	term := New(10, 4, nil)
	feed(term, "1\r\n2\r\n3\r\n4")
	term.Resize(6, 2)
	s := term.Snapshot()
	if s.Line(0) != "3" || s.Line(1) != "4" || strings.Join(s.Scrollback, "") != "12" {
		t.Fatalf("%q %q", s.Text(), s.Scrollback)
	}
	if r, _ := s.Cursor(); r != 1 {
		t.Fatalf("cursor row %d", r)
	}
}

func TestSplitSequences(t *testing.T) {
	term := New(10, 2, nil)
	for _, b := range []byte("\x1b[1;31mé\x1b[0m") {
		_, _ = term.Write([]byte{b})
	}
	s := term.Snapshot()
	if s.Line(0) != "é" || s.Cell(0, 0).Attrs.FG != "1" {
		t.Fatalf("%q %+v", s.Line(0), s.Cell(0, 0))
	}
}
