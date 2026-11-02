package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/zybuu-ai/abhed/internal/agent"
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

// dsnFor is dsn with its database replaced by db.
func dsnFor(t *testing.T, dsn, db string) string {
	t.Helper()
	c, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u := c.User
	if c.Password != "" {
		u += ":" + c.Password
	}
	return fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=disable", u, c.Host, c.Port, db)
}

// The rights Postgres 13 and 14 give a new database, made on any version
// with a superuser: schema public belongs to the superuser, and PUBLIC may
// create in it, so the owner role creates there only through PUBLIC. A
// migrate cannot fix that itself and must say the whole fix, which a
// superuser then runs; the next migrate completes and the server starts.
// Routes the runtime role may have besides PUBLIC (a group, held without
// INHERIT; owning the database) are named, and a group's table privilege is
// found by the record check too. Needs ABHED_TEST_SUPERUSER_DSN.
func TestMigrateOnPostgres14DefaultRights(t *testing.T) {
	super := os.Getenv("ABHED_TEST_SUPERUSER_DSN")
	if super == "" {
		t.Skip("set ABHED_TEST_SUPERUSER_DSN to a superuser to recreate Postgres 13 and 14's default rights")
	}
	ownerDSN, runtimeDSN := testDSN(t), os.Getenv("ABHED_TEST_RUNTIME_DSN")
	if runtimeDSN == "" {
		t.Skip("set ABHED_TEST_RUNTIME_DSN")
	}
	ctx := context.Background()
	oc, _ := pgx.ParseConfig(ownerDSN)
	rc, _ := pgx.ParseConfig(runtimeDSN)
	sc, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close(ctx)
	db := strings.ReplaceAll(testID(t, "pg14_"), "-", "_")
	if _, err := sc.Exec(ctx, `CREATE DATABASE `+db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = sc.Exec(context.Background(), `DROP DATABASE IF EXISTS `+db+` WITH (FORCE)`)
	})
	s, err := pgx.Connect(ctx, dsnFor(t, super, db))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	sql := func(q string) {
		t.Helper()
		if _, err := s.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	sql(`ALTER SCHEMA public OWNER TO ` + pgx.Identifier{sc.Config().User}.Sanitize())
	sql(`GRANT CREATE, USAGE ON SCHEMA public TO PUBLIC`)
	owner, runtime := oc.User, rc.User
	ow, rt := pgx.Identifier{owner}.Sanitize(), pgx.Identifier{runtime}.Sanitize()
	odsn, rdsn := dsnFor(t, ownerDSN, db), dsnFor(t, runtimeDSN, db)

	// The migrate refuses, leaves the owner's CREATE alone, and says the whole fix.
	err = Provision(ctx, ProvisionConfig{OwnerDSN: odsn, RuntimeRole: runtime})
	want := `GRANT CREATE ON SCHEMA public TO ` + ow + `; REVOKE CREATE ON SCHEMA public FROM PUBLIC;`
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "superuser") ||
		!strings.Contains(err.Error(), "PUBLIC's grant") {
		t.Fatalf("migrate on Postgres 14's default rights: %v; want the refusal naming PUBLIC and %q", err, want)
	}
	var ownerCan bool
	if err := s.QueryRow(ctx, `SELECT has_schema_privilege($1, 'public', 'CREATE')`, owner).Scan(&ownerCan); err != nil || !ownerCan {
		t.Fatalf("the refused migrate took the owner's CREATE away (%v)", err)
	}
	// The superuser runs exactly that; the next migrate completes.
	sql(`GRANT CREATE ON SCHEMA public TO ` + ow)
	sql(`REVOKE CREATE ON SCHEMA public FROM PUBLIC`)
	if err := Provision(ctx, ProvisionConfig{OwnerDSN: odsn, RuntimeRole: runtime}); err != nil {
		t.Fatalf("migrate after the fix: %v", err)
	}
	cfg := DefaultConfig(rdsn)
	cfg.Tenant = "t-pg14"
	p, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("runtime open after the fix: %v", err)
	}
	id := testID(t, "sess-pg14-")
	newSession(t, p, id, "t-pg14")
	if err := p.Append(ev(id, 1, agent.EvAgentMessage, agent.Trusted, map[string]int{"n": 1})); err != nil {
		t.Fatal(err)
	}
	p.Close()

	open := func() error {
		q, err := Open(ctx, cfg)
		if err == nil {
			q.Close()
		}
		return err
	}
	// A group the runtime role belongs to without INHERIT: SET ROLE still
	// reaches its CREATE, and its UPDATE on events.
	g := strings.ReplaceAll(testID(t, "abhed_grp_"), "-", "_")
	sql(`CREATE ROLE ` + g + ` NOLOGIN`)
	t.Cleanup(func() {
		_, _ = s.Exec(context.Background(), `DROP OWNED BY `+g)
		_, _ = sc.Exec(context.Background(), `DROP ROLE IF EXISTS `+g)
	})
	if _, err := s.Exec(ctx, `GRANT `+g+` TO `+rt+` WITH INHERIT FALSE`); err != nil {
		sql(`GRANT ` + g + ` TO ` + rt) // before Postgres 16
		sql(`ALTER ROLE ` + rt + ` NOINHERIT`)
		t.Cleanup(func() { _, _ = sc.Exec(context.Background(), `ALTER ROLE `+rt+` INHERIT`) })
	}
	t.Cleanup(func() { _, _ = sc.Exec(context.Background(), `REVOKE `+g+` FROM `+rt) })
	sql(`GRANT CREATE ON SCHEMA public TO ` + g)
	if err := open(); err == nil || !strings.Contains(err.Error(), "a grant to "+g+", a role it belongs to") {
		t.Fatalf("runtime open with CREATE through a group held without INHERIT: %v; want the group named", err)
	}
	if err := Provision(ctx, ProvisionConfig{OwnerDSN: odsn, RuntimeRole: runtime}); err == nil ||
		!strings.Contains(err.Error(), "REVOKE CREATE ON SCHEMA public FROM "+pgx.Identifier{g}.Sanitize()) {
		t.Fatalf("migrate with CREATE through a group: %v; want the revoke from the group", err)
	}
	sql(`REVOKE CREATE ON SCHEMA public FROM ` + g)
	sql(`GRANT UPDATE ON public.events TO ` + g)
	if err := open(); err == nil || !strings.Contains(err.Error(), "holds UPDATE on events") {
		t.Fatalf("runtime open with UPDATE on events through a group held without INHERIT: %v", err)
	}
	sql(`REVOKE UPDATE ON public.events FROM ` + g)
	if err := open(); err != nil {
		t.Fatalf("runtime open with the group's rights revoked: %v", err)
	}
	// Owning the database makes it a member of pg_database_owner (Postgres
	// 15 and later), the owner of schema public there.
	var v int
	if err := s.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v >= 150000 {
		sql(`ALTER SCHEMA public OWNER TO pg_database_owner`)
		sql(`ALTER DATABASE ` + db + ` OWNER TO ` + rt)
		if err := open(); err == nil || !strings.Contains(err.Error(), "owning the schema") {
			t.Fatalf("runtime open as the database's owner: %v; want ownership named", err)
		}
	}
}
