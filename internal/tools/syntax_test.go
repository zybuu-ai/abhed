package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The case seen in the benchmark: a model pastes diff markers into code.
func TestEditThatBreaksParsingIsRefused(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "main.go")
	const orig = "package main\n\nfunc main() {\n\tx := 1\n\t_ = x\n}\n"
	writeFile(t, p, orig)
	run(t, Read{}, s, map[string]any{"path": p})

	r := run(t, Edit{}, s, map[string]any{"path": p, "old_string": "\tx := 1\n", "new_string": "\tx := 1\n+\tif x {\n"})
	if !r.IsError || !strings.HasPrefix(r.Content, NotApplied) || !strings.Contains(r.Content, "no longer parse") {
		t.Fatalf("a breaking edit was not refused: %+v", r)
	}
	if got := readFile(t, p); got != orig {
		t.Fatalf("the file changed although the edit was refused:\n%s", got)
	}
}

func TestPastedDiffIsRefused(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "notes.txt") // no parser: only the diff guard can catch it
	writeFile(t, p, "alpha\nbeta\n")
	run(t, Read{}, s, map[string]any{"path": p})
	r := run(t, Edit{}, s, map[string]any{"path": p, "old_string": "beta\n", "new_string": "-beta\n+gamma\n"})
	if !r.IsError || !strings.Contains(r.Content, "looks like a diff") {
		t.Fatalf("a pasted diff was applied: %+v", r)
	}
	// A real line that starts with a sign is not a diff.
	r = run(t, Edit{}, s, map[string]any{"path": p, "old_string": "beta\n", "new_string": "beta\n-- signature\nplain\n"})
	if r.IsError {
		t.Fatalf("an ordinary edit was refused: %+v", r)
	}
}

// Blocking a file that was already broken would stop a refactor half-way.
func TestAlreadyBrokenFileCanStillBeEdited(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "half.go")
	writeFile(t, p, "package half\n\nfunc a( {\n")
	run(t, Read{}, s, map[string]any{"path": p})
	r := run(t, Edit{}, s, map[string]any{"path": p, "old_string": "func a( {", "new_string": "func a() {"})
	if r.IsError {
		t.Fatalf("an edit to an already broken file was refused: %+v", r)
	}
}

func TestReportModeAppliesAndWarns(t *testing.T) {
	s, dir := setup(t)
	s.Syntax = SyntaxReport
	p := filepath.Join(dir, "cfg.json")
	writeFile(t, p, `{"a": 1}`)
	run(t, Read{}, s, map[string]any{"path": p})
	r := run(t, Write{}, s, map[string]any{"path": p, "content": `{"a": 1,`})
	if r.IsError || !strings.Contains(r.Content, "Warning:") || readFile(t, p) != `{"a": 1,` {
		t.Fatalf("report mode must apply and warn: %+v", r)
	}
}

func TestOffModeAndUnknownLanguagesSkipTheCheck(t *testing.T) {
	s, dir := setup(t)
	s.Syntax = SyntaxOff
	p := filepath.Join(dir, "cfg.json")
	writeFile(t, p, `{}`)
	run(t, Read{}, s, map[string]any{"path": p})
	if r := run(t, Write{}, s, map[string]any{"path": p, "content": "{"}); r.IsError {
		t.Fatalf("off mode refused: %+v", r)
	}
	if checked, _ := parses(context.Background(), "x.rs", []byte("fn (")); checked {
		t.Fatal("a language with no parser here was judged")
	}
}

func TestNewFileIsWrittenWithAWarning(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "new.go")
	r := run(t, Write{}, s, map[string]any{"path": p, "content": "package x\nfunc (\n"})
	if r.IsError || !strings.Contains(r.Content, "does not parse") {
		t.Fatalf("a new file must be written, with a warning: %+v", r)
	}
}

func TestPythonIsCompiledNotRun(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	src := "open(" + `"` + marker + `"` + ", 'w').write('x')\n"
	checked, err := parses(context.Background(), "a.py", []byte(src))
	if !checked || err != nil {
		t.Fatalf("valid python: checked=%v err=%v", checked, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the checked source was executed")
	}
	checked, err = parses(context.Background(), "a.py", []byte("def f():\n+    return 1\n"))
	if !checked || err == nil || !strings.Contains(err.Error(), "line") {
		t.Fatalf("broken python: checked=%v err=%v", checked, err)
	}
}

func TestParseSyntaxMode(t *testing.T) {
	for in, want := range map[string]SyntaxMode{"": SyntaxRefuse, "refuse": SyntaxRefuse, "report": SyntaxReport, "off": SyntaxOff} {
		if got, err := ParseSyntaxMode(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := ParseSyntaxMode("sometimes"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

func TestForkKeepsTheMode(t *testing.T) {
	s, _ := setup(t)
	s.Syntax = SyntaxOff
	if s.Fork().Syntax != SyntaxOff {
		t.Fatal("a forked session lost the syntax mode")
	}
}
