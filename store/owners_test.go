package store

import (
	"context"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/auth"
)

// The version 4 move, run inside a transaction that is rolled back, so the
// shared test database keeps none of it.
func TestOwnerMigrationMovesOnlyCertainRows(t *testing.T) {
	p := openStore(t, "default")
	ctx := context.Background()
	x := strings.ToLower(testID(t, "o"))
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}

	accounts := []*auth.User{
		{Username: "bob" + x},
		{Username: "mallet" + x, Email: "bob" + x},
		{Username: "carol" + x, Email: "carol" + x + "@example.test"},
		{Username: "dave" + x, Email: "Dave" + x + "@Example.test"},
		{Username: "dup1" + x, Email: "shared" + x + "@example.test"},
		{Username: "dup2" + x, Email: "SHARED" + x + "@example.test"},
		{Username: "founder" + x, Email: "founder" + x + "@example.test"},
		{Username: "agent"},
		{Username: "anonymous"},
	}
	rows := []struct{ tenant, owner, want string }{
		{"default", "bob" + x, "unclaimed:bob" + x},
		{"default", "carol" + x + "@example.test", "local:carol" + x},
		{"default", "carol" + x, "local:carol" + x},
		{"default", "dave" + x + "@example.test", "local:dave" + x},
		{"default", "shared" + x + "@example.test", "unclaimed:shared" + x + "@example.test"},
		{"other" + x, "founder" + x + "@example.test", "local:founder" + x},
		{"default", "stranger" + x + "@example.test", "stranger" + x + "@example.test"},
		{"default", "oidc:" + x, "oidc:" + x},
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = testID(t, "sess-own-")
		if _, err := tx.Exec(ctx, `INSERT INTO sessions (id, tenant_id, user_id, workspace, model)
			VALUES ($1, $2, $3, '/w', 'm')`, ids[i], r.tenant, r.owner); err != nil {
			t.Fatal(err)
		}
	}
	reserved := func() (n int) {
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id IN ('agent', 'anonymous')`).Scan(&n)
		return n
	}
	before := reserved()

	remaps, err := remapOwners(ctx, tx, accounts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		var got string
		if err := tx.QueryRow(ctx, `SELECT user_id FROM sessions WHERE id = $1`, ids[i]).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != r.want {
			t.Errorf("%q in %s became %q, want %q", r.owner, r.tenant, got, r.want)
		}
	}
	if after := reserved(); after != before {
		t.Errorf("rows owned by agent or anonymous changed: %d → %d", before, after)
	}
	var ambiguous int
	for _, r := range remaps {
		if r.Ambiguous {
			ambiguous++
		}
	}
	if ambiguous != 2 {
		t.Errorf("remaps %+v, want 2 ambiguous", remaps)
	}
}

// A migrated database opens as the runtime role; the move ran once and is
// recorded, so a second run does nothing.
func TestOwnerMigrationIsRecordedAndRunsOnce(t *testing.T) {
	p := openStore(t, "default")
	ctx := context.Background()
	done, err := ownersMigrated(ctx, p.pool)
	if err != nil || !done {
		t.Fatalf("schema version 4 not recorded after Open: %v %v", done, err)
	}
	remaps, err := migrateOwners(ctx, p.pool)
	if err != nil || remaps != nil {
		t.Fatalf("second run: %+v %v", remaps, err)
	}
}

// Plain-email owners are lowercased in every tenant, case variants of one
// address merge, and namespaced or reserved owners are left as they are.
func TestOwnerMigrationLowercasesEmailOwners(t *testing.T) {
	p := openStore(t, "default")
	ctx := context.Background()
	x := strings.ToLower(testID(t, "f"))
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows := []struct{ tenant, owner, want string }{
		{"default", "Yuvraj" + x + "@Example.COM", "yuvraj" + x + "@example.com"},
		{"default", "yuvraj" + x + "@example.com", "yuvraj" + x + "@example.com"},
		{"other" + x, "YUVRAJ" + x + "@EXAMPLE.COM", "yuvraj" + x + "@example.com"},
		{"default", "Solo" + x + "@Example.com", "solo" + x + "@example.com"},
		{"default", "oidc:Sub" + x + "@X", "oidc:Sub" + x + "@X"},
		{"default", "unclaimed:Bob" + x + "@X.com", "unclaimed:Bob" + x + "@X.com"},
		{"default", "local:bob" + x, "local:bob" + x},
		{"default", "Proxy" + x, "Proxy" + x},
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = testID(t, "sess-fold-")
		if _, err := tx.Exec(ctx, `INSERT INTO sessions (id, tenant_id, user_id, workspace, model)
			VALUES ($1, $2, $3, '/w', 'm')`, ids[i], r.tenant, r.owner); err != nil {
			t.Fatal(err)
		}
	}
	folds, err := foldEmailOwners(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		var got string
		if err := tx.QueryRow(ctx, `SELECT user_id FROM sessions WHERE id = $1`, ids[i]).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != r.want {
			t.Errorf("%q in %s became %q, want %q", r.owner, r.tenant, got, r.want)
		}
		if want := auth.FoldEmailOwner(r.owner); strings.HasPrefix(r.owner, "Y") && got != want {
			t.Errorf("the migration and FoldEmailOwner disagree on %q: %q vs %q", r.owner, got, want)
		}
	}
	byOwner := map[string]EmailFold{}
	for _, f := range folds {
		byOwner[f.To] = f
	}
	merged := byOwner["yuvraj"+x+"@example.com"]
	if len(merged.Variants) != 3 || merged.Sessions != 2 {
		t.Errorf("merged fold %+v, want 3 variants and 2 rows moved", merged)
	}
	if solo := byOwner["solo"+x+"@example.com"]; len(solo.Variants) != 1 || solo.Sessions != 1 {
		t.Errorf("single fold %+v, want 1 variant and 1 row moved", solo)
	}
}
