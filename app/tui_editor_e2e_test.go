//go:build unix

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Ctrl-G's editor runs on the terminal itself: stdin, stdout and stderr are
// all a tty, never the pipes that carry the program's own output into the
// dock. What it saves comes back as the draft.
func TestTUIEditorGetsTheTerminal(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	dir := t.TempDir()
	report := filepath.Join(dir, "fds")
	script := filepath.Join(dir, "editor.sh")
	body := "#!/bin/sh\n" +
		"r=''\n" +
		"for fd in 0 1 2; do if [ -t $fd ]; then r=\"$r TTY$fd\"; else r=\"$r NOTTY$fd\"; fi; done\n" +
		"echo \"$r\" > '" + report + "'\n" +
		"printf 'edited in the editor' > \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { // #nosec G306 -- a test editor that must run
		t.Fatal(err)
	}
	t.Setenv("VISUAL", script)
	r := startTUI(t, stub, ws, 80, 24)
	r.quiet(100 * time.Millisecond)
	r.send("\x07")
	r.waitScreen("edited in the editor")
	got, err := os.ReadFile(report) // #nosec G304 -- the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if s := strings.TrimSpace(string(got)); s != "TTY0 TTY1 TTY2" {
		t.Fatalf("the editor's fds: %q, want a terminal on 0, 1 and 2", s)
	}
}
