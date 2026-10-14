package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The prompt names the project's own test command, from the file that says
// it, and says when to use ask_user and task where the session has them.
func TestPromptNamesTheTestCommandAndToolHints(t *testing.T) {
	for files, want := range map[string]string{
		"go.mod":                      "Tests: go test ./... (from go.mod)",
		"Cargo.toml":                  "Tests: cargo test (from Cargo.toml)",
		"package.json|pnpm-lock.yaml": "Tests: pnpm test (from package.json)",
		"pyproject.toml":              "Tests: pytest (from pyproject.toml)",
		"Makefile":                    "Tests: make test (from Makefile)",
		"README.md":                   "",
	} {
		ws := t.TempDir()
		for _, f := range strings.Split(files, "|") {
			body := map[string]string{"package.json": `{"scripts":{"test":"vitest"}}`, "pyproject.toml": "[tool.pytest.ini_options]\n",
				"Makefile": "build:\n\tgo build\ntest:\n\tgo test\n"}[f]
			if err := os.WriteFile(filepath.Join(ws, f), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		p := BuildSystemPrompt(BuildOptions{Profile: "main", Workspace: ws})
		if want == "" && strings.Contains(p, "Tests: ") || want != "" && !strings.Contains(p, want) {
			t.Errorf("%s: want %q in the environment", files, want)
		}
	}
	with := BuildSystemPrompt(BuildOptions{Profile: "main", Tools: []string{"ask_user", "task"}})
	without := BuildSystemPrompt(BuildOptions{Profile: "main", Tools: []string{"read"}})
	if !strings.Contains(with, "call ask_user") || !strings.Contains(with, "subagent with task") ||
		strings.Contains(without, "ask_user") || strings.Contains(without, "subagent with task") {
		t.Error("the tool hints do not follow the session's tools")
	}
}

// Outside any repository the prompt is built without starting git, whose
// start alone held the first prompt back by hundreds of milliseconds on macOS;
// wherever git could find one, it is still asked.
func TestMayBeInRepo(t *testing.T) {
	if mayBeInRepo(t.TempDir()) && repoAbove(t.TempDir()) == "" {
		t.Fatal("a folder outside any repository read as a possible one")
	}
	for name, mk := range map[string]func(dir string) error{
		".git folder": func(dir string) error { return os.Mkdir(filepath.Join(dir, ".git"), 0o700) },
		".git file":   func(dir string) error { return os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: x\n"), 0o600) },
		"bare": func(dir string) error {
			for _, d := range []string{"objects", "refs"} {
				if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
					return err
				}
			}
			return os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600)
		},
	} {
		root := t.TempDir()
		if err := mk(root); err != nil {
			t.Fatal(err)
		}
		sub := filepath.Join(root, "a", "b")
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		if !mayBeInRepo(root) || !mayBeInRepo(sub) {
			t.Errorf("%s: not read as a possible repository", name)
		}
	}
}

// repoAbove is the nearest folder at or above dir holding a .git, or "".
func repoAbove(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}
