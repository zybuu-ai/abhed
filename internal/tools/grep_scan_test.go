package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeRipgrep writes, outside any workspace, a program standing in for
// ripgrep that names everything under the folder it searches, state, links
// and vendored folders included, as holding a match, and makes
// it the one grep runs for the test. A file it names must still pass every
// check the Go path makes.
func fakeRipgrep(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	marker := filepath.Join(bin, "ran")
	script := "#!/bin/sh\n: > " + marker + "\nwhile [ \"$1\" != \"--\" ]; do shift; done\nshift\nexec /usr/bin/find \"$1\" -print0\n"
	p := filepath.Join(bin, "rg")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	useRipgrep(t, p)
	return marker
}

func useRipgrep(t *testing.T, p string) {
	t.Helper()
	was := ripgrep
	ripgrep = p
	t.Cleanup(func() { ripgrep = was })
}

// searchPaths runs a test once without ripgrep and once with the stand-in.
func searchPaths(t *testing.T, f func(t *testing.T)) {
	t.Run("go", func(t *testing.T) {
		useRipgrep(t, "")
		f(t)
	})
	t.Run("ripgrep", func(t *testing.T) {
		marker := fakeRipgrep(t)
		f(t)
		if _, err := os.Stat(marker); err != nil {
			t.Fatal("ripgrep was not run")
		}
	})
	if real := realRipgrep(); real != "" {
		t.Run("real-ripgrep", func(t *testing.T) {
			useRipgrep(t, real)
			f(t)
		})
	}
}

// realRipgrep is an installed ripgrep to test against: ABHED_TEST_RIPGREP,
// or the one found on PATH at start-up.
func realRipgrep() string {
	if p := os.Getenv("ABHED_TEST_RIPGREP"); p != "" {
		return p
	}
	return findRipgrep()
}

func grepCtx(t *testing.T, ctx context.Context, s *Session, args grepArgs) Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return Grep{}.Run(ctx, s, raw)
}

// A match in a file the read rules deny never comes back, by either search,
// in any output mode, and is not counted.
func TestGrepLeavesOutReadDeniedFiles(t *testing.T) {
	searchPaths(t, func(t *testing.T) {
		s, dir := setup(t)
		mk := func(rel, content string) {
			p := filepath.Join(dir, rel)
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		mk("app/config.go", "token := lookup(\"TOKEN_NAME\")\n")
		mk("secrets/prod.txt", "TOKEN_NAME=hunter2\n")
		denied := filepath.Join(dir, "secrets", "prod.txt")
		ctx := WithReadCheck(context.Background(), func(p string) bool { return p == denied })
		for _, mode := range []string{"files_with_matches", "content", "count"} {
			res := grepCtx(t, ctx, s, grepArgs{Pattern: "TOKEN_NAME", OutputMode: mode})
			if strings.Contains(res.Content, "prod.txt") || strings.Contains(res.Content, "hunter2") {
				t.Fatalf("%s: a read-denied file came back:\n%s", mode, res.Content)
			}
			if !strings.Contains(res.Content, "config.go") {
				t.Fatalf("%s: the allowed match is missing:\n%s", mode, res.Content)
			}
			if mode == "count" && !strings.Contains(res.Content, "1 matches across 1 files") {
				t.Fatalf("the denied file was counted:\n%s", res.Content)
			}
		}
		// The model is told a file was not searched, whatever it holds.
		if res := grepCtx(t, ctx, s, grepArgs{Pattern: "TOKEN_NAME"}); !strings.Contains(res.Content, "1 file(s) were not searched because a read rule withholds them") {
			t.Fatalf("the withheld file is not mentioned:\n%s", res.Content)
		}
		// Named as the path, it is still left out, and said to be withheld.
		res := grepCtx(t, ctx, s, grepArgs{Pattern: "TOKEN_NAME", Path: denied, OutputMode: "content"})
		if strings.Contains(res.Content, "hunter2") || !strings.Contains(res.Content, "withheld by a read rule") {
			t.Fatalf("a read-denied file named as the path:\n%s", res.Content)
		}
		// Without the rule both match, so the test fails if the filter goes.
		res = grepCtx(t, context.Background(), s, grepArgs{Pattern: "TOKEN_NAME"})
		if !strings.Contains(res.Content, "prod.txt") {
			t.Fatalf("the fixture does not match:\n%s", res.Content)
		}
	})
}

// Abhed's state, a link and a vendored folder stay out of both searches,
// even when ripgrep names them.
func TestGrepKeepsPathRulesOnBothPaths(t *testing.T) {
	searchPaths(t, func(t *testing.T) {
		s, dir := setup(t)
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "o.txt"), []byte("needle outside\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for rel, content := range map[string]string{
			"a.txt":                    "needle here\n",
			".abhed/config.json":       "needle state\n",
			"node_modules/x/index.js":  "needle vendored\n",
			"sub/.ABHED/sessions.json": "needle state by case\n",
		} {
			p := filepath.Join(dir, rel)
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(filepath.Join(outside, "o.txt"), filepath.Join(dir, "link.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "linkdir")); err != nil {
			t.Fatal(err)
		}
		res := grepCtx(t, context.Background(), s, grepArgs{Pattern: "needle", OutputMode: "content"})
		for _, bad := range []string{"outside", "state", "vendored"} {
			if strings.Contains(res.Content, "needle "+bad) {
				t.Fatalf("found %q:\n%s", bad, res.Content)
			}
		}
		if !strings.Contains(res.Content, "needle here") {
			t.Fatalf("missing the real match:\n%s", res.Content)
		}
	})
}

// Files a .gitignore excludes are passed over before they are read, by the
// repository's own ignore file too when a subfolder is searched; ! takes a
// file back, and a path naming an ignored folder still searches it.
func TestGrepPrunesGitignored(t *testing.T) {
	searchPaths(t, func(t *testing.T) {
		s, dir := setup(t)
		for rel, content := range map[string]string{
			".gitignore":           "*.log\n/out/\nkeep/*\n!keep/me.txt\n",
			"src/.gitignore":       "gen_*.go\n",
			"src/main.go":          "needle main\n",
			"src/gen_x.go":         "needle generated\n",
			"src/debug.log":        "needle log\n",
			"out/bin.txt":          "needle out\n",
			"src/out/fine.txt":     "needle nested out\n",
			"keep/drop.txt":        "needle dropped\n",
			"keep/me.txt":          "needle kept\n",
			"docs/notes/today.txt": "needle notes\n",
		} {
			p := filepath.Join(dir, rel)
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		res := grepCtx(t, context.Background(), s, grepArgs{Pattern: "needle", OutputMode: "content"})
		for _, want := range []string{"needle main", "needle nested out", "needle kept", "needle notes"} {
			if !strings.Contains(res.Content, want) {
				t.Fatalf("missing %q:\n%s", want, res.Content)
			}
		}
		for _, bad := range []string{"needle generated", "needle log", "needle out\n", "needle dropped"} {
			if strings.Contains(res.Content, bad) {
				t.Fatalf("an ignored file was searched (%q):\n%s", bad, res.Content)
			}
		}
		// A subfolder search keeps the root's rules.
		res = grepCtx(t, context.Background(), s, grepArgs{Pattern: "needle", Path: "src", OutputMode: "content"})
		if strings.Contains(res.Content, "needle log") || !strings.Contains(res.Content, "needle main") {
			t.Fatalf("the root .gitignore did not hold for a subfolder:\n%s", res.Content)
		}
		// Named, an ignored folder is searched.
		res = grepCtx(t, context.Background(), s, grepArgs{Pattern: "needle", Path: "out", OutputMode: "content"})
		if !strings.Contains(res.Content, "needle out") {
			t.Fatalf("an ignored folder named as the path was not searched:\n%s", res.Content)
		}
		// So is a folder inside one, under an anchored rule.
		for rel, content := range map[string]string{".gitignore": "/build-out/**\n", "build-out/x/y.txt": "needle deep\n"} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		res = grepCtx(t, context.Background(), s, grepArgs{Pattern: "needle", Path: "build-out/x", OutputMode: "content"})
		if !strings.Contains(res.Content, "needle deep") {
			t.Fatalf("a folder inside an ignored one, named as the path, was not searched:\n%s", res.Content)
		}
	})
}

// Both searches give the same answer as a line-by-line match, for patterns
// with and without a literal, anchors, case folding and multiline.
func TestGrepPathsAgree(t *testing.T) {
	content := "alpha Beta\nbeta gamma\n\nfunc Handle() {\n\treturn\n}\nBETA\nτέλος Τέλος\n"
	cases := []grepArgs{
		{Pattern: "beta", OutputMode: "count"},
		{Pattern: "beta", CaseInsensitive: true, OutputMode: "count"},
		{Pattern: "^beta", OutputMode: "count"},
		{Pattern: "^$", OutputMode: "count"},
		{Pattern: `\w+ \w+`, OutputMode: "count"},
		{Pattern: "Handle\\(\\) \\{\\n\\treturn", Multiline: true, OutputMode: "count"},
		{Pattern: "τέλος", CaseInsensitive: true, OutputMode: "count"},
		{Pattern: "(gamma|Handle)", OutputMode: "content", Context: 1},
	}
	want := map[int]string{}
	for i, c := range cases {
		t.Run("", func(t *testing.T) {
			paths := []string{"go", "ripgrep"}
			if realRipgrep() != "" {
				paths = append(paths, "real-ripgrep")
			}
			for _, path := range paths {
				switch path {
				case "go":
					useRipgrep(t, "")
				case "ripgrep":
					fakeRipgrep(t)
				default:
					useRipgrep(t, realRipgrep())
				}
				s, dir := setup(t)
				if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
				got := grepCtx(t, context.Background(), s, c).Content
				if path == "go" {
					want[i] = got
				} else if got != want[i] {
					t.Fatalf("%+v: paths disagree:\ngo:\n%s\nripgrep:\n%s", c, want[i], got)
				}
			}
		})
	}
	// The counts are what a plain line-by-line reading gives.
	s, dir := setup(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for pattern, n := range map[string]string{"beta": "1 matches", "^$": "2 matches", "(?i)beta": "3 matches", "(?i)τέλος": "1 matches"} {
		if got := grepCtx(t, context.Background(), s, grepArgs{Pattern: pattern, OutputMode: "count"}).Content; !strings.Contains(got, n) {
			t.Fatalf("%s: want %s, got:\n%s", pattern, n, got)
		}
	}
}

// A ripgrep that sits in the workspace is never run: the agent could have put it there.
func TestGrepNeverRunsRipgrepFromTheWorkspace(t *testing.T) {
	s, dir := setup(t)
	marker := filepath.Join(t.TempDir(), "ran")
	p := filepath.Join(dir, "bin", "rg")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n: > "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	useRipgrep(t, p)
	res := grepCtx(t, context.Background(), s, grepArgs{Pattern: "needle"})
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a ripgrep inside the workspace ran")
	}
	if !strings.Contains(res.Content, "a.txt") {
		t.Fatalf("the Go search did not take over:\n%s", res.Content)
	}
}

func TestRequiredLiterals(t *testing.T) {
	for expr, want := range map[string]string{
		"Describe what to build": "Describe what to build",
		`func\s+(\w+)Handler`:    "Handler",
		"(?i)abhed":              "ABHED", // matched without case
		"foo|bar":                "foo,bar",
		"(foo|ba.)":              "foo,ba",
		"(foo|.)":                "",
		"a*":                     "",
		`x+abc`:                  "abc",
		`\d+`:                    "",
	} {
		lits, _ := requiredLiterals(expr)
		if got := strings.Join(lits, ","); got != want {
			t.Errorf("%q: got %q, want %q", expr, got, want)
		}
	}
}

// With a real ripgrep, only the files holding a literal are left to read.
func TestRipgrepNarrows(t *testing.T) {
	rg := realRipgrep()
	if rg == "" {
		t.Skip("no ripgrep installed; set ABHED_TEST_RIPGREP to test against one")
	}
	dir := t.TempDir()
	var files []grepFile
	for name, content := range map[string]string{"a.txt": "has Needle\n", "b.txt": "nothing\n", "c.txt": "NEEDLE\n"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, grepFile{path: p, rel: name})
	}
	for _, c := range []struct {
		expr string
		want int
	}{{"Needle", 1}, {"(?i)needle", 2}, {"zzz", 0}} {
		re := regexp.MustCompile(c.expr)
		run := startRipgrep(context.Background(), rg, dir, newGrepSpec(re, c.expr, false, false, 0))
		if run == nil {
			t.Fatalf("%s: ripgrep not used", c.expr)
		}
		if got := run.narrow(files); !run.ok || len(got) != c.want {
			t.Fatalf("%s: ok %v, %d files, want %d", c.expr, run.ok, len(got), c.want)
		}
	}
}

// A ripgrep this user can change, or whose folder this user can write, is
// not taken from PATH.
func TestRipgrepThisUserCanWriteIsNotRun(t *testing.T) {
	bin := t.TempDir()
	p := filepath.Join(bin, "rg")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if got := findRipgrep(); got != "" {
		t.Fatalf("a ripgrep in a folder this user writes was taken: %s", got)
	}
	if !ripgrepWritable(p) {
		t.Fatal("a file this user writes is not reported")
	}
	// Read-only to its owner is not enough: the owner can make it writable.
	if err := os.Chmod(p, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bin, 0o755) })
	if os.Getuid() != 0 && !ripgrepWritable(p) {
		t.Fatal("a program this user owns, read-only, is taken")
	}
	if ripgrepWritable("/bin/sh") {
		t.Fatal("a root-owned program in root-owned folders is reported as writable")
	}
}

// Whether a withheld file holds the pattern changes nothing in the answer:
// it is judged by its path before it is read, so a matching and a
// non-matching pattern answer alike, over the workspace and over its folder.
func TestGrepAnswerIsTheSameWhateverAWithheldFileHolds(t *testing.T) {
	searchPaths(t, func(t *testing.T) {
		s, dir := setup(t)
		for rel, content := range map[string]string{"app/a.go": "package app\n", "secrets/keys.yaml": "api_key: sk-live-Q7X\n"} {
			p := filepath.Join(dir, rel)
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		secrets := filepath.Join(dir, "secrets")
		read := 0
		ctx := WithReadCheck(context.Background(), func(p string) bool {
			if strings.HasPrefix(p, secrets+string(filepath.Separator)) {
				read++
				return true
			}
			return false
		})
		for _, path := range []string{"", "secrets"} {
			for _, mode := range []string{"files_with_matches", "content", "count"} {
				hit := grepCtx(t, ctx, s, grepArgs{Pattern: "sk-live-Q7X", Path: path, OutputMode: mode}).Content
				miss := grepCtx(t, ctx, s, grepArgs{Pattern: "sk-live-Q7Y", Path: path, OutputMode: mode}).Content
				if strings.ReplaceAll(hit, "sk-live-Q7X", "P") != strings.ReplaceAll(miss, "sk-live-Q7Y", "P") {
					t.Fatalf("path %q, %s: the answer depends on the withheld file:\n%s\n---\n%s", path, mode, hit, miss)
				}
			}
		}
		if read == 0 {
			t.Fatal("the read rules were never asked")
		}
	})
}
