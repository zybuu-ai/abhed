package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedRepo(t *testing.T) (*Session, string) { //nolint:unparam // a fixture; the fixed argument documents what the tests rely on
	t.Helper()
	s, dir := setup(t)
	mk := func(rel, content string) {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(content), 0o644)
	}
	mk("main.go", "package main\n\nfunc main() {\n\thandleRequest()\n}\n")
	mk("pkg/auth/auth.go", "package auth\n\nfunc Login() error {\n\treturn nil\n}\n")
	mk("pkg/auth/auth_test.go", "package auth\n\nfunc TestLogin(t *testing.T) {}\n")
	mk("web/app.ts", "export function handleRequest() {}\n")
	mk("node_modules/dep/index.js", "function handleRequest(){}\n") // must be skipped
	mk(".git/config", "handleRequest\n")                            // must be skipped
	return s, dir
}

func TestGlobRecursive(t *testing.T) {
	s, _ := seedRepo(t)
	res := run(t, Glob{}, s, globArgs{Pattern: "**/*.go"})
	if res.IsError {
		t.Fatal(res.Content)
	}
	for _, want := range []string{"main.go", "pkg/auth/auth.go", "pkg/auth/auth_test.go"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("missing %s in:\n%s", want, res.Content)
		}
	}
}

func TestGlobBarePatternMatchesSubdirs(t *testing.T) {
	s, _ := seedRepo(t)
	res := run(t, Glob{}, s, globArgs{Pattern: "*.go"})
	if !strings.Contains(res.Content, "pkg/auth/auth.go") {
		t.Fatalf("bare *.go should find nested files, got:\n%s", res.Content)
	}
}

func TestGlobSkipsVendorDirs(t *testing.T) {
	s, _ := seedRepo(t)
	res := run(t, Glob{}, s, globArgs{Pattern: "**/*.js"})
	if strings.Contains(res.Content, "node_modules") {
		t.Fatalf("node_modules must be skipped:\n%s", res.Content)
	}
}

func TestGlobNoMatchIsExplicit(t *testing.T) {
	s, _ := seedRepo(t)
	res := run(t, Glob{}, s, globArgs{Pattern: "**/*.rs"})
	if !strings.Contains(res.Content, "no files matched") {
		t.Fatalf("empty result must say so: %q", res.Content)
	}
}

func TestGrepFindsAcrossFiles(t *testing.T) {
	s, _ := seedRepo(t)
	res := run(t, Grep{}, s, grepArgs{Pattern: "handleRequest"})
	if res.IsError {
		t.Fatal(res.Content)
	}
	if !strings.Contains(res.Content, "main.go") || !strings.Contains(res.Content, "web/app.ts") {
		t.Fatalf("expected both files:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "node_modules") {
		t.Fatalf("vendored dirs must be skipped:\n%s", res.Content)
	}
}

func TestGrepGlobFilter(t *testing.T) {
	s, _ := seedRepo(t)
	res := run(t, Grep{}, s, grepArgs{Pattern: "handleRequest", Glob: "*.ts"})
	if strings.Contains(res.Content, "main.go") {
		t.Fatalf("glob filter ignored:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "app.ts") {
		t.Fatalf("expected app.ts:\n%s", res.Content)
	}
}

func TestGrepContentModeShowsLineNumbers(t *testing.T) {
	s, _ := seedRepo(t)
	res := run(t, Grep{}, s, grepArgs{Pattern: "func Login", OutputMode: "content"})
	if !strings.Contains(res.Content, "auth.go") {
		t.Fatalf("expected file header:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "3") {
		t.Fatalf("expected line number:\n%s", res.Content)
	}
}

// RE2 rejects lookahead; the error must teach the model what to do instead.
func TestGrepUnsupportedRegexExplains(t *testing.T) {
	s, _ := seedRepo(t)
	res := run(t, Grep{}, s, grepArgs{Pattern: "foo(?=bar)"})
	if !res.IsError {
		t.Fatal("lookahead should be rejected")
	}
	if !strings.Contains(res.Content, "RE2") || !strings.Contains(res.Content, "lookahead") {
		t.Fatalf("error must name RE2 and the construct: %s", res.Content)
	}
}

func TestGrepNoMatchSuggestsBroadening(t *testing.T) {
	s, _ := seedRepo(t)
	res := run(t, Grep{}, s, grepArgs{Pattern: "zzznotfound"})
	if res.IsError {
		t.Fatal("no match is not an error")
	}
	if !strings.Contains(res.Content, "No matches") {
		t.Fatalf("got: %s", res.Content)
	}
}

func TestGrepCaseInsensitive(t *testing.T) {
	s, _ := seedRepo(t)
	res := run(t, Grep{}, s, grepArgs{Pattern: "HANDLEREQUEST", CaseInsensitive: true})
	if !strings.Contains(res.Content, "main.go") {
		t.Fatalf("case-insensitive search failed:\n%s", res.Content)
	}
}

func TestGrepCountMode(t *testing.T) {
	s, _ := seedRepo(t)
	res := run(t, Grep{}, s, grepArgs{Pattern: "package", OutputMode: "count"})
	if !strings.Contains(res.Content, "matches across") {
		t.Fatalf("count mode should summarize:\n%s", res.Content)
	}
}

// The worktrees of isolated subagents are copies of the workspace; glob and
// grep pass over them, as over vendored code.
func TestSearchSkipsWorktrees(t *testing.T) {
	s, dir := seedRepo(t)
	p := filepath.Join(dir, WorktreesDir, "k3f9q2", "main.go")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("package main\n\nfunc handleRequest() {}\n"), 0o644)
	for _, res := range []Result{
		run(t, Glob{}, s, globArgs{Pattern: "**/*.go"}),
		run(t, Grep{}, s, grepArgs{Pattern: "handleRequest"}),
	} {
		if strings.Contains(res.Content, WorktreesDir) {
			t.Fatalf("a worktree was searched:\n%s", res.Content)
		}
	}
}
