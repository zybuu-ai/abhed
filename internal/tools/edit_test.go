package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T) (*Session, string) {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	s, err := NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, tool Tool, s *Session, args any) Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Run(context.Background(), s, raw)
}

// The core safety property: an edit cannot touch a file the model never read.
func TestEditRequiresPriorRead(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "a.go")
	writeFile(t, p, "package main\n")

	res := run(t, Edit{}, s, editArgs{Path: p, OldString: "package main", NewString: "package other"})
	if !res.IsError {
		t.Fatal("expected refusal without a prior read")
	}
	if !strings.Contains(res.Content, "not read this session") {
		t.Fatalf("error should explain the fix, got: %s", res.Content)
	}
	// File must be untouched.
	got, _ := os.ReadFile(p)
	if string(got) != "package main\n" {
		t.Fatalf("file was modified despite refusal: %q", got)
	}
}

func TestEditExactMatchAndNoFuzzy(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "a.go")
	writeFile(t, p, "func main() {\n\tx := 1\n}\n")
	run(t, Read{}, s, readArgs{Path: p})

	// Wrong indentation must NOT match - fuzzy matching would silently corrupt.
	res := run(t, Edit{}, s, editArgs{Path: p, OldString: "x := 1", NewString: "x := 2"})
	if res.IsError {
		t.Fatalf("substring without leading tab should still match as substring: %s", res.Content)
	}
	got, _ := os.ReadFile(p)
	if !strings.Contains(string(got), "x := 2") {
		t.Fatalf("edit did not apply: %q", got)
	}
}

func TestEditMultipleMatchesRefused(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "a.go")
	writeFile(t, p, "a := 1\nb := 1\nc := 1\n")
	run(t, Read{}, s, readArgs{Path: p})

	res := run(t, Edit{}, s, editArgs{Path: p, OldString: ":= 1", NewString: ":= 2"})
	if !res.IsError {
		t.Fatal("ambiguous edit must be refused, not guessed")
	}
	if !strings.Contains(res.Content, "appears 3 times") {
		t.Fatalf("error should report the count: %s", res.Content)
	}
	if !strings.Contains(res.Content, "replace_all") {
		t.Fatalf("error should name the fix: %s", res.Content)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "a := 1\nb := 1\nc := 1\n" {
		t.Fatalf("file changed on ambiguous edit: %q", got)
	}
}

func TestEditReplaceAll(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "a.go")
	writeFile(t, p, "a := 1\nb := 1\n")
	run(t, Read{}, s, readArgs{Path: p})

	res := run(t, Edit{}, s, editArgs{Path: p, OldString: ":= 1", NewString: ":= 2", ReplaceAll: true})
	if res.IsError {
		t.Fatalf("replace_all failed: %s", res.Content)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "a := 2\nb := 2\n" {
		t.Fatalf("got %q", got)
	}
}

func TestEditNoMatchSuggestsNearest(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "a.go")
	writeFile(t, p, "func handleRequest(w http.ResponseWriter) {\n\treturn\n}\n")
	run(t, Read{}, s, readArgs{Path: p})

	res := run(t, Edit{}, s, editArgs{Path: p, OldString: "func handleRequest(w http.ResponseWriter, r *http.Request) {", NewString: "x"})
	if !res.IsError {
		t.Fatal("expected no-match error")
	}
	if !strings.Contains(res.Content, "Nearest partial match") {
		t.Fatalf("should point at the near miss, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "handleRequest") {
		t.Fatalf("snippet should show the candidate line: %s", res.Content)
	}
}

func TestEditIdenticalStringsRefused(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "a.go")
	writeFile(t, p, "x\n")
	run(t, Read{}, s, readArgs{Path: p})

	res := run(t, Edit{}, s, editArgs{Path: p, OldString: "x", NewString: "x"})
	if !res.IsError || !strings.Contains(res.Content, "identical") {
		t.Fatalf("expected identical-string refusal, got: %s", res.Content)
	}
}

func TestEditDetectsExternalChange(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "a.go")
	writeFile(t, p, "original\n")
	run(t, Read{}, s, readArgs{Path: p})

	writeFile(t, p, "changed by someone else\n") // external modification

	res := run(t, Edit{}, s, editArgs{Path: p, OldString: "original", NewString: "new"})
	if !res.IsError || !strings.Contains(res.Content, "changed on disk") {
		t.Fatalf("expected stale-read detection, got: %s", res.Content)
	}
}

func TestWriteRequiresReadBeforeOverwrite(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "a.go")
	writeFile(t, p, "important content\n")

	res := run(t, Write{}, s, writeArgs{Path: p, Content: "clobbered"})
	if !res.IsError {
		t.Fatal("overwriting an unread file must be refused")
	}
	got, _ := os.ReadFile(p)
	if string(got) != "important content\n" {
		t.Fatalf("file was clobbered: %q", got)
	}
}

func TestWriteNewFileAllowed(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "new.go")
	res := run(t, Write{}, s, writeArgs{Path: p, Content: "package main\n"})
	if res.IsError {
		t.Fatalf("creating a new file should succeed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Created") {
		t.Fatalf("should report creation: %s", res.Content)
	}
}

// Path scoping: nothing outside the workspace is reachable.
func TestWorkspaceEscapeRefused(t *testing.T) {
	s, dir := setup(t)
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	writeFile(t, outside, "secret\n")
	defer func() { _ = os.Remove(outside) }()

	for _, attempt := range []string{
		outside,
		filepath.Join(dir, "..", "outside.txt"),
		"/etc/passwd",
	} {
		res := run(t, Read{}, s, readArgs{Path: attempt})
		if !res.IsError {
			t.Fatalf("escape via %q was allowed", attempt)
		}
	}
}

// A relative path is read from the workspace root, whatever a cd did, and
// held to the same confinement as an absolute one.
func TestRelativePathIsReadFromTheRoot(t *testing.T) {
	s, dir := setup(t)
	for _, d := range []string{"src", "other"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(dir, "src", "main.go"), "package main\n")
	s.Cwd = filepath.Join(dir, "other")
	res := run(t, Read{}, s, readArgs{Path: "src/main.go"})
	if res.IsError || !strings.Contains(res.Content, "package main") {
		t.Fatalf("a relative path was not read from the root: %s", res.Content)
	}
	for _, escape := range []string{"../outside.txt", "src/../../outside.txt", ".abhed/policy.json"} {
		if res := run(t, Read{}, s, readArgs{Path: escape}); !res.IsError {
			t.Fatalf("relative %q got out: %s", escape, res.Content)
		}
	}
}

func TestReadEmptyFileIsNotSilent(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "empty.txt")
	writeFile(t, p, "")

	res := run(t, Read{}, s, readArgs{Path: p})
	if res.IsError {
		t.Fatalf("empty file is not an error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "empty") {
		t.Fatalf("empty read must say so rather than return nothing: %q", res.Content)
	}
}

func TestReadTruncationTellsModelHowToContinue(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "big.txt")
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("line\n")
	}
	writeFile(t, p, b.String())

	res := run(t, Read{}, s, readArgs{Path: p, Limit: 10})
	if !res.Truncated {
		t.Fatal("expected truncation flag")
	}
	if !strings.Contains(res.Content, "offset/limit") {
		t.Fatalf("truncation notice must explain continuation: %s", res.Content)
	}
}

func TestReadDirectorySuggestsGlob(t *testing.T) {
	s, dir := setup(t)
	res := run(t, Read{}, s, readArgs{Path: dir})
	if !res.IsError || !strings.Contains(res.Content, "glob") {
		t.Fatalf("expected glob suggestion for a directory: %s", res.Content)
	}
}

func TestReadMissingFileSuggestsGlob(t *testing.T) {
	s, dir := setup(t)
	res := run(t, Read{}, s, readArgs{Path: filepath.Join(dir, "nope.go")})
	if !res.IsError || !strings.Contains(res.Content, "glob") {
		t.Fatalf("missing-file error should point to a recovery: %s", res.Content)
	}
}

func TestReadBinaryRefused(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "bin")
	if err := os.WriteFile(p, []byte{0x7f, 'E', 'L', 'F', 0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	res := run(t, Read{}, s, readArgs{Path: p})
	if !res.IsError || !strings.Contains(res.Content, "binary") {
		t.Fatalf("expected binary refusal: %s", res.Content)
	}
}

// Atomic write: the original survives if the write fails partway.
func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "a.txt")
	run(t, Write{}, s, writeArgs{Path: p, Content: "hello"})

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".abhed-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// Indentation alone may differ: a unique line-for-line match once leading
// whitespace is set aside is applied, re-indented to the file's, and said.
func TestEditToleratesIndentationOnly(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "sum.go")
	writeFile(t, p, "package x\n\nfunc sum(xs []int) int {\n\ttotal := 0\n\tfor _, x := range xs {\n\t\ttotal += x\n\t}\n\treturn total\n}\n")
	run(t, Read{}, s, readArgs{Path: p})
	res := run(t, Edit{}, s, editArgs{Path: p,
		OldString: "\t\ttotal := 0\n\t\tfor _, x := range xs {\n\t\t\ttotal += x",
		NewString: "\t\ttotal := 1\n\t\tfor _, x := range xs {\n\t\t\ttotal *= x"})
	if res.IsError || !strings.Contains(res.Content, "Matched loosely at line 4") {
		t.Fatalf("%s", res.Content)
	}
	got, _ := os.ReadFile(p)
	want := "package x\n\nfunc sum(xs []int) int {\n\ttotal := 1\n\tfor _, x := range xs {\n\t\ttotal *= x\n\t}\n\treturn total\n}\n"
	if string(got) != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}

	// Spaces for tabs too.
	run(t, Read{}, s, readArgs{Path: p})
	res = run(t, Edit{}, s, editArgs{Path: p, OldString: "    return total", NewString: "    return total + 0"})
	if got, _ := os.ReadFile(p); res.IsError || !strings.Contains(string(got), "\n\treturn total + 0\n") {
		t.Fatalf("%s\n%s", res.Content, got)
	}
}

// Two places that match once indentation is set aside refuse, and nothing
// that differs in more than indentation is matched.
func TestEditLooseMatchStillRefusesAmbiguityAndContent(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "a.go")
	body := "func a() {\n\tx++\n}\n\nfunc b() {\n\t\tx++\n}\n"
	writeFile(t, p, body)
	run(t, Read{}, s, readArgs{Path: p})
	res := run(t, Edit{}, s, editArgs{Path: p, OldString: "   x++", NewString: "   x--"})
	if !res.IsError || !strings.Contains(res.Content, "2 places") {
		t.Fatalf("%s", res.Content)
	}
	res = run(t, Edit{}, s, editArgs{Path: p, OldString: "\t\tx +=1", NewString: "\t\tx--"})
	if !res.IsError || !strings.Contains(res.Content, "not found") {
		t.Fatalf("%s", res.Content)
	}
	if got, _ := os.ReadFile(p); string(got) != body {
		t.Fatalf("the file changed: %q", got)
	}
}

// A file with CRLF line ends keeps them through a loose match.
func TestEditLooseMatchKeepsCRLF(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "w.txt")
	writeFile(t, p, "a\r\n\tb\r\nc\r\n")
	run(t, Read{}, s, readArgs{Path: p})
	res := run(t, Edit{}, s, editArgs{Path: p, OldString: "  b", NewString: "  B"})
	if got, _ := os.ReadFile(p); res.IsError || string(got) != "a\r\n\tB\r\nc\r\n" {
		t.Fatalf("%s %q", res.Content, got)
	}
}

// A loose match that would move a line into or out of a block is refused:
// the reviewer's case, where cleanup() would have gone inside if c:.
func TestEditLooseMatchKeepsNesting(t *testing.T) {
	s, dir := setup(t)
	p := filepath.Join(dir, "m.py")
	body := "def f(c):\n    if c:\n        do()\n    cleanup()\n"
	writeFile(t, p, body)
	run(t, Read{}, s, readArgs{Path: p})
	res := run(t, Edit{}, s, editArgs{Path: p,
		OldString: "if c:\n    do()\n    cleanup()",
		NewString: "if c:\n    do()\n    cleanup()\n    log()"})
	if got, _ := os.ReadFile(p); !res.IsError || !strings.Contains(res.Content, "nested differently") || string(got) != body {
		t.Fatalf("%s\n%s", res.Content, got)
	}
	// Two old indentations onto one in the file is refused too.
	writeFile(t, p, "a\n  b\n  c\n")
	run(t, Read{}, s, readArgs{Path: p})
	if res := run(t, Edit{}, s, editArgs{Path: p, OldString: "a\n  b\n    c", NewString: "a\n  b\n    C"}); !res.IsError {
		t.Fatalf("%s", res.Content)
	}
}
