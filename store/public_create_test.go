package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Postgres before 15 lets every role create in schema public. A migrate
// revokes that from PUBLIC where it may, refuses while the runtime role
// still has it, and a runtime server with it refuses to start. The grant to
// PUBLIC is made here as Postgres 13 and 14 make it, which needs the owner
// role to own schema public; elsewhere the test skips.
func TestMigrateClosesCreateOnPublic(t *testing.T) {
	owner, runtime := testDSN(t), os.Getenv("ABHED_TEST_RUNTIME_DSN")
	if runtime == "" {
		t.Skip("set ABHED_TEST_RUNTIME_DSN to run the privilege-separation tests")
	}
	ctx := context.Background()
	o := openStore(t, "t-public")
	var owns bool
	if err := o.pool.QueryRow(ctx, `SELECT nspowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
		FROM pg_namespace WHERE nspname = 'public'`).Scan(&owns); err != nil {
		t.Fatal(err)
	}
	if !owns {
		t.Skip("the owner role does not own schema public, so it cannot grant CREATE on it to PUBLIC as Postgres 13 and 14 do")
	}
	rc, err := pgx.ParseConfig(runtime)
	if err != nil {
		t.Fatal(err)
	}
	role := pgx.Identifier{rc.User}.Sanitize()
	can := func() bool {
		t.Helper()
		var c bool
		if err := o.pool.QueryRow(ctx, `SELECT has_schema_privilege($1, 'public', 'CREATE')`, rc.User).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	t.Cleanup(func() {
		_, _ = o.pool.Exec(ctx, `REVOKE CREATE ON SCHEMA public FROM PUBLIC`)
		_, _ = o.pool.Exec(ctx, `REVOKE CREATE ON SCHEMA public FROM `+role)
	})

	// A migrated database first, so only the grant below stands in the way.
	if err := Provision(ctx, ProvisionConfig{OwnerDSN: owner, RuntimeRole: rc.User}); err != nil {
		t.Fatal(err)
	}
	// As on Postgres 13 and 14: a runtime server refuses, a migrate closes it.
	if _, err := o.pool.Exec(ctx, `GRANT CREATE ON SCHEMA public TO PUBLIC`); err != nil {
		t.Fatal(err)
	}
	if !can() {
		t.Fatal("the grant to PUBLIC did not reach the runtime role")
	}
	cfg := DefaultConfig(runtime)
	cfg.Tenant = "t-public"
	if p, err := Open(ctx, cfg); err == nil {
		p.Close()
		t.Fatal("a runtime server started while it could create in schema public")
	} else if !strings.Contains(err.Error(), "schema public") {
		t.Fatalf("open: %v; want the refusal naming schema public", err)
	}
	if err := Provision(ctx, ProvisionConfig{OwnerDSN: owner, RuntimeRole: rc.User}); err != nil {
		t.Fatalf("migrate with CREATE granted to PUBLIC: %v", err)
	}
	if can() {
		t.Fatal("the migrate left the runtime role able to create in schema public")
	}
	p, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open after the migrate: %v", err)
	}
	p.Close()

	// A grant to the runtime role itself is not undone: the migrate refuses.
	if _, err := o.pool.Exec(ctx, `GRANT CREATE ON SCHEMA public TO `+role); err != nil {
		t.Fatal(err)
	}
	if err := Provision(ctx, ProvisionConfig{OwnerDSN: owner, RuntimeRole: rc.User}); err == nil ||
		!strings.Contains(err.Error(), "REVOKE CREATE ON SCHEMA public") {
		t.Fatalf("migrate with CREATE granted to the runtime role: %v; want a refusal saying what to revoke", err)
	}
}
