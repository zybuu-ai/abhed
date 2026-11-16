package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/index"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// The loop holds the search tool's hits to the read rules: a deny rule, and
// an ask rule no one can answer through a search, keep a file's excerpt from
// the model, even from an index built before the rule.
func TestSearchResultsKeepReadRules(t *testing.T) {
	dir := tempDir(t)
	for rel, content := range map[string]string{
		"app/config.go":     "package app\n\n// api_key setting name\nconst apiKey = \"api_key\"\n",
		"secrets/keys.yaml": "api_key: s3cr3t-SEARCHLEAK\n",
		"deploy/.env.yaml":  "api_key: s3cr3t-ASKLEAK\n",
	} {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ix := index.New(dir)
	if err := ix.Build(context.Background(), index.DefaultBuildOptions()); err != nil {
		t.Fatal(err)
	}
	l, _ := harnessIn(t, dir, []scriptedTurn{
		{calls: []model.ToolCall{call("search", map[string]any{"query": "api_key", "limit": 10})}},
		{text: "done"},
	}, policy.ModeDefault, true)
	l.Tools.Add(&index.SearchTool{Index: ix})
	if err := l.Policy.AddDeny("read(secrets/**)"); err != nil {
		t.Fatal(err)
	}
	if err := l.Policy.AddAsk("read(**/.env*)"); err != nil {
		t.Fatal(err)
	}
	l.Policy.Roots = func() []string { return []string{dir} }
	if _, err := l.Run(context.Background(), "find the api key"); err != nil {
		t.Fatal(err)
	}
	got := lastToolResult(l)
	for _, leak := range []string{"SEARCHLEAK", "ASKLEAK", "keys.yaml", ".env.yaml"} {
		if strings.Contains(got, leak) {
			t.Fatalf("a withheld file's hit reached the model (%s):\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "config.go") {
		t.Fatalf("the allowed hit is missing:\n%s", got)
	}
}
