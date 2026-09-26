package ui

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

// LineReader reads a line of input with the editing a terminal user expects.
//
// The CLI read with bufio.Reader, which delivers whatever the terminal sends
// and nothing more: arrow keys arrived as escape sequences and appeared as
// "^[[D", there was no history, and a typo could only be fixed by backspacing
// to it. That is not a preference — a prompt where Left does not move the
// cursor reads as broken.
//
// When stdin is not a terminal — a pipe, a CI job, a here-doc — this falls
// straight through to line reads, because raw mode on a pipe would corrupt the
// input and there is nobody typing to benefit from it.
type LineReader struct {
	ed      *editor
	fd      int
	state   *term.State
	fallbck *bufio.Reader
	raw     bool
}

// NewLineReader prepares stdin for editing where that is possible.
func NewLineReader(prompt string) *LineReader {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return &LineReader{fallbck: bufio.NewReader(os.Stdin)}
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return &LineReader{fallbck: bufio.NewReader(os.Stdin)}
	}
	l := &LineReader{ed: newEditor(os.Stdin, os.Stdout, prompt), fd: fd, state: state, raw: true}

	return l
}

// ReadLine returns the next line. In raw mode it supports Left and Right to
// move, Up and Down for history, Home, End, Ctrl-A, Ctrl-E, Ctrl-U, Ctrl-K and
// Ctrl-W, and Ctrl-C and Ctrl-D as interrupt and end of input.
func (l *LineReader) ReadLine() (string, error) {
	if l.raw {
		return l.ed.readLine()
	}
	line, err := l.fallbck.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

// Raw reports whether editing is active, so a caller can print its own prompt
// when it is not.
func (l *LineReader) Raw() bool { return l.raw }

// ApprovalKeys routes decision keys to an approval until end, in raw mode. read
// arms the key guard each call: the approver calls it just after drawing.
func (l *LineReader) ApprovalKeys(ctx context.Context) (read func() (string, bool), end func()) {
	keys := l.ed.beginApproval()
	read = func() (string, bool) {
		l.ed.armApproval()
		select {
		case k := <-keys:
			return string(k), true
		case <-ctx.Done():
			return "", false
		}
	}
	return read, l.ed.endApproval
}

// Quiet suspends the prompt while a turn is running, so the reader can stay
// live for steering without painting over the turn's output.
func (l *LineReader) Quiet(q bool) {
	if l.raw {
		l.ed.setQuiet(q)
	}
}

// SetPrompt changes the prompt shown before the cursor.
func (l *LineReader) SetPrompt(p string) {
	if l.raw {
		l.ed.setPrompt(p)
	}
}

// Write prints through the terminal so output does not collide with a line
// being edited. Outside raw mode it goes to stdout unchanged.
func (l *LineReader) Write(p []byte) (int, error) {
	if l.raw {
		return l.ed.write(p)
	}
	return os.Stdout.Write(p)
}

// Close restores the terminal. Leaving it in raw mode makes the user's shell
// unusable afterwards, which is a worse failure than anything this package
// does, so callers must defer it.
func (l *LineReader) Close() {
	if l.raw && l.state != nil {
		_ = term.Restore(l.fd, l.state)
		l.raw = false
	}
}

// rawWriter translates newlines for a terminal in raw mode.
//
// Raw mode turns off the driver's own ONLCR translation, so a bare \n moves the
// cursor down without returning it to column 0 and every subsequent line starts
// further right — the staircase. Ordinary Go code writes \n and should not have
// to know the terminal is in raw mode, so the translation happens here, once,
// on the way out.
type rawWriter struct{ w io.Writer }

func (r rawWriter) Write(p []byte) (int, error) {
	// Only \n that is not already preceded by \r needs a carriage return
	// added; rewriting an existing CRLF would double it.
	var out []byte
	var last byte
	for _, c := range p {
		if c == '\n' && last != '\r' {
			out = append(out, '\r')
		}
		out = append(out, c)
		last = c
	}
	if _, err := r.w.Write(out); err != nil {
		return 0, err
	}
	// Report the caller's own length: it wrote p, and the padding is ours.
	return len(p), nil
}

// Capture redirects the process's standard output and error through the raw
// translation, and returns a function that restores them.
//
// Redirecting the streams rather than handing callers a writer is deliberate:
// fmt.Println and every log line in the program write to os.Stdout directly,
// and there are far too many of them to route by hand. One of them missed is a
// staircase across the screen — which is exactly what shipped.
//
// A pipe is used rather than assigning to os.Stdout, because os.Stdout is an
// *os.File and cannot be replaced with an arbitrary writer.
func (l *LineReader) Capture() func() {
	if !l.raw {
		return func() {}
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return func() {}
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return func() {}
	}

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	// Write through the terminal rather than past it. x/term tracks where the
	// cursor is so it can clear the prompt line before other output and redraw
	// it afterwards; bytes that go straight to the file bypass that bookkeeping,
	// and the prompt stops reappearing after the first message printed
	// mid-session. It also handles CRLF, so no separate translation is needed
	// on this path.
	var wg sync.WaitGroup
	pump := func(r *os.File, fallback io.Writer) {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if l.ed != nil {
					_, _ = l.ed.write(buf[:n])
				} else {
					_, _ = rawWriter{fallback}.Write(buf[:n])
				}
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(outR, origOut)
	go pump(errR, origErr)

	return func() {
		os.Stdout, os.Stderr = origOut, origErr
		outW.Close()
		errW.Close()
		wg.Wait()
		outR.Close()
		errR.Close()
	}
}

// LazyStdout resolves os.Stdout at write time.
//
// A writer that captures os.Stdout when it is constructed keeps writing to the
// original file after something replaces it — which is what left two lines of a
// multi-line error staircasing while the rest was translated correctly. Looking
// it up per write costs nothing measurable and removes the ordering hazard
// entirely.
type LazyStdout struct{}

func (LazyStdout) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

// IsTerminal reports whether output is going to a terminal, so styling is
// decided by where the bytes end up rather than by the type of the wrapper.
//
// The redirect installed for raw mode replaces os.Stdout with a pipe, and a
// pipe is not a character device — so asking os.Stdout directly would say "not
// a terminal" for exactly the case that needs colour most. The answer is fixed
// at startup, before any redirect, which is when it was true.
func (LazyStdout) IsTerminal() bool { return startedOnTerminal }

// startedOnTerminal records what stdout was before anything replaced it.
var startedOnTerminal = func() bool {
	info, err := os.Stdout.Stat()
	return err == nil && (info.Mode()&os.ModeCharDevice) != 0
}()
