package ui

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"
)

// dock is the interactive terminal surface: the input box the person types
// into, and everything drawn around it that changes in place — output that
// has not ended its line, a reply as it streams, the activity line, a dialog,
// queued messages and the footer. Finished output is committed above it into
// the terminal's own scrollback, and kept in a transcript so it can be drawn
// again at a new width or expanded with Ctrl-O.
//
// Keys are read on the dock's own goroutine, so a dialog always gets them,
// whatever the session is doing with the lines it has been handed.
type dock struct {
	mu  sync.Mutex
	scr *screen
	kr  *keyReader
	st  Style

	// size reports the terminal's columns and rows.
	size          func() (int, int)
	width, height int
	now           func() time.Time
	after         func(time.Duration, func())

	buf    inputBuf
	hist   *History
	hpos   int
	saved  string
	prompt string

	menu     []menuItem
	menuSel  int
	menuKind menuKind
	commands func() []Command
	files    func() []string

	searching bool
	sq        []rune
	sidx      int
	sOrig     snapshot

	busy bool
	act  activity
	spin int
	// streaming is set while a reply streams: the activity line gives way
	// to the reply itself.
	streaming bool
	// lastBlank is whether the last committed row was blank, so items are
	// separated by exactly one.
	lastBlank bool

	// rawTail is output that has not ended its line yet; live is a streamed
	// reply's rows that may still change.
	rawTail string
	live    []string

	// queued are messages typed while a turn runs, shown until the agent
	// takes them or the turn ends.
	queued []string

	dlg    *dialogState
	help   bool
	hint   string
	hintAt time.Time
	status func() StatusModel
	// statusSet is the model last given to SetStatus.
	statusSet StatusModel
	vim       *vimState
	pager     *pager
	missed    []block // committed while the transcript view was open
	tr        transcript
	stopped   bool // the dock no longer draws: the program is exiting

	lastEsc   time.Time
	lastCtrlC time.Time
	lastKey   time.Time

	// burst tracks keys read without a pause, which is how a paste looks on
	// a terminal that does not bracket it.
	burstKeys  int
	burstStart snapshot

	results chan readResult
	stops   chan struct{}
	// askSlot lets one dialog on screen at a time; inputEnded answers any
	// later one with its cancel choice.
	askSlot    chan struct{}
	inputEnded bool
	hotkeys    map[string]func()
	extEdit    func(string) (string, error)
}

type readResult struct {
	line string
	err  error
}

type menuKind int

const (
	menuNone menuKind = iota
	menuSlash
	menuFile
)

// menuItem is one suggestion: what the row shows and what accepting it puts
// on the line.
type menuItem struct {
	label  string
	detail string
	insert string
	// wait is true for a command that takes an argument: accepting it fills
	// the line and waits rather than submitting.
	wait bool
	// dir marks a directory in the file menu; accepting it keeps completing.
	dir bool
}

// activity is the line shown while a turn runs.
type activity struct {
	on     bool
	since  time.Time
	verb   int
	label  string
	tokens int
}

func newDock(in io.Reader, out io.Writer, st Style) *dock {
	d := &dock{
		scr:       newScreen(out),
		st:        st,
		size:      func() (int, int) { return 80, 24 },
		now:       time.Now,
		after:     func(dur time.Duration, f func()) { time.AfterFunc(dur, f) },
		hist:      &History{},
		menuSel:   -1,
		prompt:    Prompt(st),
		results:   make(chan readResult, 64),
		stops:     make(chan struct{}, 1),
		askSlot:   make(chan struct{}, 1),
		hotkeys:   map[string]func(){},
		lastBlank: true,
	}
	d.commands = commandList
	d.kr = newKeyReader(newBufReader(in))
	d.tr.max = 4000
	return d
}

// Reading.

// run reads keys until input ends. Everything it does happens under the
// lock; handlers that call back into the session run after it is released.
func (d *dock) run() {
	for {
		k, err := d.kr.read()
		if err != nil {
			d.mu.Lock()
			d.endDialogs()
			d.push(readResult{"", io.EOF})
			d.mu.Unlock()
			return
		}
		at := d.now()
		d.mu.Lock()
		post := d.key(k, at)
		d.lastKey = at
		// A burst of keys is drawn once, when it ends: a raw paste is one
		// frame rather than one per character.
		if d.kr.br.Buffered() == 0 {
			d.endBurst()
			d.draw()
		}
		d.mu.Unlock()
		if post != nil {
			post()
		}
	}
}

// push hands a result to ReadLine. The buffer is deep; a session that stops
// reading while a person keeps typing loses their lines rather than the
// terminal, which blocking here would freeze.
func (d *dock) push(r readResult) {
	select {
	case d.results <- r:
	default:
	}
}

func (d *dock) readLine() (string, error) {
	r := <-d.results
	if errors.Is(r.err, io.EOF) {
		d.push(r) // every later read ends too
	}
	return r.line, r.err
}

// key applies one key with the lock held, and returns work to run after it
// is released.
func (d *dock) key(k key, at time.Time) func() {
	gap := at.Sub(d.lastKey)
	if gap > 5*time.Millisecond || d.burstKeys == 0 {
		d.startBurst()
	}
	d.burstKeys++

	if d.pager != nil {
		d.pagerKey(k)
		return nil
	}
	if d.dlg != nil {
		d.dialogKey(k, at, gap)
		return nil
	}
	if d.searching && d.searchKey(k) {
		return nil
	}
	if !d.hintAt.IsZero() && at.Sub(d.hintAt) > 2*time.Second {
		d.hint, d.hintAt = "", time.Time{}
	}
	if d.vim != nil && d.vimKey(k) {
		d.afterEdit()
		return nil
	}

	switch k.code {
	case kPaste:
		d.help = false
		d.buf.paste(k.paste)
		d.afterEdit()
		return nil
	case kEnter:
		d.buf.insert([]rune{'\n'})
		d.afterEdit()
		return nil
	case kUp:
		if !k.alt && !k.ctrl {
			d.up()
		}
		return nil
	case kDown:
		if !k.alt && !k.ctrl {
			d.down()
		}
		return nil
	case kLeft:
		if k.alt || k.ctrl {
			d.buf.pos = d.buf.wordLeft()
		} else {
			d.buf.left()
		}
		return nil
	case kRight:
		if k.alt || k.ctrl {
			d.buf.pos = d.buf.wordRight()
		} else {
			d.buf.right()
		}
		return nil
	case kHome:
		d.buf.pos = d.buf.lineStart(d.buf.pos)
		return nil
	case kEnd:
		d.buf.pos = d.buf.lineEnd(d.buf.pos)
		return nil
	case kDelete:
		d.buf.deleteForward()
		d.afterEdit()
		return nil
	case kBackTab:
		return d.hotkey("shift+tab")
	case kEsc:
		return d.escape(at)
	case kNone:
	default:
		return nil
	}

	if k.alt {
		switch k.r {
		case 'b', 'B':
			d.buf.pos = d.buf.wordLeft()
		case 'f', 'F':
			d.buf.pos = d.buf.wordRight()
		case 'd', 'D':
			d.buf.cut(d.buf.pos, d.buf.wordRight(), true)
			d.afterEdit()
		case keyDel, keyCtrlH:
			d.buf.cut(d.buf.wordLeft(), d.buf.pos, true)
			d.afterEdit()
		case 'm', 'M':
			return d.hotkey("shift+tab") // where Shift-Tab cannot be told from Tab
		}
		return nil
	}

	switch k.r {
	case keyEnter:
		return d.enter()
	case keyCtrlJ:
		d.buf.insert([]rune{'\n'})
		d.afterEdit()
	case keyCtrlC:
		return d.ctrlC(at)
	case keyCtrlD:
		if d.buf.empty() && !d.busy {
			d.push(readResult{"", io.EOF})
			return nil
		}
		d.buf.deleteForward()
		d.afterEdit()
	case keyTab:
		d.complete()
	case keyDel, keyCtrlH:
		d.buf.backspace()
		d.afterEdit()
	case keyCtrlA:
		d.buf.pos = d.buf.lineStart(d.buf.pos)
	case keyCtrlE:
		d.buf.pos = d.buf.lineEnd(d.buf.pos)
	case keyCtrlB:
		d.buf.left()
	case keyCtrlF:
		d.buf.right()
	case keyCtrlP:
		d.up()
	case keyCtrlN:
		d.down()
	case keyCtrlU:
		d.buf.killToStart()
		d.afterEdit()
	case keyCtrlK:
		d.buf.killToEnd()
		d.afterEdit()
	case keyCtrlW:
		d.buf.cut(d.buf.wordLeftSpace(), d.buf.pos, true)
		d.afterEdit()
	case keyCtrlY:
		d.buf.yank()
		d.afterEdit()
	case keyCtrlUS:
		if d.buf.popUndo() {
			d.afterEdit()
		}
	case keyCtrlL:
		// Clear the screen and keep the session: the recovery for a display
		// something else has scribbled on.
		d.scr.raw("\x1b[H\x1b[2J")
		d.scr.forget()
	case keyCtrlR:
		d.startSearch()
	case keyCtrlO:
		d.openPager()
	case keyCtrlG:
		if d.extEdit != nil {
			return d.editExternally()
		}
	case keyCtrlT:
		return d.hotkey("ctrl+t")
	default:
		if k.r == '?' && d.buf.empty() && !d.busy {
			d.help = !d.help
			return nil
		}
		if unicode.IsPrint(k.r) || joins(k.r) {
			d.help = false
			d.buf.insert([]rune{k.r})
			d.afterEdit()
		}
	}
	return nil
}

// hotkey returns the session's handler for name, run after the lock is
// released, then redraws: most change what the footer shows.
func (d *dock) hotkey(name string) func() {
	f := d.hotkeys[name]
	if f == nil {
		return nil
	}
	return func() {
		f()
		d.mu.Lock()
		d.draw()
		d.mu.Unlock()
	}
}

// ctrlC is Ctrl-C as other agent CLIs have taught it: it clears what is
// typed; on an empty line it stops a running turn; at an idle empty prompt it
// warns, and a second press within two seconds exits.
func (d *dock) ctrlC(at time.Time) func() {
	switch {
	case len(d.menu) > 0 && !d.busy:
		d.closeMenu()
	case !d.buf.empty():
		d.remember(d.buf.expanded(), false) // cleared, not lost: Up brings it back
		d.buf.pushUndo(opOther)
		d.buf.set("")
		d.buf.pastes = nil
		d.afterEdit()
	case d.busy:
		d.push(readResult{"", errInterrupted})
	default:
		d.push(readResult{"", errInterrupted})
		if d.background() > 0 {
			return nil // the session's own warning covers background tasks
		}
		if at.Sub(d.lastCtrlC) < 2*time.Second {
			d.push(readResult{"", io.EOF})
			return nil
		}
		d.lastCtrlC = at
		d.flash("Press Ctrl-C again to exit")
	}
	return nil
}

func (d *dock) background() int {
	if d.status == nil {
		return 0
	}
	return d.statusModel().BackgroundTasks
}

// escape closes what is open, stops a running turn, and twice in quick
// succession on an idle prompt clears the line or asks to rewind.
func (d *dock) escape(at time.Time) func() {
	switch {
	case len(d.menu) > 0:
		d.closeMenu()
		return nil
	case d.help:
		d.help = false
		return nil
	case d.busy:
		select {
		case d.stops <- struct{}{}:
		default:
		}
		d.flash("interrupting…")
		return nil
	}
	double := at.Sub(d.lastEsc) < 700*time.Millisecond
	d.lastEsc = at
	if !double {
		if !d.buf.empty() {
			d.flash("Esc again to clear")
		}
		return nil
	}
	d.lastEsc = time.Time{}
	if !d.buf.empty() {
		d.remember(d.buf.expanded(), false)
		d.buf.pushUndo(opOther)
		d.buf.set("")
		d.afterEdit()
		return nil
	}
	return d.hotkey("esc esc")
}

// flash shows a hint in the footer for a couple of seconds.
func (d *dock) flash(s string) {
	d.hint, d.hintAt = s, d.now()
}

// enter submits the line, accepts a highlighted suggestion, or — after a
// backslash, or inside a raw paste — starts a new line.
func (d *dock) enter() func() {
	if d.menuSel >= 0 && d.menuSel < len(d.menu) {
		item := d.menu[d.menuSel]
		kind := d.menuKind
		d.acceptSuggestion()
		if kind == menuFile || item.wait {
			return nil
		}
	}
	if d.pasting() {
		d.buf.insert([]rune{'\n'})
		d.afterEdit()
		return nil
	}
	if d.buf.pos > 0 && d.buf.line[d.buf.pos-1] == '\\' {
		// The portable way to start a new line, which works in any terminal.
		d.buf.line = append(d.buf.line[:d.buf.pos-1], d.buf.line[d.buf.pos:]...)
		d.buf.pos--
		d.buf.insert([]rune{'\n'})
		d.afterEdit()
		return nil
	}
	shown := d.buf.String()
	out := d.buf.expanded()
	if strings.TrimSpace(out) == "" {
		d.buf.reset()
		d.afterEdit()
		return nil
	}
	// On disk a large paste stays its placeholder: the whole of it is for
	// this session's Up, not for a file that outlives it.
	d.hist.AddStored(out, shown)
	d.hpos = len(d.hist.Entries())
	d.buf.reset()
	d.help = false
	d.closeMenu()
	if d.busy {
		d.queued = append(d.queued, shown)
	} else {
		d.echo(shown)
	}
	d.push(readResult{out, nil})
	return nil
}

// remember adds s to the history and stops browsing it.
func (d *dock) remember(s string, persist bool) {
	d.hist.Add(s, persist)
	d.hpos = len(d.hist.Entries())
}

// pasting reports whether more input is already waiting: an Enter followed
// at once by more keys is a newline inside a paste, not a submit. A person
// cannot press another key within milliseconds of Enter.
func (d *dock) pasting() bool {
	if d.kr.br.Buffered() > 0 {
		return true
	}
	return d.kr.ready != nil && d.kr.ready(5*time.Millisecond)
}

// promptText is the prompt as drawn: the standard one in the current
// theme's colours, so a theme chosen after it was set still applies.
func (d *dock) promptText() string {
	if stripANSI(d.prompt) == Glyph+" " {
		return Prompt(d.st)
	}
	return d.prompt
}

// echo commits a submitted prompt to the transcript, where it was typed.
func (d *dock) echo(shown string) {
	d.commitItem(&promptBlock{text: shown, prompt: d.promptText()})
}

func (d *dock) startBurst() {
	d.burstKeys = 0
	d.burstStart = snapshot{append([]rune(nil), d.buf.line...), d.buf.pos}
}

// endBurst turns a raw paste of many lines into a placeholder, as a
// bracketed paste would have been: the terminal sent it as typing, but no
// person types forty lines in a millisecond.
func (d *dock) endBurst() {
	n := d.burstKeys
	d.burstKeys = 0
	if n < 32 || d.dlg != nil {
		return
	}
	before, now := d.burstStart.line, d.buf.line
	start := d.burstStart.pos
	if len(now) <= len(before) || start > len(before) || string(now[:start]) != string(before[:start]) {
		return
	}
	tailLen := len(before) - start
	end := len(now) - tailLen
	if end < start || string(now[end:]) != string(before[start:]) {
		return
	}
	pasted := string(now[start:end])
	if strings.Count(pasted, "\n") < pasteLines && len([]rune(pasted)) <= pasteChars {
		return
	}
	d.buf.line = append(append([]rune(nil), before[:start]...), before[start:]...)
	d.buf.pos = start
	d.buf.paste(pasted)
	d.afterEdit()
}

// History and lines.

func (d *dock) up() {
	switch {
	case len(d.menu) > 0:
		d.moveSelection(-1)
	case d.buf.onFirstLine():
		d.historyPrev()
	default:
		d.buf.up()
	}
}

func (d *dock) down() {
	switch {
	case len(d.menu) > 0:
		d.moveSelection(+1)
	case d.buf.onLastLine():
		d.historyNext()
	default:
		d.buf.down()
	}
}

func (d *dock) historyPrev() {
	h := d.hist.Entries()
	if d.hpos > len(h) {
		d.hpos = len(h)
	}
	if len(h) == 0 || d.hpos == 0 {
		return
	}
	if d.hpos == len(h) {
		d.saved = d.buf.String()
	}
	d.hpos--
	d.buf.set(h[d.hpos])
	d.buf.pastes = nil
	d.afterEdit()
}

func (d *dock) historyNext() {
	h := d.hist.Entries()
	if d.hpos >= len(h) {
		return
	}
	d.hpos++
	if d.hpos == len(h) {
		d.buf.set(d.saved)
	} else {
		d.buf.set(h[d.hpos])
	}
	d.afterEdit()
}

// Reverse search.

func (d *dock) startSearch() {
	h := d.hist.Entries()
	if !d.searching {
		d.searching = true
		d.sq = nil
		d.sidx = len(h)
		d.sOrig = snapshot{append([]rune(nil), d.buf.line...), d.buf.pos}
		d.closeMenu()
		return
	}
	d.searchFrom(d.sidx - 1) // Ctrl-R again: the next older match
}

// searchFrom finds the newest entry at or before i containing the query.
func (d *dock) searchFrom(i int) {
	h := d.hist.Entries()
	q := strings.ToLower(string(d.sq))
	for ; i >= 0; i-- {
		if i < len(h) && strings.Contains(strings.ToLower(h[i]), q) {
			d.sidx = i
			d.buf.set(h[i])
			return
		}
	}
}

// searchKey handles a key during reverse search, reporting whether it was
// consumed. Keys it does not own end the search, keeping the match.
func (d *dock) searchKey(k key) bool {
	h := d.hist.Entries()
	switch {
	case k.code == kNone && !k.alt && k.r == keyCtrlR:
		d.searchFrom(d.sidx - 1)
		return true
	case k.code == kEsc || (k.code == kNone && (k.r == keyCtrlG || k.r == keyCtrlC)):
		d.searching = false
		d.buf.line, d.buf.pos = d.sOrig.line, d.sOrig.pos
		return true
	case k.code == kNone && !k.alt && (k.r == keyDel || k.r == keyCtrlH):
		if len(d.sq) > 0 {
			d.sq = d.sq[:len(d.sq)-1]
		}
		d.searchFrom(len(h) - 1)
		return true
	case k.code == kNone && !k.alt && k.r == keyEnter:
		d.searching = false // take the match onto the line, to edit or send
		d.hpos = len(h)
		return true
	case k.code == kNone && !k.alt && unicode.IsPrint(k.r):
		d.sq = append(d.sq, k.r)
		d.searchFrom(min(d.sidx, len(h)-1))
		return true
	}
	d.searching = false
	d.hpos = len(h)
	return false
}

// Suggestions.

// afterEdit recomputes the menu from the line as it now is. It runs after
// every edit, on the current line, so deleting the last character of a
// command finds nothing to show.
func (d *dock) afterEdit() {
	var want string
	if d.menuSel >= 0 && d.menuSel < len(d.menu) {
		want = d.menu[d.menuSel].label
	}
	d.menu, d.menuKind = nil, menuNone
	word := strings.TrimLeft(d.buf.String(), " ")
	switch {
	case strings.HasPrefix(word, "/") && !strings.ContainsAny(word, " \t\n") && d.vimInsert():
		for _, c := range matchCommands(d.commands(), word) {
			d.menu = append(d.menu, menuItem{label: c.Name, detail: c.Help, insert: c.Name,
				wait: c.Args != "" && !strings.HasPrefix(c.Args, "[")})
		}
		d.menuKind = menuSlash
	default:
		if tok, _ := d.mentionToken(); tok != "" && d.files != nil {
			d.menu = fileMatches(d.files(), tok[1:], 50)
			d.menuKind = menuFile
		}
	}
	d.menuSel = -1
	for i, it := range d.menu {
		if want != "" && it.label == want {
			d.menuSel = i
			break
		}
	}
	if len(d.menu) == 0 {
		d.menuKind = menuNone
	}
}

// matchCommands ranks commands for what is typed: a prefix match first, then
// a fuzzy one, so "/cm" still finds /compact.
func matchCommands(all []Command, typed string) []Command {
	var prefix, fuzzy []Command
	q := strings.ToLower(strings.TrimPrefix(typed, "/"))
	for _, c := range all {
		name := strings.ToLower(strings.TrimPrefix(c.Name, "/"))
		switch {
		case strings.HasPrefix(name, q):
			prefix = append(prefix, c)
		default:
			if _, ok := subsequence(name, q); ok && q != "" {
				fuzzy = append(fuzzy, c)
			}
		}
	}
	return append(prefix, fuzzy...)
}

// mentionToken returns the "@..." word the cursor is at the end of, and where
// it starts; "" when the cursor is not in one.
func (d *dock) mentionToken() (string, int) {
	line := d.buf.line
	i := d.buf.pos
	for i > 0 && !unicode.IsSpace(line[i-1]) {
		i--
	}
	if i < len(line) && line[i] == '@' && (d.buf.pos == len(line) || unicode.IsSpace(line[d.buf.pos])) {
		return string(line[i:d.buf.pos]), i
	}
	return "", 0
}

func (d *dock) closeMenu() {
	d.menu, d.menuSel, d.menuKind = nil, -1, menuNone
}

func (d *dock) moveSelection(delta int) {
	if len(d.menu) == 0 {
		return
	}
	switch {
	case d.menuSel < 0 && delta > 0:
		d.menuSel = 0
	case d.menuSel < 0:
		d.menuSel = len(d.menu) - 1
	default:
		d.menuSel = (d.menuSel + delta + len(d.menu)) % len(d.menu)
	}
}

// acceptSuggestion puts the highlighted item on the line.
func (d *dock) acceptSuggestion() {
	it := d.menu[d.menuSel]
	kind := d.menuKind
	d.closeMenu()
	if kind == menuFile {
		_, start := d.mentionToken()
		ins := "@" + it.insert
		if !it.dir {
			ins += " "
		}
		d.buf.pushUndo(opOther)
		tail := append([]rune(nil), d.buf.line[d.buf.pos:]...)
		d.buf.line = append(append(d.buf.line[:start], []rune(ins)...), tail...)
		d.buf.pos = start + len([]rune(ins))
		d.afterEdit() // a directory keeps completing into itself
		return
	}
	out := it.insert
	if it.wait {
		out += " "
	}
	d.buf.set(out)
	d.afterEdit()
}

// complete is Tab: take the highlight if there is one, otherwise complete as
// far as the candidates unambiguously allow. With no menu, Tab after a paste
// placeholder opens the paste for editing.
func (d *dock) complete() {
	if len(d.menu) == 0 {
		if d.buf.expandPlaceholder() {
			d.afterEdit()
		}
		return
	}
	if d.menuSel >= 0 || len(d.menu) == 1 {
		if d.menuSel < 0 {
			d.menuSel = 0
		}
		d.acceptSuggestion()
		return
	}
	if d.menuKind == menuSlash {
		typed := strings.TrimSpace(d.buf.String())
		var prefixed []Command
		for _, c := range d.commands() {
			if strings.HasPrefix(c.Name, typed) {
				prefixed = append(prefixed, c)
			}
		}
		if p := CommonPrefix(prefixed); len(p) > len(typed) {
			d.buf.set(p)
			d.afterEdit()
			return
		}
	}
	d.menuSel = 0
}

// editExternally opens the line in $VISUAL or $EDITOR.
func (d *dock) editExternally() func() {
	text := d.buf.expanded()
	d.scr.clear()
	d.stopped = true
	return func() {
		got, err := d.extEdit(text)
		d.mu.Lock()
		defer d.mu.Unlock()
		d.stopped = false
		d.scr.forget()
		if err != nil {
			d.flash("editor: " + err.Error())
			d.draw()
			return
		}
		d.buf.pastes = nil
		d.buf.pushUndo(opOther)
		d.buf.set(strings.TrimRight(got, "\n"))
		d.afterEdit()
		d.draw()
	}
}

// Output.

// liveRows is how many rows of a streaming reply the dock holds before
// they are committed: a third of the screen.
func (d *dock) liveRows() int { return max(3, d.height/3) }

// streamWidth is the width a reply is laid out at, inside its indent.
func (d *dock) streamWidth() int { return max(d.contentWidth()-2, 10) }

// streamRows commits rows of a streaming reply, adding them to its block,
// and shows live as the rows still changing.
func (d *dock) streamRows(b *streamBlock, commit, live []string) {
	d.live = live
	if len(commit) == 0 {
		d.draw()
		return
	}
	if len(b.rows) == 0 {
		d.tr.add(b)
	}
	b.rows = append(b.rows, commit...)
	d.commitLines(commit)
}

// commitLines writes finished rows above the region.
func (d *dock) commitLines(lines []string) {
	if d.stopped {
		return
	}
	if d.pager != nil {
		d.missed = append(d.missed, &rowsBlock{rows: lines})
		return
	}
	d.measure()
	d.flushTail(&lines)
	if len(lines) > 0 {
		d.lastBlank = stripANSI(lines[len(lines)-1]) == ""
	}
	rows, cr, cc := d.frame()
	d.scr.commit(lines, rows, cr, cc)
}

// separate puts one blank row before a new item, unless there is one.
func (d *dock) separate() {
	if !d.lastBlank {
		d.commit(&rawBlock{text: ""})
	}
}

// commitItem commits b as a new item of the transcript: a reply, a tool
// call, a prompt, a dialog's record.
func (d *dock) commitItem(b block) {
	d.separate()
	d.commit(b)
}

// dequeue drops a queued message the agent has now taken.
func (d *dock) dequeue(text string) {
	for i, q := range d.queued {
		if q == text || strings.TrimSpace(q) == strings.TrimSpace(text) {
			d.queued = append(d.queued[:i], d.queued[i+1:]...)
			return
		}
	}
	if len(d.queued) > 0 {
		d.queued = d.queued[1:] // it was expanded or edited on its way: the oldest went
	}
}

// flushLive commits a streamed reply's rows as they stand.
func (d *dock) flushLive() {
	if len(d.live) == 0 {
		return
	}
	rows := d.live
	d.live = nil
	d.commit(&rowsBlock{rows: rows})
}

// commit appends a finished block to the transcript and draws it above the
// region.
func (d *dock) commit(b block) {
	d.tr.add(b)
	if d.stopped {
		return
	}
	if d.pager != nil {
		d.missed = append(d.missed, b)
		return
	}
	d.measure()
	lines := b.lines(d.contentWidth(), d.st, false)
	d.flushTail(&lines)
	if len(lines) > 0 {
		d.lastBlank = stripANSI(lines[len(lines)-1]) == ""
	}
	rows, cr, cc := d.frame()
	d.scr.commit(lines, rows, cr, cc)
}

// flushTail puts output that has not ended its line in front of lines being
// committed, so the two are not interleaved.
func (d *dock) flushTail(lines *[]string) {
	if d.rawTail == "" {
		return
	}
	tail := d.rawTail
	d.rawTail = ""
	d.tr.add(&rawBlock{text: tail})
	*lines = append(hardWrap(tail, d.contentWidth()), *lines...)
}

// write is output from the program: complete lines are committed, and a
// trailing partial line is held in the region until its newline arrives.
func (d *dock) write(p []byte) {
	text := d.rawTail + sanitize(string(p), true)
	i := strings.LastIndexByte(text, '\n')
	if i < 0 {
		d.rawTail = text
		d.draw()
		return
	}
	d.rawTail = ""
	d.commit(&rawBlock{text: text[:i]})
	d.rawTail = text[i+1:]
	if d.rawTail != "" {
		d.draw()
	}
}

// Drawing.

func (d *dock) measure() {
	w, h := d.size()
	if w < 20 {
		w = 20
	}
	if h < 6 {
		h = 6
	}
	d.width, d.height = w, h
}

// contentWidth is what a row may use: one column is always left free.
func (d *dock) contentWidth() int { return max(d.width-1, 19) }

func (d *dock) draw() {
	if d.stopped || d.pager != nil {
		return
	}
	d.measure()
	rows, cr, cc := d.frame()
	d.scr.render(rows, cr, cc)
}

// frame lays the region out: rows top to bottom and where the cursor goes.
func (d *dock) frame() (rows []string, cr, cc int) {
	if d.pager != nil {
		return nil, 0, 0
	}
	w := d.contentWidth()
	s := d.st
	maxRows := d.height - 1

	var top []string
	if d.rawTail != "" {
		top = append(top, hardWrap(d.rawTail, w)...)
	}
	top = append(top, d.live...)
	if d.act.on && !d.streaming && d.dlg == nil {
		top = append(top, d.activityRow(w))
	}

	var mid []string
	cursorRow, cursorCol := -1, 0
	switch {
	case d.dlg != nil:
		mid = d.dlg.rows(d, w, maxRows-len(top)-2)
	default:
		for i, q := range d.queued {
			if i == 3 && len(d.queued) > 4 {
				mid = append(mid, s.Dim(fmt.Sprintf("  ↳ … %d more queued", len(d.queued)-3)))
				break
			}
			q = strings.ReplaceAll(q, "\n", " ⏎ ")
			mid = append(mid, s.Dim(truncateWidth("  ↳ "+q, w)))
		}
		if len(top) > 0 || len(mid) > 0 {
			mid = append(mid, "")
		}
		rule := s.Dim(strings.Repeat("─", w))
		mid = append(mid, rule)
		in, r, c := d.layout(w)
		// A long entry shows a window around the cursor.
		room := max(3, maxRows-len(top)-len(mid)-4)
		if len(in) > room {
			start := min(max(0, r-room/2), len(in)-room)
			in = in[start : start+room]
			r -= start
		}
		cursorRow, cursorCol = len(mid)+r, c
		mid = append(mid, in...)
		mid = append(mid, rule)
	}

	below := d.belowRows(w)

	// Keep the region within the screen: output rows give way first, since
	// they are also on their way into the transcript.
	if extra := len(top) + len(mid) + len(below) - maxRows; extra > 0 && len(top) > 0 {
		top = top[min(extra, len(top)):]
	}
	rows = append(append(top, mid...), below...)
	if len(rows) > maxRows {
		rows = rows[len(rows)-maxRows:]
		cursorRow -= len(top) + len(mid) + len(below) - maxRows
	}
	if cursorRow < 0 {
		return rows, max(len(rows)-1, 0), 0
	}
	return rows, len(top) + cursorRow, cursorCol
}

// layout breaks the line into screen rows at width w, and says where the
// cursor falls.
func (d *dock) layout(w int) (rows []string, curRow, curCol int) {
	prompt := d.promptText()
	if d.searching {
		prompt = d.st.Dim("search: ") + string(d.sq) + d.st.Dim(" ▸ ")
	} else if d.vim != nil && !d.vim.insert {
		prompt = d.st.Dim("N ") + prompt
	}
	pw := displayWidth(prompt)
	indent := strings.Repeat(" ", pw)
	var row strings.Builder
	row.WriteString(prompt)
	col := pw
	line := d.buf.line
	placed := false
	for i := 0; i < len(line); {
		e := clusterEnd(line, i)
		if i == d.buf.pos {
			curRow, curCol, placed = len(rows), col, true
		}
		if line[i] == '\n' {
			rows = append(rows, row.String())
			row.Reset()
			row.WriteString(indent)
			col = pw
			i = e
			continue
		}
		cw := clusterWidth(line[i:e])
		if col+cw > w {
			rows = append(rows, row.String())
			row.Reset()
			row.WriteString(indent)
			col = pw
			if i == d.buf.pos {
				curRow, curCol = len(rows), col
			}
		}
		row.WriteString(string(line[i:e]))
		col += cw
		i = e
	}
	if !placed {
		curRow, curCol = len(rows), col
		if col >= w {
			rows = append(rows, row.String())
			row.Reset()
			row.WriteString(indent)
			curRow, curCol = len(rows), pw
		}
	}
	rows = append(rows, row.String())
	return rows, curRow, curCol
}

// belowRows is what sits under the input: the menu while one is open, the
// shortcut help after "?", otherwise the footer.
func (d *dock) belowRows(w int) []string {
	s := d.st
	var out []string
	switch {
	case len(d.menu) > 0 && d.dlg == nil:
		const menuMax = 8
		start := 0
		if d.menuSel >= menuMax {
			start = d.menuSel - menuMax + 1
		}
		end := min(start+menuMax, len(d.menu))
		labelW := 0
		for _, it := range d.menu[start:end] {
			labelW = max(labelW, displayWidth(it.label))
		}
		labelW = min(labelW, w/2)
		for i := start; i < end; i++ {
			it := d.menu[i]
			label := truncateWidth(it.label, labelW)
			pad := strings.Repeat(" ", labelW-displayWidth(label))
			detail := truncateWidth(it.detail, max(0, w-labelW-6))
			if i == d.menuSel {
				out = append(out, "  "+s.Reverse(" "+label+pad+"  "+detail+" "))
			} else {
				out = append(out, "  "+s.Accent(label)+pad+"   "+s.Dim(detail))
			}
		}
		if more := len(d.menu) - (end - start); more > 0 {
			out = append(out, "  "+s.Dim(fmt.Sprintf("… %d more", more)))
		}
	case d.help && d.dlg == nil:
		out = shortcutHelp(s, w)
	default:
		out = d.footerRows(w)
	}
	return out
}

// shortcuts is the help "?" shows, as pairs.
var shortcuts = [][2]string{
	{"/ for commands", "shift+enter, alt+enter or \\⏎ newline"},
	{"@ for file paths", "ctrl+r search history"},
	{"shift+tab cycle mode", "ctrl+o transcript and full output"},
	{"esc interrupt", "ctrl+g edit in $EDITOR"},
	{"ctrl+c clear, then exit", "ctrl+l clear screen · ctrl+_ undo"},
	{"tab expand a paste", "ctrl+u/k/w cut · ctrl+y paste back"},
}

func shortcutHelp(s Style, w int) []string {
	col := 26
	var out []string
	for _, p := range shortcuts {
		if w > col*2+6 {
			out = append(out, "  "+s.Dim(padRight(p[0], col)+p[1]))
			continue
		}
		out = append(out, "  "+s.Dim(truncateWidth(p[0], w-2)))
		out = append(out, "  "+s.Dim(truncateWidth(p[1], w-2)))
	}
	return out
}
