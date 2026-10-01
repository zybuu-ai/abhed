package ui

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// LineReader is the interactive terminal: the input dock, the keys, and the
// output drawn above them.
//
// When stdin is not a terminal — a pipe, a CI job, a here-doc — it falls
// straight through to line reads, because raw mode on a pipe would corrupt
// the input and there is nobody typing to benefit from it.
type LineReader struct {
	d       *dock
	fd      int
	state   *term.State
	fallbck *bufio.Reader
	raw     bool
	done    chan struct{}
	close   sync.Once
	// resize, for a reader over a stream, sets the size it reports.
	resize func(cols, rows int)
	// tty is the terminal itself, kept before Capture replaces os.Stdout,
	// for restoring it after a panic without the dock's lock.
	tty *os.File
}

// activeReader is the terminal to restore if the program panics.
var activeReader atomic.Pointer[LineReader]

// RestoreOnPanic, deferred at the top of a goroutine, puts the terminal back
// in its ordinary mode before a panic ends the program, then lets the panic
// go on: a shell left in raw mode, with bracketed paste and the keyboard
// protocol on, is unusable. It takes no lock, since the panic may have
// happened while one was held.
func RestoreOnPanic() {
	if r := recover(); r != nil {
		if l := activeReader.Load(); l != nil {
			l.emergencyRestore()
		}
		panic(r)
	}
}

func (l *LineReader) emergencyRestore() {
	if l.tty != nil {
		_, _ = l.tty.WriteString("\x1b[0m" + modesOff + "\r\n")
	}
	if l.state != nil {
		_ = term.Restore(l.fd, l.state)
	}
}

// Terminal modes the dock turns on while it runs, and off when it closes:
// bracketed paste, so a paste arrives as one; and the keyboard protocol's
// "disambiguate" level, so Shift+Enter and a lone Esc can be told apart.
// A terminal that knows neither ignores both.
const (
	modesOn  = "\x1b[?2004h\x1b[>1u"
	modesOff = "\x1b[<u\x1b[?2004l\x1b[?25h"
)

// errInterrupted is Ctrl-C on an empty line: the session decides whether it
// stops a turn or, pressed again, ends the session.
var errInterrupted = errors.New("interrupted")

// ErrCtrlC is what ReadLine returns for Ctrl-C, for callers that script it.
var ErrCtrlC = errInterrupted

// ErrInterrupted reports whether a read ended in Ctrl-C.
func ErrInterrupted(err error) bool { return errors.Is(err, errInterrupted) }

// NewLineReader prepares stdin for editing where that is possible.
func NewLineReader(prompt string) *LineReader {
	fd := int(os.Stdin.Fd())
	outFd := int(os.Stdout.Fd())
	if !term.IsTerminal(fd) || !term.IsTerminal(outFd) || dumbTerminal() {
		return &LineReader{fallbck: bufio.NewReader(os.Stdin)}
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return &LineReader{fallbck: bufio.NewReader(os.Stdin)}
	}
	// The theme: the one saved with /theme, else what the environment says,
	// else what this terminal said about its background — remembered from
	// before, or asked now. The question is sent either way when the theme
	// is not chosen, and a late answer is applied when it comes; startup
	// waits for it only the first time a terminal is seen, and briefly.
	u := loadPrefs()
	theme, vim := u.Theme, u.Vim
	if theme == "auto" {
		theme = ""
	}
	if theme == "" {
		theme = ThemeFromEnv()
	}
	auto := theme == ""
	cached := u.Terminals[terminalID()]
	syncMode := -1
	if cached.Sync != nil {
		syncMode = *cached.Sync
	}
	var typed []byte
	if auto && cached.Theme != "" {
		theme = cached.Theme
	}
	if auto || syncMode < 0 {
		wait := probeWait
		if cached.Theme != "" || !auto {
			wait = 0 // what is known is used now; the answer refreshes it
		}
		t, sy, rest := probeTerminal(os.Stdin, os.Stdout, wait)
		typed = rest
		if auto && t != "" {
			theme = t
			rememberTerminal(t, -1)
		}
		if sy >= 0 {
			syncMode = sy
			rememberTerminal("", sy)
		}
	}
	if SetTheme(theme) != nil {
		_ = SetTheme("dark")
	}
	var in io.Reader = os.Stdin
	if len(typed) > 0 {
		in = io.MultiReader(bytes.NewReader(typed), os.Stdin)
	}
	d := newDock(in, os.Stdout, NewStyle(LazyStdout{}))
	d.prompt = prompt
	d.autoTheme = auto
	d.scr.sync = syncMode == 1
	d.detected = func(theme string, sync int) { go rememberTerminal(theme, sync) }
	if vim {
		d.setVim(true)
	}
	d.kr.ready = readyFunc(os.Stdin)
	d.size = func() (int, int) {
		w, h, err := term.GetSize(outFd)
		if err != nil || w <= 0 {
			return 80, 24
		}
		return w, h
	}
	// The terminal is kept now: Capture later swaps os.Stdout and os.Stderr
	// for pipes, and the editor needs the terminal on all three fds.
	ttyIn, ttyOut := os.Stdin, os.Stdout
	d.extEdit = func(text string) (string, error) { return externalEdit(fd, state, ttyIn, ttyOut, text) }
	go cleanDrafts()
	l := &LineReader{d: d, fd: fd, state: state, raw: true, done: make(chan struct{}), tty: os.Stdout}
	activeReader.Store(l)
	d.scr.raw(modesOn)
	l.start()
	watchResize(l.done, d.resized)
	return l
}

// NewLineReaderOn builds a reader over any stream with the terminal's
// behaviour and a fixed size: the dock with no terminal to configure. Tests
// drive it with keys and read the frames it writes.
func NewLineReaderOn(in io.Reader, out io.Writer, cols, rows int) *LineReader {
	d := newDock(in, out, Style{enabled: true})
	var mu sync.Mutex
	d.size = func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return cols, rows
	}
	l := &LineReader{d: d, raw: true, done: make(chan struct{})}
	l.resize = func(c, r int) {
		mu.Lock()
		cols, rows = c, r
		mu.Unlock()
		d.resized()
	}
	l.start()
	return l
}

func (l *LineReader) start() {
	l.d.mu.Lock()
	l.d.draw()
	l.d.mu.Unlock()
	go l.d.run()
	go l.tick()
}

// tick drives what changes with time: the activity line's spinner and
// clock, a hint's expiry, and — where no resize signal exists — the size.
func (l *LineReader) tick() {
	defer RestoreOnPanic()
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-t.C:
			l.d.tickFrame()
		}
	}
}

func (d *dock) tickFrame() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	redraw := false
	if d.act.on {
		d.spin++
		if d.spin%40 == 0 {
			d.act.verb++ // a new word every four seconds reads as progress
		}
		redraw = len(d.live) == 0 && d.pager == nil
	}
	if !d.hintAt.IsZero() && d.now().Sub(d.hintAt) > 2*time.Second {
		d.hint, d.hintAt = "", time.Time{}
		redraw = true
	}
	if !resizeSignalled {
		if w, h := d.size(); w != d.width || h != d.height {
			d.repaint()
			return
		}
	}
	if redraw {
		d.draw()
	}
}

// resized is called when the terminal reports a new size.
func (d *dock) resized() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.repaint()
}

// repaint draws the screen again from the transcript, at the current size.
//
// A terminal reflows its own rows on a resize, each terminal differently,
// and a region drawn at the old width no longer sits where its rows say.
// Rather than guess, the visible screen is cleared and the tail of the
// transcript drawn again at the new width, with the region under it: no
// ghost of the old dock is left, and the text is wrapped for the new width
// rather than cut by the terminal. Scrollback keeps what was above.
func (d *dock) repaint() {
	if d.stopped {
		return
	}
	d.measure()
	if d.pager != nil {
		d.drawPager()
		return
	}
	d.scr.raw("\x1b[H\x1b[2J")
	d.scr.forget()
	lines := d.tr.tail(d.contentWidth(), d.st, 2*d.height)
	rows, cr, cc := d.frame()
	d.scr.commit(lines, rows, cr, cc)
}

// ReadLine returns the next line, or an error: ErrCtrlC for Ctrl-C, io.EOF
// when input ends.
func (l *LineReader) ReadLine() (string, error) {
	if l.raw {
		return l.d.readLine()
	}
	line, err := l.fallbck.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

// Typing reports whether the person has something on the line: a wake turn
// waits, since their message will carry the result anyway.
func (l *LineReader) Typing() bool {
	if !l.raw {
		return false
	}
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	return !l.d.buf.empty()
}

// Raw reports whether editing is active, so a caller can print its own prompt
// when it is not.
func (l *LineReader) Raw() bool { return l.raw }

// Quiet marks a turn as running (true) or finished (false). The dock stays
// on screen either way: what is typed during a turn is shown as it is typed,
// and Esc stops the turn.
func (l *LineReader) Quiet(q bool) {
	if !l.raw {
		return
	}
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	l.d.busy = q
	if q {
		l.d.next = "" // a new turn takes the offered prompt away
	}
	if !q {
		l.d.queued = nil
		l.d.act = activity{}
		l.d.flushLive()
	}
	l.d.closeMenu()
	l.d.draw()
}

// SetPrompt changes the prompt shown before the cursor.
func (l *LineReader) SetPrompt(p string) {
	if l.raw {
		l.d.mu.Lock()
		l.d.prompt = p
		l.d.draw()
		l.d.mu.Unlock()
	}
}

// Stops delivers Esc pressed while a turn runs: stop the turn. Unlike Ctrl-C
// it never counts toward exiting. Nil when there is no terminal.
func (l *LineReader) Stops() <-chan struct{} {
	if !l.raw {
		return nil
	}
	return l.d.stops
}

// SetStatusFunc supplies what the footer shows; it is asked on every draw,
// so a change such as /model shows at once.
func (l *LineReader) SetStatusFunc(f func() StatusModel) {
	if l.raw {
		l.d.mu.Lock()
		l.d.status = f
		l.d.draw()
		l.d.mu.Unlock()
	}
}

// SetHistory replaces the in-memory history with h.
func (l *LineReader) SetHistory(h *History) {
	if l.raw && h != nil {
		l.d.mu.Lock()
		l.d.hist = h
		l.d.hpos = len(h.Entries())
		l.d.mu.Unlock()
	}
}

// Hotkey runs f when the named key is pressed at the prompt: "shift+tab",
// "ctrl+t", "esc esc". f runs outside the dock's lock and may call back.
func (l *LineReader) Hotkey(name string, f func()) {
	if l.raw {
		l.d.mu.Lock()
		l.d.hotkeys[name] = f
		l.d.mu.Unlock()
	}
}

// SetCommands supplies the slash commands the menu offers.
func (l *LineReader) SetCommands(f func() []Command) {
	if l.raw && f != nil {
		l.d.mu.Lock()
		l.d.commands = f
		l.d.mu.Unlock()
	}
}

// SetFiles supplies workspace paths for @ completion.
func (l *LineReader) SetFiles(f func() []string) {
	if l.raw {
		l.d.mu.Lock()
		l.d.files = f
		l.d.mu.Unlock()
	}
}

// SetVim turns modal (vim) editing of the input on or off.
func (l *LineReader) SetVim(on bool) {
	if l.raw {
		l.d.mu.Lock()
		l.d.setVim(on)
		l.d.draw()
		l.d.mu.Unlock()
	}
}

// Vim reports whether modal editing is on.
func (l *LineReader) Vim() bool {
	if !l.raw {
		return false
	}
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	return l.d.vim != nil
}

// Flash shows a hint in the footer for a moment.
func (l *LineReader) Flash(s string) {
	if l.raw {
		l.d.mu.Lock()
		l.d.flash(s)
		l.d.draw()
		l.d.mu.Unlock()
	}
}

// SetAutoTheme says whether the terminal's answers about its background may
// set the theme: false once a theme is chosen, true for /theme auto, which
// also asks the terminal again.
func (l *LineReader) SetAutoTheme(auto bool) {
	if !l.raw {
		return
	}
	l.d.mu.Lock()
	l.d.autoTheme = auto
	if auto {
		l.d.scr.raw("\x1b]11;?\x07")
	}
	l.d.mu.Unlock()
}

// Repaint draws the screen again from the transcript, as a resize does:
// after a theme change, so what is on screen is in the new colours.
func (l *LineReader) Repaint() {
	if l.raw {
		l.d.resized()
	}
}

// Write prints through the terminal so output does not collide with the
// dock. Outside raw mode it goes to stdout unchanged.
func (l *LineReader) Write(p []byte) (int, error) {
	if l.raw {
		l.d.mu.Lock()
		l.d.write(p)
		l.d.mu.Unlock()
		return len(p), nil
	}
	return os.Stdout.Write(p)
}

// Close restores the terminal. Leaving it in raw mode makes the user's shell
// unusable afterwards, which is a worse failure than anything this package
// does, so callers must defer it.
func (l *LineReader) Close() {
	if !l.raw {
		return
	}
	l.close.Do(func() {
		activeReader.CompareAndSwap(l, nil)
		close(l.done)
		l.d.mu.Lock()
		l.d.endDialogs()
		if l.d.pager != nil {
			l.d.closePager()
		}
		l.d.flushLive()
		if l.d.rawTail != "" {
			tail := l.d.rawTail
			l.d.rawTail = ""
			l.d.commit(&rawBlock{text: tail})
		}
		l.d.scr.clear()
		l.d.stopped = true
		l.d.scr.raw(modesOff)
		l.d.mu.Unlock()
		if l.state != nil {
			_ = term.Restore(l.fd, l.state)
		}
	})
}

// externalEdit opens text in $VISUAL or $EDITOR with the terminal in its
// ordinary mode, and returns what was saved. in and out are the terminal,
// never the capture pipes.
func externalEdit(fd int, raw *term.State, in, out *os.File, text string) (string, error) {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	name, err := newDraft(text)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(name) }()
	if raw != nil {
		_ = term.Restore(fd, raw)
	}
	_, _ = out.WriteString(modesOff)
	fields := strings.Fields(editor)
	cmd := exec.Command(fields[0], append(fields[1:], name)...) // #nosec G204 G702 -- the person's own editor, on their own file
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, out
	runErr := cmd.Run()
	if _, err := term.MakeRaw(fd); err != nil {
		return "", err
	}
	_, _ = out.WriteString(modesOn)
	if runErr != nil {
		return "", runErr
	}
	return readDraft(name)
}

// draftMax bounds what is read back from the editor.
const draftMax = 8 << 20

// draftDir is where a prompt is written for the person's editor: under
// ~/.abhed, in a folder only they can open, which the sandbox keeps the
// agent out of. The shared temporary folder is one a sandboxed command can
// write, where a running task could read the draft, change it, or swap it
// for a link to a file it cannot read.
func draftDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("no home folder for the draft")
	}
	dir := filepath.Join(home, ".abhed", "drafts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// A link in its place, or a folder someone else owns, is not used:
	// the draft is read back into the person's input box.
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ownedByMe(info) {
		return "", fmt.Errorf("%s is not a folder of yours; not writing the draft there", dir)
	}
	return dir, os.Chmod(dir, 0o700) // #nosec G302 -- a folder needs its search bit; only its owner has any
}

// newDraft writes text to a new file of its own in draftDir.
func newDraft(text string) (string, error) {
	dir, err := draftDir()
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "prompt-*.md") // created exclusively, 0600
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), f.Close()
}

// readDraft reads the draft back, never through a link.
func readDraft(name string) (string, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|noFollow, 0) // #nosec G304 -- the draft this process made
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, draftMax))
	return string(data), err
}

// cleanDrafts removes drafts a crash left behind: those a day old, since
// another session may have one open in an editor now.
func cleanDrafts() {
	dir, err := draftDir()
	if err != nil {
		return
	}
	names, _ := filepath.Glob(filepath.Join(dir, "prompt-*.md"))
	for _, n := range names {
		if info, err := os.Lstat(n); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.Remove(n)
		}
	}
}

// Capture redirects the process's standard output and error into the dock,
// and returns a function that restores them.
//
// Redirecting the streams rather than handing callers a writer is deliberate:
// fmt.Println and every log line in the program write to os.Stdout directly,
// and there are far too many of them to route by hand.
func (l *LineReader) Capture() func() {
	if !l.raw {
		return l.captureLines()
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return func() {}
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return func() {}
	}

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	var wg sync.WaitGroup
	pump := func(r *os.File) {
		defer RestoreOnPanic()
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				_, _ = l.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(outR)
	go pump(errR)

	return func() {
		os.Stdout, os.Stderr = origOut, origErr
		_ = outW.Close()
		_ = errW.Close()
		wg.Wait()
		_ = outR.Close()
		_ = errR.Close()
	}
}

// LazyStdout resolves os.Stdout at write time.
//
// A writer that captures os.Stdout when it is constructed keeps writing to the
// original file after something replaces it.
type LazyStdout struct{}

func (LazyStdout) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

// IsTerminal reports whether output is going to a terminal, fixed at startup
// before any redirect, which is when it was true.
func (LazyStdout) IsTerminal() bool { return startedOnTerminal }

// startedOnTerminal records what stdout was before anything replaced it.
var startedOnTerminal = func() bool {
	info, err := os.Stdout.Stat()
	return err == nil && (info.Mode()&os.ModeCharDevice) != 0
}()

func newBufReader(in io.Reader) *bufio.Reader { return bufio.NewReaderSize(in, 64*1024) }

// dumbTerminal reports a terminal that says it cannot move the cursor:
// TERM=dumb, or no TERM at all outside Windows, whose consoles do not set
// one. It gets lines in and lines out, as a pipe does: no raw mode, no
// bracketed paste, no questions to the terminal, no cursor addressing.
func dumbTerminal() bool {
	t := os.Getenv("TERM")
	return t == "dumb" || t == "" && runtime.GOOS != "windows"
}

// captureLines is Capture for the line mode, on a terminal: what the program
// prints goes to the terminal through the same filter the dock applies, so
// file contents or model text printed by a command cannot move the cursor,
// set the clipboard or retitle the window. Piped output is left alone.
func (l *LineReader) captureLines() func() {
	if !startedOnTerminal {
		return func() {}
	}
	origOut, origErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		return func() {}
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return func() {}
	}
	os.Stdout, os.Stderr = outW, errW
	keepSGR := NewStyle(LazyStdout{}).enabled
	var wg sync.WaitGroup
	pump := func(r *os.File, to *os.File) {
		defer RestoreOnPanic()
		defer wg.Done()
		f := &streamFilter{keepSGR: keepSGR}
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				_, _ = to.WriteString(f.feed(buf[:n]))
			}
			if err != nil {
				_, _ = to.WriteString(f.flush())
				return
			}
		}
	}
	wg.Add(2)
	go pump(outR, origOut)
	go pump(errR, origErr)
	return func() {
		os.Stdout, os.Stderr = origOut, origErr
		_ = outW.Close()
		_ = errW.Close()
		wg.Wait()
		_ = outR.Close()
		_ = errR.Close()
	}
}

// streamFilter is sanitize over a stream: an escape or a character split
// across two reads is held until it is whole.
type streamFilter struct {
	keepSGR bool
	carry   []byte
	cg      colourGuard
}

func (f *streamFilter) clean(s string) string {
	out := sanitize(s, f.keepSGR)
	if f.keepSGR {
		out = f.cg.filter(out)
	}
	return out
}

func (f *streamFilter) feed(p []byte) string {
	data := append(append([]byte(nil), f.carry...), p...)
	f.carry = nil
	cut := len(data)
	// Hold an escape that has not ended, and a character not yet whole.
	if i := bytes.LastIndexByte(data, 0x1b); i >= 0 && escEnd(string(data), i) >= len(data) && len(data)-i < 4096 {
		cut = i
	}
	for k := cut - 1; k >= 0 && k >= cut-3; k-- {
		if !utf8.RuneStart(data[k]) {
			continue
		}
		if !utf8.FullRune(data[k:cut]) {
			cut = k
		}
		break
	}
	f.carry = append([]byte(nil), data[cut:]...)
	return f.clean(string(data[:cut]))
}

func (f *streamFilter) flush() string {
	out := f.clean(string(f.carry))
	f.carry = nil
	return out
}
