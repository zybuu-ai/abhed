package toolset

import (
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// The index is built to the configured read rules: a deny rule, and the
// built-in ask rules for credentials files, keep a file out of it.
func TestIndexKeepsToTheReadRules(t *testing.T) {
	ws := t.TempDir()
	cfg := config.Default()
	cfg.Permissions.Deny = append(cfg.Permissions.Deny, "read(secrets/**)")
	withheld := IndexOptions(cfg, ws).Withheld
	for rel, want := range map[string]bool{"secrets/a.yaml": true, ".env": true, "certs/server.key": true, "app/main.go": false} {
		if got := withheld(filepath.Join(ws, rel)); got != want {
			t.Errorf("%s: withheld %v, want %v", rel, got, want)
		}
	}
}
