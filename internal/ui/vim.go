package ui

import "unicode"

// vimState is the optional modal editing of the input: Esc leaves insert
// mode for normal mode, where letters move and edit rather than type.
//
// It covers the motions and edits people reach for in a one-paragraph
// prompt — h l w b e 0 ^ $, x X D C, dd cc dw cw de ce, i a I A o, u p P,
// j k through lines and history — not an editor. Keys it does not know fall
// through to the ordinary bindings, so Ctrl-C, Ctrl-R and Enter work in
// either mode.
type vimState struct {
	insert  bool
	pending rune // an operator (d, c) waiting for its motion
}

// SetVim turns modal editing on or off.
func (d *dock) setVim(on bool) {
	if on {
		d.vim = &vimState{insert: true}
		return
	}
	d.vim = nil
}

// vimInsert reports whether keys type text: always without vim mode.
func (d *dock) vimInsert() bool { return d.vim == nil || d.vim.insert }

// vimKey applies k in vim mode, reporting whether it was consumed.
func (d *dock) vimKey(k key) bool {
	v := d.vim
	if v.insert {
		// Esc leaves insert mode; while a turn runs, a second Esc (now in
		// normal mode) interrupts, as it does without vim.
		if k.code == kEsc {
			v.insert = false
			d.buf.left()
			return true
		}
		return false
	}
	if k.code != kNone || k.alt {
		return false // arrows, Esc and the rest keep their ordinary meaning
	}
	b := &d.buf
	r := k.r
	if v.pending != 0 {
		op := v.pending
		v.pending = 0
		from, to := b.pos, b.pos
		switch r {
		case op: // dd, cc: the whole line
			from, to = b.lineStart(b.pos), b.lineEnd(b.pos)
			if op == 'd' && to < len(b.line) {
				to++
			} else if op == 'd' && from > 0 {
				from--
			}
		case 'w':
			to = b.wordRight()
			for to < len(b.line) && b.line[to] == ' ' {
				to++ // dw takes the space after the word too
			}
			if op == 'c' {
				to = d.vimWordEnd()
			}
		case 'e':
			to = d.vimWordEnd()
		case 'b':
			from = b.wordLeft()
		case '$':
			to = b.lineEnd(b.pos)
		case '0':
			from = b.lineStart(b.pos)
		default:
			return true
		}
		b.cut(from, to, true)
		if op == 'c' {
			v.insert = true
		}
		return true
	}
	switch r {
	case 'h':
		if b.pos > b.lineStart(b.pos) {
			b.left()
		}
	case 'l':
		if b.pos < b.lineEnd(b.pos) {
			b.right()
		}
	case 'w':
		b.pos = b.wordRight()
		for b.pos < len(b.line) && unicode.IsSpace(b.line[b.pos]) {
			b.pos++
		}
	case 'b':
		b.pos = b.wordLeft()
	case 'e':
		b.pos = max(d.vimWordEnd()-1, b.pos)
	case '0':
		b.pos = b.lineStart(b.pos)
	case '^':
		b.pos = b.lineStart(b.pos)
		for b.pos < len(b.line) && b.line[b.pos] == ' ' {
			b.pos++
		}
	case '$':
		b.pos = max(b.lineEnd(b.pos)-1, b.lineStart(b.pos))
	case 'x':
		b.deleteForward()
	case 'X':
		b.backspace()
	case 'D':
		b.cut(b.pos, b.lineEnd(b.pos), true)
	case 'C':
		b.cut(b.pos, b.lineEnd(b.pos), true)
		v.insert = true
	case 'd', 'c':
		v.pending = r
	case 'i':
		v.insert = true
	case 'a':
		b.right()
		v.insert = true
	case 'I':
		b.pos = b.lineStart(b.pos)
		v.insert = true
	case 'A':
		b.pos = b.lineEnd(b.pos)
		v.insert = true
	case 'o':
		b.pos = b.lineEnd(b.pos)
		b.insert([]rune{'\n'})
		v.insert = true
	case 'O':
		b.pos = b.lineStart(b.pos)
		b.insert([]rune{'\n'})
		b.pos--
		v.insert = true
	case 'u':
		b.popUndo()
	case 'p':
		b.right()
		b.yank()
	case 'P':
		b.yank()
	case 'j':
		d.down()
	case 'k':
		d.up()
	default:
		// Enter, Ctrl keys and the rest keep their ordinary meaning.
		return r >= 0x20 && r != 0x7f
	}
	return true
}

// vimWordEnd is just past the end of the word at or after the cursor.
func (d *dock) vimWordEnd() int {
	b := &d.buf
	i := b.pos
	for i < len(b.line) && !isWordRune(b.line[i]) {
		i++
	}
	for i < len(b.line) && isWordRune(b.line[i]) {
		i++
	}
	return i
}
