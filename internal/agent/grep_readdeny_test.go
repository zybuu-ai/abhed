package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// The loop holds grep's results to the read deny rules: a match in a file
// the agent may not read never reaches the model.
func TestGrepResultsKeepReadDenyRules(t *testing.T) {
	dir := tempDir(t)
	for rel, content := range map[string]string{"app.go": "KEY_NAME used\n", "vault/prod.env": "KEY_NAME=s3cr3t-value\n"} {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l, _ := harnessIn(t, dir, []scriptedTurn{
		{calls: []model.ToolCall{call("grep", map[string]any{"pattern": "KEY_NAME", "output_mode": "content"})}},
		{text: "done"},
	}, policy.ModeDefault, true)
	if err := l.Policy.AddDeny("read(vault/**)"); err != nil {
		t.Fatal(err)
	}
	l.Policy.Roots = func() []string { return []string{dir} }
	if _, err := l.Run(context.Background(), "find KEY_NAME"); err != nil {
		t.Fatal(err)
	}
	got := lastToolResult(l)
	if strings.Contains(got, "s3cr3t") || strings.Contains(got, "prod.env") {
		t.Fatalf("a read-denied file's match reached the model:\n%s", got)
	}
	if !strings.Contains(got, "app.go") {
		t.Fatalf("the allowed match is missing:\n%s", got)
	}
}
