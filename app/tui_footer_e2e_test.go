//go:build unix

package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// footerRow is the screen's last non-empty row: the status line.
func footerRows(r *tuiRun) string {
	lines := r.term.Lines()
	var out []string
	for i := len(lines) - 1; i >= 0 && len(out) < 3; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			out = append([]string{lines[i]}, out...)
		}
	}
	return strings.Join(out, "\n")
}

// The footer shows the model, the mode, how full the context is, the
// session's tokens and where the session is; /model changes it at once.
func TestTUIFooterShowsTheSession(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git", "HEAD"), []byte("ref: refs/heads/feature-x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := startTUI(t, stub, ws, 120, 30)
	r.waitFor("the footer", false, func(s string) bool {
		return strings.Contains(s, "stub-1") && strings.Contains(s, "(feature-x)") && strings.Contains(s, "● default mode")
	})
	r.send("hello\r")
	r.waitText("You said: hello")
	r.waitFor("context and tokens in the footer", false, func(s string) bool {
		return strings.Contains(s, "% context") && strings.Contains(s, "1.3k tokens")
	})
	t.Log(r.capture("the footer after one reply"))
	r.send("/model stub2\r")
	r.waitFor("the new model in the footer", false, func(s string) bool { return strings.Contains(footerRows(r), "stub-2") })
}

// A status line command's output replaces the built-in status row.
func TestTUIStatusLineCommand(t *testing.T) {
	testSandbox(t, t.TempDir())
	stub, ws := tuiWorkspace(t, `,"statusline":{"command":"cat >/dev/null; printf 'custom \\033[32mstatus\\033[0m line'"}`)
	r := startTUI(t, stub, ws, 100, 30)
	r.waitFor("the custom status line", false, func(s string) bool { return strings.Contains(s, "custom status line") })
}

// Shift-Tab steps through default, accept-edits and plan and never
// reaches auto or bypass, however often it is pressed; the mode is always
// on screen.
func TestTUIShiftTabNeverReachesAuto(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 100, 30)
	r.waitScreen("stub-1 · ") // the session has filled the footer and taken the key
	var seen []string
	for i := 0; i < 10; i++ {
		r.markBytes()
		r.send("\x1b[Z")
		r.drawn(80 * time.Millisecond)
		f := footerRows(r)
		for _, m := range []string{"default mode", "accept edits", "plan mode", "auto mode", "bypass"} {
			if strings.Contains(f, m) {
				seen = append(seen, m)
			}
		}
		if strings.Contains(f, "auto") || strings.Contains(f, "bypass") {
			t.Fatalf("Shift-Tab reached %q:\n%s", f, r.term.Dump())
		}
	}
	want := "accept edits,plan mode,default mode,accept edits,plan mode,default mode,accept edits,plan mode,default mode,accept edits"
	if got := strings.Join(seen, ","); got != want {
		t.Fatalf("modes seen %s", got)
	}
}

// Shift-Tab during a turn waits for the turn to end, and says so.
func TestTUIShiftTabDuringATurn(t *testing.T) {
	stub, ws := tuiWorkspace(t, "")
	r := startTUI(t, stub, ws, 100, 30)
	r.send("please slow\r")
	r.waitScreen("word2")
	r.send("\x1b[Z")
	r.waitScreen("→ accept-edits after this turn")
	r.waitText("word59")
	r.waitFor("the mode to apply", false, func(s string) bool { return strings.Contains(s, "⏵⏵ accept edits") })
}

// A status line command's output keeps its text and colour, and nothing
// that could write the clipboard or retitle the window reaches the terminal.
func TestTUIStatusLineIsSanitized(t *testing.T) {
	testSandbox(t, t.TempDir())
	stub, ws := tuiWorkspace(t, `,"statusline":{"command":"cat >/dev/null; printf '\\033]52;c;U1BPT0Y=\\007\\033]0;title\\007\\033[2J\\033[32mok\\033[0m line'"}`)
	r := startTUI(t, stub, ws, 100, 30)
	r.waitFor("the status line", false, func(s string) bool { return strings.Contains(s, "ok line") })
	r.mu.Lock()
	// The CLI's own background-colour query is the one OSC it sends.
	wire := strings.ReplaceAll(string(r.raw), "\x1b]11;?\a", "")
	r.mu.Unlock()
	for _, f := range []string{"\x1b]52", "\x1b]0;title", "\x1b[2J", "\a"} {
		if strings.Contains(wire, f) {
			t.Fatalf("%q from the status line reached the terminal", f)
		}
	}
	if !strings.Contains(wire, "32mok") {
		t.Fatalf("the status line's colour was lost")
	}
}

// /review runs its turn in plan mode and puts the earlier mode back when the
// turn ends; the footer shows the mode put back, as the record and /status do.
func TestTUIFooterAfterReviewShowsTheModePutBack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	stub, ws := tuiWorkspace(t, "")
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-q", "-m", "one"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hello world\nREVIEW-MARK\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// /review runs git where commands run, and the container image has none,
	// so the session gets a PATH, inside the sandbox's view, with no engine.
	t.Setenv("PATH", pathWithout(t, ws, "docker", "podman", "nerdctl"))
	r := startTUI(t, stub, ws, 120, 30)
	r.waitFor("the footer", false, func(s string) bool { return strings.Contains(s, "● default mode") })
	r.send("!git --version\r")
	r.waitFor("whether the session has git", true, func(s string) bool {
		return strings.Contains(s, "git version") || strings.Contains(s, "Exit 127")
	})
	if !strings.Contains(r.term.All(), "git version") {
		t.Fatalf("git does not run where the session runs commands, so /review cannot:\n%s", r.term.All())
	}
	r.send("/review\r")
	r.waitFor("the mode put back", true, func(s string) bool { return strings.Contains(s, "mode: plan → default") })
	r.drawn(200 * time.Millisecond)
	r.waitFor("default mode in the footer", false, func(string) bool {
		f := footerRows(r)
		return strings.Contains(f, "● default mode") && !strings.Contains(f, "plan mode")
	})
}

// pathWithout is a PATH of links, in the git workspace ws, to every program
// on PATH but the named ones.
func pathWithout(t *testing.T, ws string, names ...string) string {
	t.Helper()
	skip := map[string]bool{}
	for _, n := range names {
		skip[n] = true
	}
	dir := filepath.Join(ws, ".bin")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git", "info", "exclude"), []byte(".bin/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if skip[e.Name()] {
				continue
			}
			// The first program of a name on PATH wins, as a lookup would.
			_ = os.Symlink(filepath.Join(d, e.Name()), filepath.Join(dir, e.Name()))
		}
	}
	return dir
}
