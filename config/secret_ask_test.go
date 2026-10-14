package config

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// The built-in rules ask before the read tool opens a file that commonly
// holds credentials, at the top of the workspace or below it.
func TestDefaultAsksBeforeReadingSecretFiles(t *testing.T) {
	ws := t.TempDir()
	pol := policy.New(policy.ModeDefault)
	if err := pol.AddAsk(Default().Permissions.Ask...); err != nil {
		t.Fatal(err)
	}
	if err := pol.AddAllow("read"); err != nil {
		t.Fatal(err)
	}
	pol.Roots = func() []string { return []string{ws} }
	for path, want := range map[string]policy.Decision{
		".env": policy.Ask, "app/.env": policy.Ask, ".env.production": policy.Ask,
		"certs/server.pem": policy.Ask, "tls.key": policy.Ask, "home/.ssh/id_ed25519": policy.Ask,
		"id_rsa.pub": policy.Ask, "main.go": policy.Allow, "docs/env.md": policy.Allow,
	} {
		args, _ := json.Marshal(map[string]string{"path": filepath.Join(ws, path)})
		if got := pol.Evaluate("read", false, args); got.Decision != want {
			t.Errorf("read %s: %s (%s), want %s", path, got.Decision, got.Reason, want)
		}
	}
}
