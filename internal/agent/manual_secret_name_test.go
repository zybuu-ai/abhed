package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// The workbench's own file actions keep stored secrets out of file names as
// the agent's write and edit do: a save, a new folder and a rename's new name.
func TestManualActionsRefuseASecretFileName(t *testing.T) {
	const value = "hunter2-s3cr3t-value"
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("TOKEN", value); err != nil {
		t.Fatal(err)
	}
	l, store := manualLoop(t)
	l.Tools = tools.NewRegistry(tools.Bash{}, tools.Write{})
	l.Recorder.Redact = vault.Redactor()
	args := func(m map[string]string) json.RawMessage { b, _ := json.Marshal(m); return b }

	if _, refused, err := l.ManualAuthorize("write", "u1", args(map[string]string{"path": "/w/leak-" + value + ".md", "content": ""})); err != nil || refused == nil {
		t.Fatalf("a save to a secret-named file was not refused: %v %v", refused, err)
	}
	cases := map[string]struct {
		action string
		args   map[string]string
	}{
		"u2": {"mkdir", map[string]string{"path": "/w/" + value}},
		"u3": {"rename", map[string]string{"path": "/w/notes.md", "to": "/w/notes-" + value + ".md"}},
	}
	for id, c := range cases {
		res, err := l.ManualAs(context.Background(), nil, c.action, id, args(c.args), "bash", bashArgs("true"))
		if err != nil || !res.IsError || !strings.Contains(res.Content, "file names") {
			t.Fatalf("%s was not refused: %+v %v", c.action, res, err)
		}
	}
	for _, id := range []string{"u1", "u2", "u3"} {
		ds := decisionsOf(t, store, id)
		if len(ds) != 1 || ds[0].Type != EvActionDenied || strings.Contains(string(ds[0].Payload), value) {
			t.Fatalf("%s recorded %+v", id, ds)
		}
	}
	if _, refused, err := l.ManualAuthorize("write", "u4", args(map[string]string{"path": "/w/plain.md", "content": ""})); err != nil || refused != nil {
		t.Fatalf("an ordinary save was refused: %v %v", refused, err)
	}
}
