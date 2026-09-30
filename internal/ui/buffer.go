package ui

import (
	"fmt"
	"strings"
	"unicode"
)

// inputBuf is the text being typed and the cursor in it, with the editing
// operations a prompt needs. It does no I/O, so every key's effect can be
// tested without a terminal.
//
// The cursor is a rune index that only ever rests between grapheme clusters:
// Left, Right and Backspace step over "é" written as e + accent, a flag, or a
// family emoji as one character, which is what the person sees.
type inputBuf struct {
	line []rune
	pos  int

	undo   []snapshot
	lastOp editOp
	kill   string
	// killAt and killLen are where the last kill left the cursor and the
	// line's length, so a kill straight after it adds to the kill buffer,
	// as Ctrl-W twice then Ctrl-Y brings back both words.
	killAt, killLen int

	// pastes are large pastes shown on the line as a placeholder; submit
	// expands each placeholder back to its text.
	pastes []string
}

type snapshot struct {
	line []rune
	pos  int
}

type editOp int

const (
	opOther editOp = iota
	opInsert
)

func (b *inputBuf) String() string { return string(b.line) }

func (b *inputBuf) empty() bool { return len(b.line) == 0 }

// reset clears the line, its undo history and its pastes.
func (b *inputBuf) reset() {
	b.line, b.pos, b.undo, b.lastOp, b.pastes = b.line[:0], 0, nil, opOther, nil
}

// set replaces the line and puts the cursor at its end.
func (b *inputBuf) set(s string) {
	b.line = []rune(s)
	b.pos = len(b.line)
}

func (b *inputBuf) pushUndo(op editOp) {
	if op == opInsert && b.lastOp == opInsert {
		return // a run of typing undoes as one step
	}
	b.lastOp = op
	b.undo = append(b.undo, snapshot{append([]rune(nil), b.line...), b.pos})
	if len(b.undo) > 200 {
		b.undo = b.undo[1:]
	}
}

func (b *inputBuf) popUndo() bool {
	if len(b.undo) == 0 {
		return false
	}
	s := b.undo[len(b.undo)-1]
	b.undo = b.undo[:len(b.undo)-1]
	b.line, b.pos = s.line, s.pos
	b.lastOp = opOther
	return true
}

// insert puts rs at the cursor. Control characters other than newline are
// dropped: they would draw nothing and send the model bytes nobody typed.
func (b *inputBuf) insert(rs []rune) {
	clean := rs[:0:0]
	for _, r := range rs {
		switch {
		case r == '\n':
		case r == '\t':
		case r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0:
			continue
		}
		clean = append(clean, r)
	}
	if len(clean) == 0 {
		return
	}
	b.killLen = -1
	op := opInsert
	if len(clean) != 1 || clean[0] == ' ' || clean[0] == '\n' {
		op = opOther
	}
	b.pushUndo(op)
	if op != opInsert {
		b.lastOp = opOther
	}
	tail := append([]rune(nil), b.line[b.pos:]...)
	b.line = append(append(b.line[:b.pos], clean...), tail...)
	b.pos += len(clean)
}

// backspace removes the character before the cursor: a whole cluster, or a
// whole paste placeholder.
func (b *inputBuf) backspace() {
	if b.pos == 0 {
		return
	}
	if n := b.placeholderBefore(); n > 0 {
		b.cut(b.pos-n, b.pos, false)
		return
	}
	b.pushUndo(opOther)
	b.killLen = -1
	start := clusterStart(b.line, b.pos)
	b.line = append(b.line[:start], b.line[b.pos:]...)
	b.pos = start
}

func (b *inputBuf) deleteForward() {
	if b.pos >= len(b.line) {
		return
	}
	b.pushUndo(opOther)
	b.killLen = -1
	end := clusterEnd(b.line, b.pos)
	b.line = append(b.line[:b.pos], b.line[end:]...)
}

func (b *inputBuf) left() {
	b.pos = clusterStart(b.line, b.pos)
}

func (b *inputBuf) right() {
	if b.pos < len(b.line) {
		b.pos = clusterEnd(b.line, b.pos)
	}
}

// cut removes line[from:to], into the kill buffer when kill is set, for
// Ctrl-Y to bring back.
func (b *inputBuf) cut(from, to int, kill bool) {
	from = max(from, 0)
	to = min(to, len(b.line))
	if from >= to {
		return
	}
	b.pushUndo(opOther)
	if kill {
		text := string(b.line[from:to])
		switch {
		case b.killLen == len(b.line) && b.killAt == b.pos && b.kill != "" && to == b.pos:
			b.kill = text + b.kill // killing backwards again
		case b.killLen == len(b.line) && b.killAt == b.pos && b.kill != "" && from == b.pos:
			b.kill += text // killing forwards again
		default:
			b.kill = text
		}
	}
	b.line = append(b.line[:from], b.line[to:]...)
	b.pos = from
	if kill {
		b.killAt, b.killLen = b.pos, len(b.line)
	} else {
		b.killLen = -1
	}
}

func (b *inputBuf) yank() {
	b.killLen = -1
	if b.kill != "" {
		b.pushUndo(opOther)
		b.lastOp = opOther
		b.insert([]rune(b.kill))
	}
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

// wordLeft is the start of the word before the cursor.
func (b *inputBuf) wordLeft() int {
	i := b.pos
	for i > 0 && !isWordRune(b.line[i-1]) {
		i--
	}
	for i > 0 && isWordRune(b.line[i-1]) {
		i--
	}
	return i
}

// wordLeftSpace is Ctrl-W's reach: back to whitespace, as a shell does, so a
// path is erased whole.
func (b *inputBuf) wordLeftSpace() int {
	i := b.pos
	for i > 0 && unicode.IsSpace(b.line[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(b.line[i-1]) {
		i--
	}
	return i
}

func (b *inputBuf) wordRight() int {
	i := b.pos
	for i < len(b.line) && !isWordRune(b.line[i]) {
		i++
	}
	for i < len(b.line) && isWordRune(b.line[i]) {
		i++
	}
	return i
}

// lineStart and lineEnd bound the logical line (between newlines) holding i.
func (b *inputBuf) lineStart(i int) int {
	for i > 0 && b.line[i-1] != '\n' {
		i--
	}
	return i
}

func (b *inputBuf) lineEnd(i int) int {
	for i < len(b.line) && b.line[i] != '\n' {
		i++
	}
	return i
}

// killToStart is Ctrl-U: to the start of the logical line, or at its start,
// the newline before it.
func (b *inputBuf) killToStart() {
	start := b.lineStart(b.pos)
	if start == b.pos && start > 0 {
		start--
	}
	b.cut(start, b.pos, true)
}

// killToEnd is Ctrl-K.
func (b *inputBuf) killToEnd() {
	end := b.lineEnd(b.pos)
	if end == b.pos && end < len(b.line) {
		end++
	}
	b.cut(b.pos, end, true)
}

// onFirstLine and onLastLine say whether Up and Down leave a multi-line entry
// for history rather than moving within it.
func (b *inputBuf) onFirstLine() bool { return b.lineStart(b.pos) == 0 }
func (b *inputBuf) onLastLine() bool  { return b.lineEnd(b.pos) == len(b.line) }

// up and down move between the logical lines of a multi-line entry, keeping
// the column as far as the target line allows.
func (b *inputBuf) up() {
	start := b.lineStart(b.pos)
	if start == 0 {
		return
	}
	col := b.pos - start
	prevStart := b.lineStart(start - 1)
	b.pos = min(prevStart+col, start-1)
	b.snap()
}

func (b *inputBuf) down() {
	end := b.lineEnd(b.pos)
	if end == len(b.line) {
		return
	}
	col := b.pos - b.lineStart(b.pos)
	next := end + 1
	b.pos = min(next+col, b.lineEnd(next))
	b.snap()
}

// snap moves the cursor back onto a cluster boundary.
func (b *inputBuf) snap() {
	for p := 0; p < len(b.line); {
		e := clusterEnd(b.line, p)
		if e > b.pos {
			if p < b.pos {
				b.pos = p
			}
			return
		}
		p = e
	}
}

// Pastes.

// pasteThreshold is when a paste becomes a placeholder: more lines than fit
// comfortably in the input, or enough text to push the prompt off screen.
const (
	pasteLines = 3
	pasteChars = 800
)

// paste inserts pasted text. A large paste becomes a placeholder on the line,
// so a pasted log does not bury the prompt, and it is sent whole, as one
// message, when the line is submitted.
func (b *inputBuf) paste(text string) {
	text = strings.ReplaceAll(text, "\t", "    ")
	if text == "" {
		return
	}
	lines := strings.Count(strings.TrimRight(text, "\n"), "\n") + 1
	if lines > pasteLines || len([]rune(text)) > pasteChars {
		b.pastes = append(b.pastes, text)
		b.pushUndo(opOther)
		b.lastOp = opOther
		b.insertRaw([]rune(pasteLabel(len(b.pastes), text)))
		return
	}
	b.pushUndo(opOther)
	b.lastOp = opOther
	b.insert([]rune(text))
}

func (b *inputBuf) insertRaw(rs []rune) {
	tail := append([]rune(nil), b.line[b.pos:]...)
	b.line = append(append(b.line[:b.pos], rs...), tail...)
	b.pos += len(rs)
}

// pasteLabel is the placeholder for paste n, as it appears on the line.
func pasteLabel(n int, text string) string {
	lines := strings.Count(strings.TrimRight(text, "\n"), "\n") + 1
	if lines == 1 {
		return fmt.Sprintf("[Pasted text #%d, %d chars]", n, len([]rune(text)))
	}
	return fmt.Sprintf("[Pasted text #%d +%d lines]", n, lines)
}

// placeholderBefore is the length of the paste placeholder that ends at the
// cursor, or 0.
func (b *inputBuf) placeholderBefore() int {
	for i, p := range b.pastes {
		l := []rune(pasteLabel(i+1, p))
		if b.pos >= len(l) && string(b.line[b.pos-len(l):b.pos]) == string(l) {
			return len(l)
		}
	}
	return 0
}

// expandPlaceholder replaces the placeholder before the cursor with its text,
// so the person can edit what they pasted. It reports whether there was one.
func (b *inputBuf) expandPlaceholder() bool {
	for i, p := range b.pastes {
		l := []rune(pasteLabel(i+1, p))
		if b.pos >= len(l) && string(b.line[b.pos-len(l):b.pos]) == string(l) {
			b.pushUndo(opOther)
			b.lastOp = opOther
			tail := append([]rune(nil), b.line[b.pos:]...)
			start := b.pos - len(l)
			b.line = append(append(b.line[:start], []rune(p)...), tail...)
			b.pos = start + len([]rune(p))
			return true
		}
	}
	return false
}

// expanded is the line with every placeholder replaced by its paste: what is
// actually sent.
func (b *inputBuf) expanded() string {
	s := string(b.line)
	for i, p := range b.pastes {
		s = strings.Replace(s, pasteLabel(i+1, p), p, 1)
	}
	return s
}
