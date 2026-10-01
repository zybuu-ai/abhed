//go:build unix

package app

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// A terminal that says it cannot move the cursor gets lines in and lines
// out: no raw mode, no bracketed paste, no questions to the terminal, no
// cursor addressing or colour — and the session still works.
func TestTUIDumbTerminal(t *testing.T) {
	for _, term := range []string{"dumb", ""} {
		t.Run("TERM="+term, func(t *testing.T) {
			_, ws := tuiWorkspace(t, "")
			t.Setenv("TERM", term)
			cmd := mainHelper([]string{"-C", ws}, "TERM="+term)
			tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
			if err != nil {
				t.Skipf("no pty: %v", err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = tty.Close() })
			var mu sync.Mutex
			var out strings.Builder
			go func() {
				buf := make([]byte, 4096)
				for {
					n, err := tty.Read(buf)
					mu.Lock()
					out.Write(buf[:n])
					mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
			wait := func(s string) string {
				for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
					mu.Lock()
					got := out.String()
					mu.Unlock()
					if strings.Contains(got, s) {
						return got
					}
				}
				mu.Lock()
				defer mu.Unlock()
				t.Fatalf("never saw %q:\n%q", s, out.String())
				return ""
			}
			wait("Type a task")
			_, _ = io.WriteString(tty, "hello\r")
			got := wait("You said: hello")
			if strings.Contains(got, "\x1b") {
				i := strings.Index(got, "\x1b")
				t.Fatalf("an escape was written to a dumb terminal: …%q…", got[i:min(len(got), i+20)])
			}
		})
	}
}
