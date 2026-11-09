package model

import (
	"regexp"
	"strings"
)

// One logical block: an inline completion offers the statement at the
// cursor and, when that statement opens a block, the block's body; it stops
// at a blank line, at a line back at the statement's own indentation (after
// one closing line), or after maxBlockLines.

// maxBlockLines bounds one block, however deep its body.
const maxBlockLines = 12

// fimEnd matches the markers a fill-in-the-middle model writes when it is
// done (or starts another file); nothing after one is the suggestion.
var fimEnd = regexp.MustCompile(`<\|(?:endoftext|file_sep|fim_prefix|fim_suffix|fim_middle|fim_pad|repo_name|im_end)\|>|<(?:fim_prefix|fim_suffix|fim_middle|file_sep)>|<EOT>|<｜fim▁(?:begin|hole|end)｜>`)

// FIMStops are the stop sequences sent with a fill-in-the-middle request:
// the end-of-text and file markers that end an answer. Four, the most an
// OpenAI-shaped endpoint takes. A blank line is not one: it ends a block,
// but not a docstring with paragraphs, so OneBlock decides where the block
// ends and the stream is stopped there.
var FIMStops = []string{"<|endoftext|>", "<|file_sep|>", "<|fim_pad|>", "<|fim_prefix|>"}

// cutAtFIMEnd is text up to the first end marker.
func cutAtFIMEnd(text string) string {
	if m := fimEnd.FindStringIndex(text); m != nil {
		return text[:m[0]]
	}
	return text
}

// blockClosers start a line that closes the block its statement opened.
var blockClosers = []string{"}", ")", "]", "end", "fi", "done", "esac", "</"}

// indentWidth is the width of s's leading whitespace, a tab counting four.
func indentWidth(s string) int {
	w := 0
	for _, r := range s {
		switch r {
		case ' ':
			w++
		case '\t':
			w += 4
		default:
			return w
		}
	}
	return w
}

// quoted strips one line's string literals, so brackets inside them are not counted.
var quoted = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|` + "`[^`]*`")

// depthOf is the bracket depth a line leaves open, and whether it opens a
// block by its last character.
func depthOf(line string) (int, bool) {
	code := quoted.ReplaceAllString(line, "")
	if i := strings.Index(code, "//"); i >= 0 {
		code = code[:i]
	}
	if i := strings.Index(code, " #"); i >= 0 {
		code = code[:i]
	}
	d := 0
	for _, r := range code {
		switch r {
		case '(', '[', '{':
			d++
		case ')', ']', '}':
			d--
		}
	}
	t := strings.TrimRight(code, " \t\r\n")
	opens := strings.HasSuffix(t, ":") || strings.HasSuffix(t, "{") || strings.HasSuffix(t, "=>") ||
		strings.HasSuffix(t, " do") || strings.HasSuffix(t, " then") || strings.HasSuffix(t, "(") || strings.HasSuffix(t, "[")
	return d, opens
}

// OneBlock cuts a suggestion inserted after prefix to one logical block. It
// reports complete when the block's end has been seen, so a streamed answer
// can stop there; an answer that ends first is offered as it is.
func OneBlock(prefix, text string) (string, bool) {
	text = cutAtFIMEnd(text)
	cur := prefix[strings.LastIndexByte(prefix, '\n')+1:]
	lines := strings.SplitAfter(text, "\n")
	// The first line continues the cursor's line; when the answer starts on
	// a new line, its first line with code is the statement instead.
	first := 0
	head := cur + lines[0]
	if strings.TrimSpace(lines[0]) == "" && len(lines) > 1 {
		for first = 1; first < len(lines) && strings.TrimSpace(lines[first]) == ""; first++ {
		}
		if first == len(lines) {
			return text, false
		}
		head = lines[first]
	}
	if !strings.HasSuffix(lines[first], "\n") {
		return text, false // the statement's line is not finished yet
	}
	base := indentWidth(head)
	depth, opens := depthOf(head)
	triple := strings.Count(head, `"""`)%2 == 1 || strings.Count(head, `'''`)%2 == 1
	kept := first + 1
	cut := func(n int) (string, bool) {
		return strings.TrimRight(strings.Join(lines[:n], ""), "\n"), true
	}
	for i := first + 1; i < len(lines); i++ {
		line := lines[i]
		if !strings.HasSuffix(line, "\n") {
			// Unfinished, but once its indentation shows it is outside the
			// block, it is known not to belong: the answer can stop here.
			if !triple && depth == 0 && strings.TrimSpace(line) != "" &&
				(indentWidth(line) < base || (!opens && indentWidth(line) == base)) {
				return cut(kept)
			}
			return text, false
		}
		if kept-first >= maxBlockLines {
			return cut(kept)
		}
		body := strings.TrimSpace(line)
		switch {
		case triple || depth > 0:
			// Inside an open string or bracket: the statement goes on.
		case body == "":
			return cut(kept)
		case opens && indentWidth(line) > base:
			// The body of the block the statement opened.
		case opens && indentWidth(line) == base && closes(body):
			return cut(i + 1)
		default:
			return cut(kept)
		}
		if strings.Count(line, `"""`)%2 == 1 || strings.Count(line, `'''`)%2 == 1 {
			triple = !triple
		}
		if !triple {
			d, _ := depthOf(line)
			depth += d
		}
		kept = i + 1
	}
	return text, false
}

// closes reports a line that closes a block.
func closes(body string) bool {
	for _, c := range blockClosers {
		rest, ok := strings.CutPrefix(body, c)
		if ok && (rest == "" || !isWordByte(rest[0]) || !isWordByte(c[len(c)-1])) {
			return true
		}
	}
	return false
}

func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}
