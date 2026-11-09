package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -p --continue with no task goes on with the run that stopped at its turn
// limit: recorded as turns.extended, then refused once nothing is left to
// continue.
func TestPrintContinueAfterTheTurnLimit(t *testing.T) {
	var frames []string
	for i := range 8 {
		frames = append(frames, callTool("c"+string(rune('a'+i)), "glob", map[string]any{"pattern": "*.none" + string(rune('a'+i))}))
	}
	m := newScriptModel(t, frames...)
	ws := bgWorkspace(t, m.URL, `,"limits":{"max_continues":1}`)
	run := func(args ...string) (string, string, int) {
		var out, errb bytes.Buffer
		cmd := mainHelper(append([]string{"-C", ws}, args...))
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if err != nil {
			code = cmd.ProcessState.ExitCode()
		}
		return out.String(), errb.String(), code
	}
	out, errOut, code := run("-p", "go", "-max-turns", "2", "-output-format", "json")
	if code == 0 || !strings.Contains(out, `"subtype":"max_turns"`) {
		t.Fatalf("first run: %d %s %s", code, out, errOut)
	}
	out, errOut, _ = run("-p", "--continue", "-max-turns", "2", "-output-format", "json")
	if !strings.Contains(out, `"type":"turns.extended"`) || !strings.Contains(out, `"surface":"print"`) {
		t.Fatalf("continue: no turns.extended:\n%s\n%s", out, errOut)
	}
	// The one continuation allowed is used; the next is refused, with why.
	_, errOut, code = run("-p", "--continue", "-max-turns", "2")
	if code != 2 || !strings.Contains(errOut, "max_continues") {
		t.Fatalf("past the bound: %d %s", code, errOut)
	}
	// Forking a conversation that cannot be continued writes no branch.
	before, _, _ := run("sessions")
	_, errOut, code = run("-p", "--continue", "--fork-session", "-max-turns", "2")
	if code != 2 || !strings.Contains(errOut, "max_continues") {
		t.Fatalf("a fork past the bound: %d %s", code, errOut)
	}
	if after, _, _ := run("sessions"); after != before {
		t.Fatalf("a refused fork wrote a branch:\n%s\n%s", before, after)
	}
	// Nor does going on from a copy of the record kept outside it.
	home, _ := os.UserHomeDir()
	files, _ := filepath.Glob(filepath.Join(home, ".abhed", "records", "*", "*.jsonl"))
	var copyPath string
	for _, f := range files {
		if filepath.Base(f) == "index.jsonl" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		copyPath = filepath.Join(t.TempDir(), "copy.jsonl")
		write(t, copyPath, string(data))
		break
	}
	if copyPath == "" {
		t.Fatalf("no session file among %v", files)
	}
	_, errOut, code = run("-p", "-r", copyPath, "-max-turns", "2")
	if code != 2 || !strings.Contains(errOut, "max_continues") {
		t.Fatalf("a copy past the bound: %d %s", code, errOut)
	}
	if after, _, _ := run("sessions"); after != before {
		t.Fatalf("a refused copy wrote a session:\n%s\n%s", before, after)
	}
}
