package app

import (
	"io"
	"os"
	"strings"
	"testing"
)

// An unknown output format is refused with the valid ones named. It used to
// print text, so a script asking for another format parsed prose.
func TestUnknownOutputFormatIsRefused(t *testing.T) {
	out, code := stderrOf(t, []string{"-p", "hi", "-output-format", "stream-json"})
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(out, `"stream-json"`) || !strings.Contains(out, "text or json") {
		t.Errorf("error does not name the format and the valid ones:\n%s", out)
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
