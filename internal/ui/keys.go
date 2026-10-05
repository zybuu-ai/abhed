package ui

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// key is one decoded keypress: a rune, or a named key with modifiers, or a
// bracketed paste.
//
// The editor used to read one byte at a time and treat each byte as a rune.
// That broke three things users hit in their first minute: any non-ASCII
// character arrived as two or three garbage characters ("é" became "Ã©"), a
// Ctrl- or Alt-modified arrow ("\x1b[1;5C") inserted ";5C" into the line, and
// a pasted block of text either lost its newlines or submitted at the first
// one. Decoding into whole keys first fixes all three at the source.
type key struct {
	r     rune // the character, or a control code (Ctrl-A is 1) when code == 0
	code  keyCode
	alt   bool
	ctrl  bool
	shift bool
	paste string // the text of a bracketed paste, when code == kPaste
}

type keyCode int

const (
	kNone keyCode = iota
	kUp
	kDown
	kLeft
	kRight
	kHome
	kEnd
	kDelete
	kPgUp
	kPgDn
	kEsc
	kEnter // Enter with a modifier (Shift, Alt, Ctrl): a newline, not a submit
	kBackTab
	kPaste
	kInsert
	kUnknown
	// kReply is the terminal answering a query — a colour, its device
	// attributes, a mode report — not a key anyone pressed. Its text is in
	// key.paste. However late it arrives, it is never typing.
	kReply
)

// keyReader decodes a terminal's input stream into keys.
type keyReader struct {
	br *bufio.Reader
	// ready reports whether more input arrives within a duration; nil when
	// the source cannot be polled, as in tests.
	ready func(time.Duration) bool
	// escAt is when a lone Esc was last returned: a reply split straight
	// after its ESC arrives as "]11;…" shortly after it.
	escAt time.Time
	now   func() time.Time
}

func newKeyReader(br *bufio.Reader) *keyReader { return &keyReader{br: br, now: time.Now} }

// escWait is how long a lone Esc waits for the rest of a sequence. Terminals
// send a sequence in one write, so this only matters over a slow link.
const escWait = 30 * time.Millisecond

// more reports whether another byte follows an Esc as part of the same key:
// already read with it, or arriving within escWait. Where the input cannot be
// polled, only bytes that arrived with the Esc count, so a lone Esc is a key
// at once rather than waiting for the next one.
func (k *keyReader) more() bool {
	if k.br.Buffered() > 0 {
		return true
	}
	if k.ready == nil {
		return false
	}
	return k.ready(escWait)
}

// read returns the next key.
//
// A lone Esc is told apart from the start of an escape sequence by whether
// more input follows at once: a terminal writes a whole sequence in one go,
// and a person pressing Esc does not follow it with "[A" within milliseconds.
func (k *keyReader) read() (key, error) {
	r, size, err := k.br.ReadRune()
	if err != nil {
		return key{}, err
	}
	if r == utf8.RuneError && size == 1 {
		return key{code: kUnknown}, nil
	}
	if r != 0x1b {
		// "]" or "P" soon after a lone Esc may be the rest of a reply the
		// link split straight after its ESC.
		if (r == ']' || r == 'P') && !k.escAt.IsZero() && k.now().Sub(k.escAt) < replyWait && k.replyFollows(r) {
			k.escAt = time.Time{}
			return k.controlString(r, false)
		}
		k.escAt = time.Time{}
		return key{r: r}, nil
	}
	k.escAt = time.Time{}
	if !k.more() {
		k.escAt = k.now()
		return key{code: kEsc}, nil
	}
	next, _, err := k.br.ReadRune()
	if err != nil {
		return key{code: kEsc}, nil //nolint:nilerr // the Esc was a key; the next read reports the end
	}
	switch next {
	case '[':
		return k.csi()
	case ']', 'P', '_', '^', 'X':
		// A terminal's answer arrives whole, in one write; Alt+] or
		// Alt+Shift+P pressed by a person is those two bytes alone.
		if k.br.Buffered() > 0 || k.replyFollows(next) {
			return k.controlString(next, true)
		}
	case 'O':
		return k.ss3()
	case '\r', '\n':
		return key{code: kEnter, alt: true}, nil
	case 0x7f, 8:
		return key{r: 0x7f, alt: true}, nil
	case 0x1b:
		// Esc Esc: the first is a key of its own; hand the second back.
		_ = k.br.UnreadRune()
		return key{code: kEsc}, nil
	}
	return key{r: next, alt: true}, nil
}

// replyWait is how long a split reply may take to go on after "ESC ]" or
// "ESC P"; a person's Alt key waits this long before it is a key.
const replyWait = 150 * time.Millisecond

// replyTotal bounds the whole of a reply's arrival, however short its gaps.
const replyTotal = 2 * replyWait

// replyFollows reports whether what arrives within replyWait after "ESC ]"
// or "ESC P" begins as a terminal's answer does: "digits;" for an OSC, as
// the colour answer "11;rgb:…" does, and "1$r", "0$r", "1+r", "0+r" or ">|"
// for a DCS. A reply split by a slow link right after its introducer, or
// after its first digit, was otherwise read as Alt+] and then typed into the
// prompt. It waits again while what has come could still be either.
func (k *keyReader) replyFollows(kind rune) bool {
	if (kind != ']' && kind != 'P') || k.ready == nil {
		return false
	}
	for n := 0; n < 16; {
		if k.br.Buffered() <= n {
			if !k.ready(replyWait) {
				return false
			}
			if _, err := k.br.Peek(n + 1); err != nil {
				return false
			}
		}
		head, _ := k.br.Peek(k.br.Buffered())
		switch replyShape(kind, head) {
		case 1:
			return true
		case -1:
			return false
		}
		n = len(head)
	}
	return false
}

// replyShape says whether head begins as a reply to kind does: 1 yes, -1
// no, 0 not known until more arrives.
func replyShape(kind rune, head []byte) int {
	if kind == 'P' {
		shape := -1
		for _, p := range []string{"1$r", "0$r", "1+r", "0+r", ">|"} {
			n := min(len(head), len(p))
			if string(head[:n]) != p[:n] {
				continue
			}
			if n == len(p) {
				return 1
			}
			shape = 0
		}
		return shape
	}
	i := 0
	for i < len(head) && head[i] >= '0' && head[i] <= '9' {
		i++
	}
	switch {
	case i == len(head):
		return 0
	case i > 0 && head[i] == ';':
		return 1
	}
	return -1
}

// replyMax bounds a terminal reply that is read and set aside.
const replyMax = 4096

// controlString reads an OSC, DCS, APC, PM or SOS string to its end — BEL,
// or ESC backslash — and reports it as a reply. Such strings are how a
// terminal answers a query; read as keys, a late colour answer typed
// "11;rgb:…" into the prompt and its BEL was Ctrl-G, opening the editor.
//
// A reply arrives in a burst. When input pauses for replyWait, or replyMax
// bytes pass, before a terminator, it was not a reply: nothing is consumed,
// the introducer is returned as the key it was (alt: it followed an ESC) and
// what followed it is read again as typing.
func (k *keyReader) controlString(kind rune, alt bool) (key, error) {
	limit := min(replyMax, k.br.Size()-1)
	n := 0
	// A reply is a burst: typing that kept coming in short gaps was held,
	// unshown, until it stopped. Past replyTotal it is typing.
	deadline := k.now().Add(replyTotal)
	for {
		if n >= limit {
			return key{r: kind, alt: alt}, nil
		}
		if k.br.Buffered() <= n {
			wait := min(replyWait, deadline.Sub(k.now()))
			if k.ready != nil && (wait <= 0 || !k.ready(wait)) {
				return key{r: kind, alt: alt}, nil
			}
			if _, err := k.br.Peek(n + 1); err != nil && k.br.Buffered() <= n {
				// The input ended: what came is a reply cut short.
				body, _ := k.br.Peek(k.br.Buffered())
				out := key{code: kReply, paste: string(kind) + string(body)}
				_, _ = k.br.Discard(len(body))
				return out, nil
			}
		}
		buf, _ := k.br.Peek(k.br.Buffered())
		i := bytes.IndexAny(buf[n:], "\x07\x1b")
		if i < 0 {
			n = len(buf)
			continue
		}
		end := n + i
		out := key{code: kReply, paste: string(kind) + string(buf[:end])}
		st := buf[end] == 0x1b
		_, _ = k.br.Discard(end + 1)
		if st {
			// ESC backslash ends it; any other ESC starts a new sequence
			// and cuts this one short.
			if k.br.Buffered() > 0 || k.ready == nil || k.ready(escWait) {
				if b, err := k.br.Peek(1); err == nil && b[0] == '\\' {
					_, _ = k.br.Discard(1)
				}
			}
		}
		return out, nil
	}
}

// csi decodes "ESC [" params final.
func (k *keyReader) csi() (key, error) {
	var params strings.Builder
	var final rune
	for i := 0; i < 32; i++ {
		c, _, err := k.br.ReadRune()
		if err != nil {
			return key{code: kUnknown}, nil //nolint:nilerr // a cut-off sequence is a key not known; the next read reports the end
		}
		if c >= 0x40 && c <= 0x7e {
			final = c
			break
		}
		params.WriteRune(c)
	}
	if p := params.String(); strings.HasPrefix(p, "?") && (final == 'c' || final == 'y') || strings.HasSuffix(p, "$") && final == 'y' {
		return key{code: kReply, paste: "[" + p + string(final)}, nil
	}
	// Focus reports (DEC 1004): the terminal gained or lost focus.
	if params.Len() == 0 && (final == 'I' || final == 'O') {
		return key{code: kReply, paste: "[" + string(final)}, nil
	}
	ps := strings.Split(params.String(), ";")
	num := func(i int) int {
		if i >= len(ps) {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimLeft(ps[i], "<>=?"))
		return n
	}
	out := key{}
	applyMod(&out, num(1))

	switch final {
	case 'A':
		out.code = kUp
	case 'B':
		out.code = kDown
	case 'C':
		out.code = kRight
	case 'D':
		out.code = kLeft
	case 'H':
		out.code = kHome
	case 'F':
		out.code = kEnd
	case 'Z':
		out.code = kBackTab
		out.shift = true
	case 'u':
		// The kitty keyboard protocol and CSI-u: codepoint;modifiers.
		return csiU(num(0), num(1)), nil
	case '~':
		switch num(0) {
		case 1, 7:
			out.code = kHome
		case 4, 8:
			out.code = kEnd
		case 2:
			out.code = kInsert
		case 3:
			out.code = kDelete
		case 5:
			out.code = kPgUp
		case 6:
			out.code = kPgDn
		case 200:
			return k.paste()
		case 27:
			// xterm's modifyOtherKeys: 27;modifiers;codepoint.
			return csiU(num(2), num(1)), nil
		default:
			out.code = kUnknown
		}
	default:
		out.code = kUnknown
	}
	return out, nil
}

// csiU turns a codepoint and modifier pair into a key. Shift+Enter arrives
// this way on terminals that can tell it from Enter.
func csiU(cp, mod int) key {
	out := key{}
	applyMod(&out, mod)
	switch cp {
	case 13:
		if out.shift || out.alt || out.ctrl {
			out.code = kEnter
			return out
		}
		out.r = '\r'
	case 27:
		out.code = kEsc
	case 9:
		if out.shift {
			out.code = kBackTab
			return out
		}
		out.r = '\t'
	case 127:
		out.r = 0x7f
	default:
		if out.ctrl {
			// A control combination is a command, never text: map the ones
			// with a traditional control code and drop the rest, so Ctrl-_
			// cannot insert "-".
			switch {
			case cp >= 'a' && cp <= 'z':
				out.r = rune(cp - 'a' + 1)
			case cp == '-' || cp == '_' || cp == '/':
				out.r = keyCtrlUS
			case cp == '[':
				out.code = kEsc
			default:
				out.code = kUnknown
			}
			out.ctrl = false
			return out
		}
		if cp < 0x20 || cp > 0x10ffff || (cp >= 57344 && cp <= 63743) {
			// Control codes and the private-use range kitty uses for keys
			// such as Caps Lock and the keypad's own keys.
			out.code = kUnknown
			return out
		}
		out.r = rune(cp)
	}
	return out
}

func applyMod(k *key, mod int) {
	if mod < 2 {
		return
	}
	m := mod - 1
	k.shift = m&1 != 0
	k.alt = m&2 != 0 || m&8 != 0 // Alt, or Meta
	k.ctrl = m&4 != 0
}

// ss3 decodes "ESC O" final, which some terminals send for arrows and Home/End.
func (k *keyReader) ss3() (key, error) {
	c, _, err := k.br.ReadRune()
	if err != nil {
		return key{code: kUnknown}, nil //nolint:nilerr // a cut-off sequence is a key not known; the next read reports the end
	}
	switch c {
	case 'A':
		return key{code: kUp}, nil
	case 'B':
		return key{code: kDown}, nil
	case 'C':
		return key{code: kRight}, nil
	case 'D':
		return key{code: kLeft}, nil
	case 'H':
		return key{code: kHome}, nil
	case 'F':
		return key{code: kEnd}, nil
	case 'M':
		return key{code: kEnter, shift: true}, nil
	}
	return key{code: kUnknown}, nil
}

// pasteEnd is the bracketed-paste terminator.
const pasteEnd = "\x1b[201~"

// maxPaste bounds what one paste may hold, so a runaway paste cannot take the
// process's memory with it. Past it the rest of the paste is dropped.
const maxPaste = 8 << 20

// paste reads a bracketed paste up to its terminator. Line endings are
// normalised to "\n" so a paste from any platform edits the same way.
func (k *keyReader) paste() (key, error) {
	var b strings.Builder
	var tail []rune // the last runes read, to find the terminator past the cap
	for {
		r, _, err := k.br.ReadRune()
		if err != nil {
			break
		}
		tail = append(tail, r)
		if len(tail) > len(pasteEnd) {
			tail = tail[1:]
		}
		if r == '~' && string(tail) == pasteEnd {
			break
		}
		if b.Len() < maxPaste {
			b.WriteRune(r)
		}
	}
	text := strings.TrimSuffix(b.String(), pasteEnd[:len(pasteEnd)-1])
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return key{code: kPaste, paste: text}, nil //nolint:nilerr // a paste cut off by the end of input is still what was pasted
}

// Control codes, as the decoder reports them in key.r.
const (
	keyCtrlA  = 1
	keyCtrlB  = 2
	keyCtrlC  = 3
	keyCtrlD  = 4
	keyCtrlE  = 5
	keyCtrlF  = 6
	keyCtrlG  = 7
	keyCtrlH  = 8
	keyTab    = 9
	keyCtrlJ  = 10
	keyCtrlK  = 11
	keyCtrlL  = 12
	keyEnter  = 13
	keyCtrlN  = 14
	keyCtrlO  = 15
	keyCtrlP  = 16
	keyCtrlR  = 18
	keyCtrlS  = 19
	keyCtrlT  = 20
	keyCtrlU  = 21
	keyCtrlW  = 23
	keyCtrlY  = 25
	keyCtrlZ  = 26
	keyEsc    = 27
	keyCtrlUS = 31 // Ctrl-_ : undo
	keyDel    = 127
)
