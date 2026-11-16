package abhed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// An embedded agent's policy runs the read-only classifier on the hosts the
// person's own file declares, as the CLI's and the server's do.
func TestSDKPolicyClassifiesRemoteReads(t *testing.T) {
	managedFile(t, "")
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"ssh":{"enabled":true,"hosts":[{"name":"app1","addr":"10.0.0.1","user":"ops"}]}}`
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(context.Background(), Options{Workspace: t.TempDir(), ConfigDir: t.TempDir(), Provider: testProvider})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	call := func(cmd string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"host": "app1", "command": cmd})
		return b
	}
	if got := a.loop.Policy.Evaluate("ssh", true, call("du -sh /var/log | sort -h")); got.Decision != policy.Allow || got.Rule != policy.ReadOnlyRule {
		t.Fatalf("metadata read: %+v", got)
	}
	if got := a.loop.Policy.Evaluate("ssh", true, call("cat /etc/os-release")); got.Decision != policy.Ask {
		t.Fatalf("content read without read_contents: %+v", got)
	}
}
