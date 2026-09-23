package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// Every property of the isolation is on the command itself.
func TestPythonRunsIsolated(t *testing.T) {
	cmd := pythonCommand(context.Background(), "/usr/bin/python3", "pass", "a.py")
	args := strings.Join(cmd.Args[1:3], " ")
	if args != "-I -S" {
		t.Fatalf("interpreter flags %q, want -I -S", args)
	}
	if cmd.Env == nil || len(cmd.Env) != 0 {
		t.Fatalf("environment %v, want empty", cmd.Env)
	}
	if cmd.Dir != os.TempDir() || cmd.WaitDelay == 0 {
		t.Fatalf("dir %q, wait delay %v", cmd.Dir, cmd.WaitDelay)
	}
}

// A python3 reached through a symlink, such as a virtualenv's, is run as the
// real file, so the virtualenv and its site packages are skipped.
func TestTheRealInterpreterIsRun(t *testing.T) {
	found, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	real, _ := filepath.EvalSymlinks(found)
	s, _ := setup(t)
	link := t.TempDir()
	if err := os.Symlink(real, filepath.Join(link, "python3")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", link+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, _, ok := s.interpreter()
	if !ok || got != real {
		t.Fatalf("ran %q, want the real file %q", got, real)
	}
}

// fakePython is an interpreter that would be accepted if it were ever run: it
// answers the version question, and leaves a marker so a test can tell.
func fakePython(t *testing.T, bin string) (marker string) {
	t.Helper()
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(t.TempDir(), "ran")
	writeFile(t, filepath.Join(bin, "python3"), "#!/bin/sh\n/usr/bin/touch "+marker+"\necho 3 13\n")
	if err := os.Chmod(filepath.Join(bin, "python3"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func refusesInterpreter(t *testing.T, s *Session, marker string) {
	t.Helper()
	if _, _, ok := s.interpreter(); ok {
		t.Fatal("an interpreter inside the session's roots was accepted")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an interpreter inside the session's roots was run")
	}
}

// A directory whose name starts with two dots is still inside the workspace.
func TestADotDotDirectoryIsInsideTheWorkspace(t *testing.T) {
	s, dir := setup(t)
	refusesInterpreter(t, s, fakePython(t, filepath.Join(dir, "..venv", "bin")))
}

// An extra root is writable by the agent just like the workspace.
func TestAnInterpreterInAnExtraRootIsNeverRun(t *testing.T) {
	s, _ := setup(t)
	extra := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(extra); err == nil {
		extra = resolved
	}
	if err := s.AddRoot(extra); err != nil {
		t.Fatal(err)
	}
	refusesInterpreter(t, s, fakePython(t, filepath.Join(extra, "bin")))
}

// The fake is accepted outside the roots, so the two tests above are not vacuous.
func TestTheFakeInterpreterIsAcceptedOutsideTheRoots(t *testing.T) {
	s, _ := setup(t)
	fakePython(t, filepath.Join(t.TempDir(), "bin"))
	if _, ver, ok := s.interpreter(); !ok || ver != [2]int{3, 13} {
		t.Fatalf("the fake interpreter was not accepted outside the roots: %v %v", ver, ok)
	}
}

// A failed version check is asked again next time, and a hanging one is cut off.
func TestAFailedVersionCheckIsNotRemembered(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	count := filepath.Join(t.TempDir(), "count")
	py := filepath.Join(bin, "python3")
	writeFile(t, py, "#!/bin/sh\nif [ -f "+count+" ]; then echo 3 12; else /usr/bin/touch "+count+"; exit 1; fi\n")
	if err := os.Chmod(py, 0o755); err != nil {
		t.Fatal(err)
	}
	defer pyVersions.Delete(py)
	if _, ok := pythonVersion(py); ok {
		t.Fatal("the first, failing check was trusted")
	}
	if v, ok := pythonVersion(py); !ok || v != [2]int{3, 12} {
		t.Fatalf("the failure was remembered: %v %v", v, ok)
	}
}

func TestAHangingVersionCheckIsCutOff(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the five-second bound")
	}
	py := filepath.Join(t.TempDir(), "python3")
	writeFile(t, py, "#!/bin/sh\n/bin/sleep 30\n")
	if err := os.Chmod(py, 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, ok := pythonVersion(py); ok {
		t.Fatal("a hanging check was trusted")
	}
	if took := time.Since(start); took > pythonTimeout+3*time.Second {
		t.Fatalf("the check took %v", took)
	}
}

// Below 3.12 a Python verdict warns; from 3.12 it refuses. Seeding the version
// runs both branches whatever python3 this machine has.
func TestPythonRefusalFollowsTheInterpreterVersion(t *testing.T) {
	s, dir := setup(t)
	py, _, ok := s.interpreter()
	if !ok {
		t.Skip("no python3 outside the workspace")
	}
	defer pyVersions.Delete(py)
	p := filepath.Join(dir, "a.py")
	for _, c := range []struct {
		ver    [2]int
		refuse bool
	}{{[2]int{3, 9}, false}, {[2]int{3, 12}, true}} {
		pyVersions.Store(py, c.ver)
		writeFile(t, p, "def f():\n    return 1\n")
		run(t, Read{}, s, map[string]any{"path": p})
		r := run(t, Edit{}, s, map[string]any{"path": p, "old_string": "def f():", "new_string": "def f(:"})
		if r.IsError != c.refuse || !strings.Contains(r.Content, fmt.Sprintf("python%d.%d", c.ver[0], c.ver[1])) {
			t.Fatalf("python %v: refused=%v, want %v: %s", c.ver, r.IsError, c.refuse, r.Content)
		}
	}
}

// Without a conclusive answer about the file before the change, a broken
// result is warned about, never refused.
func TestAnUnknownBeforeNeverRefuses(t *testing.T) {
	s, dir := setup(t)
	real := parse
	defer func() { parse = real }()
	parse = func(s *Session, ctx context.Context, path string, content []byte) verdict {
		if strings.Contains(string(content), "broken") {
			return verdict{checked: true, err: fmt.Errorf("bad"), strict: true, by: "test"}
		}
		return verdict{} // no answer for the old content
	}
	p := filepath.Join(dir, "a.go")
	writeFile(t, p, "fine")
	run(t, Read{}, s, map[string]any{"path": p})
	r := run(t, Edit{}, s, map[string]any{"path": p, "old_string": "fine", "new_string": "broken"})
	if r.IsError || !strings.Contains(r.Content, "Warning:") {
		t.Fatalf("an unknown baseline must warn, not refuse: %+v", r)
	}
}

func TestDiffsInFilesWhereMarkersAreContent(t *testing.T) {
	for ext := range notCode {
		if path := "file" + ext; looksLikeDiff(path, "old", "-old\n+new") {
			t.Errorf("%s: a hunk in a file where it is content was refused", path)
		}
	}
	for _, c := range []struct{ old, new string }{{"offset", "-offset"}, {"1", "-1"}, {"1,", "-1,\n-2,"}, {"x", "+x"}} {
		if looksLikeDiff("a.py", c.old, c.new) {
			t.Errorf("%q -> %q taken for a diff", c.old, c.new)
		}
	}
}

// Old text that already has marked lines is itself diff-like content, so the
// guard stands aside.
func TestOldTextWithMarkersTurnsTheGuardOff(t *testing.T) {
	if looksLikeDiff("run.sh", "-a\n+b\n", "-a\n+c\n") {
		t.Fatal("editing marked lines was taken for a pasted diff")
	}
}

// Creating a file with a pasted diff in report mode keeps the warning.
func TestReportModeWarnsOfADiffWhenCreating(t *testing.T) {
	s, dir := setup(t)
	s.Syntax = SyntaxReport
	p := filepath.Join(dir, "new.sh")
	r := run(t, Edit{}, s, map[string]any{"path": p, "old_string": "", "new_string": "-a\n+b\n"})
	if r.IsError || !strings.Contains(r.Content, "pasted diff") {
		t.Fatalf("the diff warning was lost on create: %+v", r)
	}
}
