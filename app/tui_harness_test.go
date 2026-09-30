//go:build unix

package app

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/zybuu-ai/abhed/internal/ui/vt"
)

// tuiRun is the CLI on a pty of a fixed size, with everything it writes
// applied to an emulated terminal: tests assert on the screen a person would
// see, and on the bytes it took to draw.
type tuiRun struct {
	t    *testing.T
	tty  *os.File
	cmd  *exec.Cmd
	term *vt.Terminal
	stub *tuiStub
	ws   string
	done chan struct{}

	mu     sync.Mutex
	raw    []byte
	change time.Time // when the screen last changed
	mark   int
	// chunks records when each read arrived and where it ends in raw.
	chunks []chunk
}

type chunk struct {
	at  time.Time
	end int
}

// seenAfterMark is when the output first contained s after the mark, or
// the zero time.
func (r *tuiRun) seenAfterMark(s string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := strings.Index(string(r.raw[r.mark:]), s)
	if i < 0 {
		return time.Time{}
	}
	i += r.mark
	for _, c := range r.chunks {
		if c.end >= i+len(s) {
			return c.at
		}
	}
	return time.Time{}
}

// tuiWorkspace writes a home config naming a stub model and a workspace with
// a file to work on, and returns both.
func tuiWorkspace(t *testing.T, extra string) (*tuiStub, string) {
	t.Helper()
	// The canonical path: the tools refuse a new file named through a link
	// to the workspace, and macOS's temporary folder is one.
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hello world\nthe end\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := &tuiStub{ws: ws}
	url := stub.start(t)
	home := t.TempDir()
	cfg := `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` +
		url + `","model":"stub-1","context_window":8192},"stub2":{"type":"openai-compatible","base_url":"` +
		url + `","model":"stub-2","context_window":8192}}}` + extra + `}`
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	t.Setenv("COLORFGBG", "")
	return stub, ws
}

func startTUI(t *testing.T, stub *tuiStub, ws string, cols, rows int, args ...string) *tuiRun {
	t.Helper()
	return startTUIWith(t, stub, ws, cols, rows, "", args...)
}

// startTUIWith starts the CLI on a terminal that answers a background-colour
// query with background ("" answers nothing, as many terminals do).
func startTUIWith(t *testing.T, stub *tuiStub, ws string, cols, rows int, background string, args ...string) *tuiRun {
	t.Helper()
	return startTUIDelayed(t, stub, ws, cols, rows, background, 0, args...)
}

// startTUIDelayed answers the background query after delay, as a terminal
// over a slow link does.
func startTUIDelayed(t *testing.T, stub *tuiStub, ws string, cols, rows int, background string, delay time.Duration, args ...string) *tuiRun {
	t.Helper()
	cmd := mainHelper(append([]string{"-C", ws}, args...), "TERM="+os.Getenv("TERM"))
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	r := &tuiRun{t: t, tty: tty, cmd: cmd, term: vt.New(cols, rows), stub: stub, ws: ws, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		buf := make([]byte, 64*1024)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				now := time.Now()
				r.mu.Lock()
				r.raw = append(r.raw, buf[:n]...)
				r.change = now
				r.chunks = append(r.chunks, chunk{now, len(r.raw)})
				r.mu.Unlock()
				_, _ = r.term.Write(buf[:n])
				if background != "" && strings.Contains(string(buf[:n]), "\x1b]11;?") {
					go func() {
						time.Sleep(delay)
						_, _ = io.WriteString(tty, "\x1b]11;rgb:"+background+"\x07\x1b[?62;22c")
					}()
				}
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // the helper this test started
		_ = cmd.Wait()
		_ = tty.Close()
	})
	r.waitText("shift+tab")
	return r
}

func (r *tuiRun) send(s string) {
	r.t.Helper()
	if _, err := io.WriteString(r.tty, s); err != nil {
		r.t.Fatal(err)
	}
}

// typed sends s a key at a time, as a person types.
func (r *tuiRun) typed(s string) {
	for _, c := range s {
		r.send(string(c))
		time.Sleep(3 * time.Millisecond)
	}
}

// waitFor waits until the screen (or, with all, the scrollback too)
// satisfies pred.
func (r *tuiRun) waitFor(what string, all bool, pred func(string) bool) {
	r.t.Helper()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		text := r.term.Text()
		if all {
			text = r.term.All()
		}
		if pred(text) {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("never saw %s:\n%s\n--- scrollback and screen ---\n%s", what, r.term.Dump(), r.term.All())
		}
	}
}

func (r *tuiRun) waitText(s string) {
	r.t.Helper()
	r.waitFor(`"`+s+`"`, true, func(text string) bool { return strings.Contains(text, s) })
}

func (r *tuiRun) waitScreen(s string) {
	r.t.Helper()
	r.waitFor(`"`+s+`" on screen`, false, func(text string) bool { return strings.Contains(text, s) })
}

// quiet waits until nothing has been drawn for d.
func (r *tuiRun) quiet(d time.Duration) {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		r.mu.Lock()
		last := r.change
		r.mu.Unlock()
		if time.Since(last) >= d {
			return
		}
	}
}

// drawn waits for output after a mark, then for it to stop.
func (r *tuiRun) drawn(settle time.Duration) {
	for deadline := time.Now().Add(2 * time.Second); r.bytesSinceMark() == 0 && time.Now().Before(deadline); {
		time.Sleep(2 * time.Millisecond)
	}
	r.quiet(settle)
}

func (r *tuiRun) markBytes() {
	r.mu.Lock()
	r.mark = len(r.raw)
	r.mu.Unlock()
}

func (r *tuiRun) bytesSinceMark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.raw) - r.mark
}

func (r *tuiRun) rawSinceMark() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.raw[r.mark:])
}

func (r *tuiRun) resize(cols, rows int) {
	r.t.Helper()
	r.term.Resize(cols, rows)
	if err := pty.Setsize(r.tty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		r.t.Fatal(err)
	}
}

// capture is the screen as text, for the before and after captures a
// failure or a log shows.
func (r *tuiRun) capture(label string) string {
	return "=== " + label + " ===\n" + r.term.Dump()
}

func (r *tuiRun) exit() {
	r.t.Helper()
	r.send("\x03")
	time.Sleep(50 * time.Millisecond)
	r.send("\x03")
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		r.t.Fatalf("did not exit:\n%s", r.term.Dump())
	}
}
