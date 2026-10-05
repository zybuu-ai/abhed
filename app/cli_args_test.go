package app

import (
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

// An unknown output format is refused with the valid ones named. It used to
// print text, so a script asking for another format parsed prose.
func TestUnknownOutputFormatIsRefused(t *testing.T) {
	out, code := stderrOf(t, []string{"-p", "hi", "-output-format", "xml"})
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(out, `"xml"`) || !strings.Contains(out, "text, json, stream-json") {
		t.Errorf("error does not name the format and the valid ones:\n%s", out)
	}
}

// An unknown -mode is a bad invocation like any other: exit 2, with the value
// and the valid ones named.
func TestUnknownModeIsRefused(t *testing.T) {
	out, code := stderrOf(t, []string{"-C", t.TempDir(), "-p", "hi", "-mode", "yolo"})
	if code != 2 || !strings.Contains(out, `"yolo"`) || !strings.Contains(out, "accept-edits") {
		t.Errorf("exit %d, stderr:\n%s", code, out)
	}
}

// A word that is not a command is an error, not a session opened as if it
// had run; "version" is a command.
func TestUnknownCommandIsRefused(t *testing.T) {
	out, code := stderrOf(t, []string{"-C", t.TempDir(), "frobnicate"})
	if code != 2 || !strings.Contains(out, `unknown command "frobnicate"`) {
		t.Errorf("exit %d, stderr:\n%s", code, out)
	}
}

func TestVersionCommandPrintsTheVersion(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := Main([]string{"version"})
	os.Stdout = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if code != 0 || !strings.HasPrefix(string(out), "abhed ") {
		t.Errorf("exit %d, stdout %q", code, out)
	}
}

// -h after a command that reads no flags of its own prints its usage and does
// nothing else: `index -h` used to build the index and `init -h` to write a
// config.
func TestSubcommandHelpDoesNotRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"init", "index", "trust", "user", "acp", "rpc", "doctor", "providers"} {
		ws := t.TempDir()
		out, code := stdoutOf(t, func() int { return Main([]string{"-C", ws, name, "-h"}) })
		if code != 0 || !strings.HasPrefix(out, "usage: abhed "+name) {
			t.Errorf("%s -h: exit %d, stdout:\n%s", name, code, out)
		}
		if _, err := os.Stat(ws + "/.abhed"); err == nil {
			t.Errorf("%s -h wrote .abhed", name)
		}
	}
}

// An edition's subcommand gets its own flags; the global flag set used to
// refuse them as "flag provided but not defined".
func TestEditionCommandTakesItsOwnFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	var got []string
	run := WithCommand("identities", func(_ string, args []string) int { got = args; return 3 })
	if code := Main([]string{"-C", ws, "identities", "forget", "-email", "x"}, run); code != 3 {
		t.Fatalf("exit %d, args %q", code, got)
	}
	if want := []string{"forget", "-email", "x"}; !slices.Equal(got, want) {
		t.Errorf("args %q, want %q", got, want)
	}
}
