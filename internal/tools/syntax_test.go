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
	p := filepath.Join(dir, "run.sh") // no parser: only the diff guard can catch it
	writeFile(t, p, "alpha\nbeta\n")
	run(t, Read{}, s, map[string]any{"path": p})
	r := run(t, Edit{}, s, map[string]any{"path": p, "old_string": "beta\n", "new_string": "-beta\n+gamma\n"})
	if !r.IsError || !strings.Contains(r.Content, "pasted diff") {
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
	if v := s.parses(context.Background(), "x.rs", []byte("fn (")); v.checked {
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
	s, _ := setup(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	src := "open(" + `"` + marker + `"` + ", 'w').write('x')\n"
	v := s.parses(context.Background(), "a.py", []byte(src))
	if !v.checked || v.err != nil {
		t.Fatalf("valid python: %+v", v)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the checked source was executed")
	}
	v = s.parses(context.Background(), "a.py", []byte("def f():\n+    return 1\n"))
	if !v.checked || v.err == nil || !strings.Contains(v.err.Error(), "line") || !strings.HasPrefix(v.by, "python") {
		t.Fatalf("broken python: %+v", v)
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

// An agent can write a python3 into the workspace and put it first on PATH;
// the check must never run it.
func TestAnInterpreterInsideTheWorkspaceIsNeverRun(t *testing.T) {
	s, dir := setup(t)
	bin := filepath.Join(dir, ".venv", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	writeFile(t, filepath.Join(bin, "python3"), "#!/bin/sh\ntouch "+marker+"\nexit 1\n")
	if err := os.Chmod(filepath.Join(bin, "python3"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if v := s.parses(context.Background(), "a.py", []byte("x = (\n")); v.checked {
		t.Fatalf("a workspace interpreter was used: %+v", v)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the workspace's python3 ran")
	}
}

// Site packages, and so .pth files, never load while checking.
func TestSitePackagesDoNotLoad(t *testing.T) {
	s, _ := setup(t)
	py, _, ok := s.interpreter()
	if !ok {
		t.Skip("no python3 outside the workspace")
	}
	out, err := exec.Command(py, "-I", "-S", "-c", "import sys; print('site' in sys.modules)").Output()
	if err != nil || strings.TrimSpace(string(out)) != "False" {
		t.Fatalf("site loaded under -I -S: %q %v", out, err)
	}
}

func TestListsAndNumbersAreNotMistakenForDiffs(t *testing.T) {
	for _, c := range []struct{ path, old, new string }{
		{"README.md", "Some text", "- one\n- two"},
		{"deploy.yml", "tasks: []", "- import_tasks: a.yml"},
		{"data.csv", "0", "-1\n-2"},
		{"fix.patch", " context", "+added line"},
		{"run.sh", "echo hi", "-1\n-2"},
	} {
		if looksLikeDiff(c.path, c.old, c.new) {
			t.Errorf("%s: %q taken for a diff", c.path, c.new)
		}
	}
	if !looksLikeDiff("run.sh", "echo hi\n", "-echo hi\n+echo bye\n") {
		t.Error("a real hunk was not recognised")
	}
}

func TestWriteThatBreaksParsingIsRefusedAndNotCheckpointed(t *testing.T) {
	s, dir := setup(t)
	checkpoints := 0
	s.Checkpoint = func(string, []byte, bool) { checkpoints++ }
	p := filepath.Join(dir, "cfg.json")
	writeFile(t, p, `{"a": 1}`)
	run(t, Read{}, s, map[string]any{"path": p})
	r := run(t, Write{}, s, map[string]any{"path": p, "content": `{"a": 1,`})
	if !r.IsError || !strings.HasPrefix(r.Content, NotApplied) || readFile(t, p) != `{"a": 1}` || checkpoints != 0 {
		t.Fatalf("refused write: %+v, checkpoints %d", r, checkpoints)
	}
}

func TestEditInReportModeAppliesAndWarns(t *testing.T) {
	s, dir := setup(t)
	s.Syntax = SyntaxReport
	p := filepath.Join(dir, "main.go")
	writeFile(t, p, "package main\n")
	run(t, Read{}, s, map[string]any{"path": p})
	r := run(t, Edit{}, s, map[string]any{"path": p, "old_string": "package main\n", "new_string": "package main\nfunc (\n"})
	if r.IsError || !strings.Contains(r.Content, "no longer parses") {
		t.Fatalf("report mode: %+v", r)
	}
}

func TestPythonRefusalFollowsTheInterpreterVersion(t *testing.T) {
	s, dir := setup(t)
	if _, _, ok := s.interpreter(); !ok {
		t.Skip("no python3 outside the workspace")
	}
	p := filepath.Join(dir, "a.py")
	writeFile(t, p, "def f():\n    return 1\n")
	run(t, Read{}, s, map[string]any{"path": p})
	v := s.parses(context.Background(), p, []byte("def f(:\n"))
	r := run(t, Edit{}, s, map[string]any{"path": p, "old_string": "def f():", "new_string": "def f(:"})
	if v.strict != r.IsError {
		t.Fatalf("strict=%v but refused=%v: %+v", v.strict, r.IsError, r)
	}
	if !strings.Contains(r.Content, v.by) {
		t.Fatalf("the message does not name the interpreter %q: %s", v.by, r.Content)
	}
}

func TestAlreadyBrokenFileKeepsItsNote(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "half.go")
	writeFile(t, p, "package half\n\nfunc a( {\n")
	run(t, Read{}, s, map[string]any{"path": p})
	r := run(t, Edit{}, s, map[string]any{"path": p, "old_string": "package half", "new_string": "package whole"})
	if r.IsError || !strings.Contains(r.Content, "still does not parse") {
		t.Fatalf("an edit to a broken file must apply with a note: %+v", r)
	}
}
