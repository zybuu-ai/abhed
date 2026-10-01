//go:build unix

package app

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// dumbRun is the CLI on a pty that says it cannot move the cursor: the line
// mode, where approvals are numbered prompts answered with a line.
type dumbRun struct {
	t   *testing.T
	tty *os.File
	mu  sync.Mutex
	out strings.Builder
}

func startDumb(t *testing.T, ws string) *dumbRun {
	t.Helper()
	cmd := mainHelper([]string{"-C", ws}, "TERM=dumb")
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 100, Rows: 30})
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = tty.Close() })
	d := &dumbRun{t: t, tty: tty}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := tty.Read(buf)
			d.mu.Lock()
			d.out.Write(buf[:n])
			d.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	d.wait("Type a task")
	return d
}

func (d *dumbRun) text() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.out.String()
}

func (d *dumbRun) wait(s string) {
	d.t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !strings.Contains(d.text(), s); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			d.t.Fatalf("never saw %q:\n%q", s, d.text())
		}
	}
}

func (d *dumbRun) send(s string) { _, _ = io.WriteString(d.tty, s) }

// In the line mode too, a model-chosen command cannot show one thing and run
// another, and letters or Enter alone do not approve it.
func TestDumbTerminalApprovalShowsTheRealCommand(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	_ = stub
	d := startDumb(t, ws)
	d.send("please spoof\r")
	d.wait("answer 1-2:")
	got := d.text()
	if !strings.Contains(got, "touch pwned #⟨U+200D⟩⟨\\r⟩│ $ ls -la") || strings.Contains(got, "#\u200d\r") {
		t.Fatalf("the prompt does not show the whole command:\n%q", got)
	}
	for _, k := range []string{"y\r", "a\r", "A\r", "\r"} {
		d.send(k)
		time.Sleep(150 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(ws, "pwned")); err == nil {
		t.Fatal("a letter or Enter approved the command")
	}
	d.send("2\r") // No
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(ws, "pwned")); err == nil {
		t.Fatal("the command ran")
	}
}

// In the line mode, a model-chosen path cannot write a clipboard, set the
// title or erase the screen, and nothing drawn carries an escape at all.
func TestDumbTerminalPathEscapes(t *testing.T) {
	_, ws := tuiWorkspace(t, "")
	d := startDumb(t, ws)
	d.send("please escape\r")
	d.wait("answer 1-")
	d.send("3\r")
	time.Sleep(500 * time.Millisecond)
	if got := d.text(); strings.Contains(got, "\x1b") || strings.Contains(got, "\a") {
		i := strings.IndexAny(got, "\x1b\a")
		t.Fatalf("an escape reached a dumb terminal: …%q…", got[max(0, i-20):min(len(got), i+30)])
	}
}

// In the line mode, what a command prints goes through the same filter: its
// escapes never reach the terminal, and its text does.
func TestDumbTerminalCommandOutputEscapes(t *testing.T) {
	_, ws := tuiWorkspace(t, "")
	d := startDumb(t, ws)
	d.send("please print\r")
	d.wait("answer 1-")
	d.send("1\r")
	d.wait("Done")
	time.Sleep(300 * time.Millisecond)
	got := d.text()
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\a") {
		i := strings.IndexAny(got, "\x1b\a")
		t.Fatalf("an escape reached a dumb terminal: …%q…", got[max(0, i-20):min(len(got), i+30)])
	}
	if !strings.Contains(got, "TITLE") {
		t.Fatalf("the command's output is missing:\n%q", got)
	}
}

// In the line mode, text the program prints itself (here /model listing a
// provider named with escapes in the config) goes through the output filter
// too: nothing drawn carries an escape.
func TestDumbTerminalPrintedTextIsFiltered(t *testing.T) {
	_, ws := tuiWorkspace(t, "")
	cfgPath := filepath.Join(os.Getenv("HOME"), ".abhed", "config.json")
	cfg, err := os.ReadFile(cfgPath) // #nosec G304 -- the test's own config
	if err != nil {
		t.Fatal(err)
	}
	hostile := strings.Replace(string(cfg), `"stub2":`, `"stub2\u001b]0;TITLE\u0007\u001b[2Jx":`, 1)
	if err := os.WriteFile(cfgPath, []byte(hostile), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDumb(t, ws)
	d.send("/model\r")
	d.wait("current: stub-1")
	time.Sleep(300 * time.Millisecond)
	got := d.text()
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\a") {
		i := strings.IndexAny(got, "\x1b\a")
		t.Fatalf("an escape reached a dumb terminal: …%q…", got[max(0, i-20):min(len(got), i+30)])
	}
	if !strings.Contains(got, "stub2x") {
		t.Fatalf("the provider list is missing:\n%q", got)
	}
}
