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
