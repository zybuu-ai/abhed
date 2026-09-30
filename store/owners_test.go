package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zybuu-ai/abhed/auth"
)

// The version 4 move, run inside a transaction that is rolled back, so the
// shared test database keeps none of it. The same rows go through both
// policies: local-only moves a key one account in the row's tenant holds,
// unclaim moves none, and neither crosses a tenant.
func TestOwnerMigrationMovesOnlyCertainRows(t *testing.T) {
	x := strings.ToLower(testID(t, "o"))
	other := "other" + x
	accounts := []*auth.User{
		{Username: "bob" + x},
		{Username: "mallet" + x, Email: "bob" + x},
		{Username: "carol" + x, Email: "carol" + x + "@example.test"},
		{Username: "dave" + x, Email: "Dave" + x + "@Example.test"},
		{Username: "dup1" + x, Email: "shared" + x + "@example.test"},
		{Username: "dup2" + x, Email: "SHARED" + x + "@example.test"},
		{Username: "founder" + x, Email: "founder" + x + "@example.test"},
		{Username: "olga" + x, Email: "olga" + x + "@example.test", Tenant: other},
		{Username: "agent"},
		{Username: "anonymous"},
	}
	type row struct{ tenant, owner, localOnly, unclaim string }
	u := func(k string) string { return UnclaimedPrefix + k }
	rows := []row{
		// Two accounts hold the key: unclaimed under both policies.
		{"default", "bob" + x, u("bob" + x), u("bob" + x)},
		{"default", "shared" + x + "@example.test", u("shared" + x + "@example.test"), u("shared" + x + "@example.test")},
		// One account holds it: moved only when local accounts were the only way in.
		{"default", "carol" + x + "@example.test", "local:carol" + x, u("carol" + x + "@example.test")},
		{"default", "carol" + x, "local:carol" + x, u("carol" + x)},
		{"default", "dave" + x + "@example.test", "local:dave" + x, u("dave" + x + "@example.test")},
		{other, "olga" + x + "@example.test", "local:olga" + x, u("olga" + x + "@example.test")},
		// An account in another tenant never takes a row.
		{other, "founder" + x + "@example.test", "founder" + x + "@example.test", "founder" + x + "@example.test"},
		{"default", "olga" + x + "@example.test", "olga" + x + "@example.test", "olga" + x + "@example.test"},
		// Rows no account names: single sign-on and proxy principals, kept.
		{"default", "stranger" + x + "@example.test", "stranger" + x + "@example.test", "stranger" + x + "@example.test"},
		{"default", "proxyuser" + x, "proxyuser" + x, "proxyuser" + x},
		{"default", "oidc:" + x, "oidc:" + x, "oidc:" + x},
	}
	for _, policy := range []OwnerPolicy{OwnersLocalOnly, OwnersUnclaim} {
		t.Run(string(policy), func(t *testing.T) {
			p := openStore(t, "default")
			ctx := context.Background()
			tx, err := p.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
				t.Fatal(err)
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
			remaps, err := remapOwners(ctx, tx, accounts, policy)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
				t.Fatal(err)
			}
			for i, r := range rows {
				want := r.localOnly
				if policy == OwnersUnclaim {
					want = r.unclaim
				}
				var got string
				if err := tx.QueryRow(ctx, `SELECT user_id FROM sessions WHERE id = $1`, ids[i]).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Errorf("%q in %s became %q, want %q", r.owner, r.tenant, got, want)
				}
			}
			if after := reserved(); after != before {
				t.Errorf("rows owned by agent or anonymous changed: %d → %d", before, after)
			}
			for _, r := range remaps {
				if r.Ambiguous != strings.HasPrefix(r.To, UnclaimedPrefix) {
					t.Errorf("remap %+v: Ambiguous disagrees with its target", r)
				}
			}
		})
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
	remaps, err := migrateOwners(ctx, p.pool, OwnersLocalOnly)
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
		{"default", "Alice" + x + "@Example.COM", "alice" + x + "@example.com"},
		{"default", "alice" + x + "@example.com", "alice" + x + "@example.com"},
		{"other" + x, "ALICE" + x + "@EXAMPLE.COM", "alice" + x + "@example.com"},
		{"default", "Solo" + x + "@Example.com", "solo" + x + "@example.com"},
		{"default", "oidc:Sub" + x + "@X", "oidc:Sub" + x + "@X"},
		{"default", "unclaimed:Bob" + x + "@X.com", "unclaimed:Bob" + x + "@X.com"},
		{"default", "local:bob" + x, "local:bob" + x},
		{"default", "Proxy" + x, "Proxy" + x},
		// Titlecase letters fold differently in Go and in Postgres lower().
		{"default", "ǅ" + x + "@example.com", "ǆ" + x + "@example.com"},
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
		if want := auth.FoldEmailOwner(r.owner); got != want {
			t.Errorf("the migration and FoldEmailOwner disagree on %q: %q vs %q", r.owner, got, want)
		}
	}
	byOwner := map[string]EmailFold{}
	for _, f := range folds {
		byOwner[f.To] = f
	}
	merged := byOwner["alice"+x+"@example.com"]
	if len(merged.Variants) != 3 || merged.Sessions != 2 {
		t.Errorf("merged fold %+v, want 3 variants and 2 rows moved", merged)
	}
	if solo := byOwner["solo"+x+"@example.com"]; len(solo.Variants) != 1 || solo.Sessions != 1 {
		t.Errorf("single fold %+v, want 1 variant and 1 row moved", solo)
	}
}

// A server running as the runtime role refuses a database the owner
// migration has not run on.
func TestOpenRefusesAnUnmigratedOwnerSchema(t *testing.T) {
	runtimeStore(t, "default") // provisioned, so only the check below can refuse
	runtime := os.Getenv("ABHED_TEST_RUNTIME_DSN")
	saved := checkOwnersMigrated
	checkOwnersMigrated = func(context.Context, *pgxpool.Pool) (bool, error) { return false, nil }
	defer func() { checkOwnersMigrated = saved }()
	p, err := Open(context.Background(), DefaultConfig(runtime))
	if err == nil {
		p.Close()
		t.Fatal("the runtime role opened a database without schema version 4")
	}
	if !errors.Is(err, errOwnersNotMigrated) {
		t.Fatalf("refused for another reason: %v", err)
	}
}

func TestParseOwnerPolicy(t *testing.T) {
	for _, ok := range []string{"local-only", "unclaim"} {
		if _, err := ParseOwnerPolicy(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	if _, err := ParseOwnerPolicy("move"); err == nil {
		t.Error("an unknown policy was accepted")
	}
}
