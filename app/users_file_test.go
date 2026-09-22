package app

import (
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/store"
)

// Accounts live beside the workspace config unless the operator says
// otherwise, and then exactly where they said.
func TestUsersFileFollowsTheConfiguration(t *testing.T) {
	cfg := config.Default()
	if got := usersFile(cfg, "/ws"); got != filepath.Join("/ws", ".abhed", "users.json") {
		t.Fatalf("default: %s", got)
	}
	cfg.Auth.UsersFile = "/state/users.json"
	if got := usersFile(cfg, "/ws"); got != "/state/users.json" {
		t.Fatalf("configured: %s", got)
	}
}

func TestMigrateExtensionsAreCollected(t *testing.T) {
	a := newApp(WithMigrateExtension(store.Extension{SQL: "CREATE TABLE IF NOT EXISTS x (id text)", Grants: map[string]string{"x": "SELECT"}}))
	if len(a.migrate) != 1 || a.migrate[0].Grants["x"] != "SELECT" {
		t.Fatalf("extensions: %+v", a.migrate)
	}
}
