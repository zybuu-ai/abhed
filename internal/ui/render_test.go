package ui

import (
	"bytes"
	"strings"
	"testing"
)

// Styling must follow where the bytes end up, not the type of the writer.
//
// Wrapping os.Stdout in anything at all silently turned colour off, because the
// check could only recognise an *os.File — so the banner went monochrome the
// moment the interactive path started routing output through a wrapper.
func TestStyleAsksAWriterThatKnows(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")

	if s := NewStyle(claimsTerminal{true}); !s.enabled {
		t.Error("a writer reporting a terminal must get colour")
	}
	if s := NewStyle(claimsTerminal{false}); s.enabled {
		t.Error("a writer reporting no terminal must not get colour")
	}

	// A plain buffer is not a terminal and must stay monochrome, so piped
	// output and captured logs are not full of escape codes.
	if s := NewStyle(&bytes.Buffer{}); s.enabled {
		t.Error("a buffer is not a terminal")
	}
}

// NO_COLOR outranks everything, including a writer that says it is a terminal.
func TestNoColorWins(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "xterm-256color")
	if s := NewStyle(claimsTerminal{true}); s.enabled {
		t.Error("NO_COLOR must disable colour whatever the writer says")
	}
}

type claimsTerminal struct{ yes bool }

func (claimsTerminal) Write(p []byte) (int, error) { return len(p), nil }
func (c claimsTerminal) IsTerminal() bool          { return c.yes }

// Models write markdown whether or not anything asked them to, so a raw print
// shows the reader the markup instead of the answer.
func TestMarkdownRendersWhatModelsActuallyEmit(t *testing.T) {
	s := Style{true}

	got := Markdown(s, "**z/OS** is an operating system.")
	if strings.Contains(got, "**") {
		t.Errorf("bold markers survived: %q", got)
	}
	if !strings.Contains(got, "z/OS") {
		t.Errorf("the text itself was lost: %q", got)
	}

	got = Markdown(s, "### A heading")
	if strings.Contains(got, "###") {
		t.Errorf("heading hashes survived: %q", got)
	}

	got = Markdown(s, "*   an item\n*   another")
	if strings.Count(got, "•") != 2 {
		t.Errorf("want two bullets: %q", got)
	}

	got = Markdown(s, "Run `SETROPTS` now.")
	if strings.Contains(got, "`") {
		t.Errorf("code ticks survived: %q", got)
	}
}

// A table's columns are the reason it was written as a table, and alignment is
// the one thing a raw print cannot give.
func TestMarkdownAlignsATable(t *testing.T) {
	in := "| Feature | Value |\n| :--- | :--- |\n| Short | x |\n| Much longer name | y |"
	got := Markdown(Style{false}, in)

	if strings.Contains(got, "|") {
		t.Errorf("pipes survived: %q", got)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 4 {
		t.Fatalf("want header, rule and two rows, got %d lines:\n%s", len(lines), got)
	}
	// The two value cells must start at the same column.
	col := func(s string) int { return strings.Index(s, "x") }
	if c := col(lines[2]); c > 0 {
		if strings.Index(lines[3], "y") != c {
			t.Errorf("columns are not aligned:\n%s", got)
		}
	}
}

// A fenced block means the characters inside it are not markup.
func TestMarkdownLeavesCodeBlocksAlone(t *testing.T) {
	got := Markdown(Style{false}, "```go\nx := **p\n```")
	if !strings.Contains(got, "x := **p") {
		t.Errorf("a code block was reformatted: %q", got)
	}
}

// Anything unrecognised must survive as written: an unrendered line is
// readable, a mangled one is not.
func TestMarkdownPassesThroughPlainText(t *testing.T) {
	const plain = "Just a sentence with no markup at all."
	if got := Markdown(Style{false}, plain); got != plain {
		t.Errorf("plain text was altered:\n  in:  %q\n  out: %q", plain, got)
	}
}

// An unclosed marker is not emphasis, and must not swallow the rest of the line.
func TestMarkdownToleratesUnclosedMarkers(t *testing.T) {
	const in = "This has an **unclosed marker and keeps going."
	if got := Markdown(Style{false}, in); !strings.Contains(got, "keeps going") {
		t.Errorf("text after an unclosed marker was lost: %q", got)
	}
}
