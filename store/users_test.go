package store

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/auth"
)

// MigrateUsers is called defensively by every user operation, and one of those
// is Authenticate's lookup on the sign-in path. Without the sync.Once guard,
// each password check ran a CREATE TABLE statement first, taking DDL locks
// against a table that already existed.
func TestMigrateUsersRunsOnce(t *testing.T) {
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run store integration tests")
	}
	ctx := context.Background()
	pg, err := Open(ctx, singleRoleConfig(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pg.Close()

	if err := pg.MigrateUsers(ctx); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	// Drop the table behind the store's back. A second MigrateUsers must NOT
	// recreate it: if it does, the Once guard is gone and the DDL is running
	// on every lookup again.
	if _, err := pg.pool.Exec(ctx, `DROP TABLE IF EXISTS users`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := pg.MigrateUsers(ctx); err != nil {
		t.Fatalf("second migrate returned an error: %v", err)
	}
	var exists bool
	if err := pg.pool.QueryRow(ctx,
		`SELECT to_regclass('public.users') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check: %v", err)
	}
	if exists {
		t.Error("MigrateUsers re-ran its DDL; the sync.Once guard is missing, " +
			"so every sign-in pays for a CREATE TABLE")
	}
	// Leave the schema in place for other tests.
	pg.usersOnce = sync.Once{}
	if err := pg.MigrateUsers(ctx); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

// Accounts must round-trip through Postgres with the hash intact, or every
// imported user is locked out.
func TestUserRoundTrip(t *testing.T) {
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run store integration tests")
	}
	ctx := context.Background()
	pg, err := Open(ctx, singleRoleConfig(dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pg.Close()
	defer pg.Delete(ctx, "roundtrip")

	want := &auth.User{
		Username: "roundtrip", Email: "rt@example.com", Name: "Round Trip",
		Tenant: "default", Groups: []string{"eng", "oncall"},
		Hash: "$2a$10$abcdefghijklmnopqrstuv", MustChange: true,
	}
	if err := pg.Put(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := pg.Get(ctx, "ROUNDTRIP") // case-insensitive lookup
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Hash != want.Hash {
		t.Errorf("hash = %q, want %q — an imported user cannot sign in", got.Hash, want.Hash)
	}
	if got.Email != want.Email || got.Name != want.Name {
		t.Errorf("profile lost: %+v", got)
	}
	if len(got.Groups) != 2 || got.Groups[0] != "eng" {
		t.Errorf("groups = %v, want [eng oncall]", got.Groups)
	}
	if !got.MustChange {
		t.Error("MustChange lost in the round trip: an admin-set password " +
			"would silently become permanent")
	}
}
