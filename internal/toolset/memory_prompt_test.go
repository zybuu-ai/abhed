package toolset

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
)

type promptAdapter struct{}

func (promptAdapter) Name() string                           { return "p" }
func (promptAdapter) Profile() model.Profile                 { return model.Profile{Name: "p"} }
func (promptAdapter) CountTokens(model.Request) (int, error) { return 0, nil }
func (promptAdapter) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	return nil, nil
}

// The server's, the SDK's and eval's prompt has no read rules to ask, so it
// follows no memory import: a file a deny rule would keep out is never read.
func TestServerPromptFollowsNoMemoryImport(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	for p, c := range map[string]string{"ABHED.md": "PROJECT @.env", ".env": "ENV-CANARY"} {
		if err := os.WriteFile(filepath.Join(ws, p), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := SystemPrompt(ws, promptAdapter{}, "", nil)
	if !strings.Contains(p, "PROJECT") || strings.Contains(p, "ENV-CANARY") {
		t.Fatalf("prompt:\n%s", p)
	}
}
