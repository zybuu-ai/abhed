package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/auth"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/store"
)

// Only a local-accounts deployment with nothing beside it may move rows to
// accounts; every other mode, or none, unclaims them.
func TestOwnerPolicyFollowsTheAuthMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*config.Config)
		want store.OwnerPolicy
	}{
		{"local", func(c *config.Config) { c.Auth.Mode = "local" }, store.OwnersLocalOnly},
		{"local with a provider", func(c *config.Config) {
			c.Auth.Mode, c.Auth.Provider, c.Auth.ClientID = "local", "google", "id"
		}, store.OwnersUnclaim},
		{"proxy", func(c *config.Config) { c.Auth.Mode = "proxy" }, store.OwnersUnclaim},
		{"oidc", func(c *config.Config) { c.Auth.Mode = "oidc" }, store.OwnersUnclaim},
		{"none", func(c *config.Config) { c.Auth.Mode = "none" }, store.OwnersUnclaim},
		{"unset", func(c *config.Config) { c.Auth.Mode = "" }, store.OwnersUnclaim},
	} {
		cfg := config.Default()
		tc.set(&cfg)
		if got := ownerPolicy(cfg); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
		if got := storeConfig(cfg).Owners; got != tc.want {
			t.Errorf("%s: a single-role start would use %s, want %s", tc.name, got, tc.want)
		}
	}
}

// The owner migration reads the accounts in auth.users_file, or in the
// default file when one exists, with their tenants.
func TestMigrateReadsTheUsersFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "users.json")
	fs, err := auth.NewFileUserStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []*auth.User{{Username: "founder", Email: "founder@example.test"}, {Username: "ann", Tenant: "t2"}} {
		if err := fs.Put(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.Auth.Mode, cfg.Auth.UsersFile = "local", path
	got, from, err := fileOwnerAccounts(cfg, dir)
	if err != nil || from != path || len(got) != 2 {
		t.Fatalf("users_file: %d accounts from %q: %v", len(got), from, err)
	}
	tenants := map[string]string{}
	for _, u := range got {
		tenants[u.Username] = u.Tenant
	}
	if tenants["ann"] != "t2" || tenants["founder"] != "" {
		t.Fatalf("tenants %v", tenants)
	}

	// No users_file and no default file: the table alone, nothing created.
	cfg.Auth.UsersFile = ""
	ws := t.TempDir()
	if got, from, err := fileOwnerAccounts(cfg, ws); err != nil || from != "" || got != nil {
		t.Fatalf("no file: %v %q %v", got, from, err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".abhed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("looking for the default file created its directory")
	}
	// The default file beside the workspace config counts when it exists.
	def, err := auth.NewFileUserStore(usersFile(cfg, ws))
	if err != nil {
		t.Fatal(err)
	}
	if err := def.Put(context.Background(), &auth.User{Username: "bob"}); err != nil {
		t.Fatal(err)
	}
	if got, from, err := fileOwnerAccounts(cfg, ws); err != nil || from == "" || len(got) != 1 {
		t.Fatalf("default file: %v %q %v", got, from, err)
	}
}
