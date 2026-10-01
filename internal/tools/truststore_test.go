package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// The workspace-trust store is state in the home directory: a session rooted
// there can neither read nor rewrite it, nor plant one beside it.
func TestToolsCannotTouchTheTrustStore(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	state := filepath.Join(home, tools.StateDir)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := config.TrustStorePath()
	if err != nil || filepath.Dir(store) != state {
		t.Fatalf("the trust store %q is not in the state directory %s: %v", store, state, err)
	}
	if err := os.WriteFile(store, []byte(`{"version":1,"workspaces":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := tools.NewSession(home)
	if err != nil {
		t.Fatal(err)
	}
	grant := `{"version":1,"workspaces":{"/repo":{"sha256":"x","decision":"trusted"}}}`
	for _, c := range []struct {
		tool tools.Tool
		args map[string]any
	}{
		{tools.Read{}, map[string]any{"path": store}},
		{tools.Write{}, map[string]any{"path": store, "content": grant}},
		{tools.Edit{}, map[string]any{"path": store, "old_string": `{}`, "new_string": `{"/repo":{}}`}},
		{tools.Write{}, map[string]any{"path": filepath.Join(state, "trust.json.new"), "content": grant}},
	} {
		raw, _ := json.Marshal(c.args)
		if res := c.tool.Run(context.Background(), s, raw); !res.IsError || !strings.Contains(res.Content, "own state") {
			t.Errorf("%s %s: not refused: %q", c.tool.Name(), c.args["path"], res.Content)
		}
	}
	if got, _ := os.ReadFile(store); string(got) != `{"version":1,"workspaces":{}}` {
		t.Fatalf("the trust store was changed: %s", got)
	}
	if _, err := os.Stat(filepath.Join(state, "trust.json.new")); err == nil {
		t.Fatal("a file was planted beside the trust store")
	}
}
