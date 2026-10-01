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
	got, from, err := fileOwnerAccounts(cfg, dir, false)
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
	if got, from, err := fileOwnerAccounts(cfg, ws, false); err != nil || from != "" || got != nil {
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
	if got, from, err := fileOwnerAccounts(cfg, ws, false); err != nil || from == "" || len(got) != 1 {
		t.Fatalf("default file: %v %q %v", got, from, err)
	}
}

// A configured users_file that is missing or unreadable stops the migration
// unless --force-no-accounts is given, and a relative one is found beside the
// workspace whatever directory migrate runs in.
func TestMigrateNeedsItsUsersFile(t *testing.T) {
	ws := t.TempDir()
	cfg := config.Default()
	cfg.Auth.Mode, cfg.Auth.UsersFile = "local", "state/users.json"
	if _, _, err := fileOwnerAccounts(cfg, ws, false); err == nil {
		t.Fatal("a missing users_file was accepted")
	}
	if got, _, err := fileOwnerAccounts(cfg, ws, true); err != nil || got != nil {
		t.Fatalf("forced: %v %v", got, err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(ws, "state", "users.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fileOwnerAccounts(cfg, ws, false); err == nil {
		t.Fatal("an unreadable users_file was accepted")
	}
	fs, err := auth.NewFileUserStore(bad)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(bad)
	if err := fs.Put(context.Background(), &auth.User{Username: "founder"}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if got, from, err := fileOwnerAccounts(cfg, ws, false); err != nil || len(got) != 1 || from != bad {
		t.Fatalf("relative path from another directory: %v %q %v", got, from, err)
	}
}

// migrate hands the users_file accounts to the owner migration, says where
// it found them, and refuses a missing file.
func TestMigrateCommandPassesFileAccounts(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(config.TrustEnv, "1") // the test wrote this configuration
	t.Setenv("ABHED_DATABASE_URL", "postgres://rt@127.0.0.1:1/x")
	t.Setenv("ABHED_MIGRATE_DATABASE_URL", "postgres://owner@127.0.0.1:1/x")
	users := filepath.Join(ws, "state", "users.json")
	fs, err := auth.NewFileUserStore(users)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Put(context.Background(), &auth.User{Username: "founder", Email: "founder@example.test"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	conf := `{"auth":{"mode":"local","users_file":"state/users.json"},"storage":{"driver":"postgres"}}`
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	var got store.ProvisionConfig
	saved := provision
	provision = func(_ context.Context, c store.ProvisionConfig) error { got = c; return nil }
	defer func() { provision = saved }()
	if code := migrateCmd(ws, nil, nil, config.TrustGranted); code != 0 {
		t.Fatalf("migrate exited %d", code)
	}
	if got.Owners != store.OwnersLocalOnly || got.AllowNoAccounts || len(got.OwnerAccounts) != 1 ||
		got.OwnerAccounts[0].Email != "founder@example.test" {
		t.Fatalf("provision got owners %q allow %v accounts %+v", got.Owners, got.AllowNoAccounts, got.OwnerAccounts)
	}
	if code := migrateCmd(ws, []string{"--force-no-accounts"}, nil, config.TrustGranted); code != 0 || !got.AllowNoAccounts {
		t.Fatalf("--force-no-accounts: exit %d, allow %v", code, got.AllowNoAccounts)
	}
}
