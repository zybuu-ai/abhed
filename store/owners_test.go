package store

import (
	"context"
	"errors"
	"os"
	"strconv"
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
	remaps, err := migrateOwners(ctx, p.pool, OwnerMigration{Policy: OwnersLocalOnly})
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

// Accounts from a users file join the table's. One the table also holds
// keeps its file email as a key in the table's tenant, and is dropped when
// the file puts it in another tenant.
func TestMergeAccounts(t *testing.T) {
	table := []*auth.User{{Username: "bob", Email: "bob@table.test"}}
	file := []*auth.User{{Username: "BOB", Email: "bob@file.test"}, {Username: "Bob", Tenant: "t9"},
		{Username: "ann", Tenant: "t2"}, nil, {}}
	got := mergeAccounts(table, file)
	if len(got) != 3 || got[0].Email != "bob@table.test" || got[1].Email != "bob@file.test" || got[2].Username != "ann" {
		t.Fatalf("merged %+v", got)
	}
	if n := distinctUsernames(got); n != 2 {
		t.Fatalf("%d distinct, want 2", n)
	}
	if len(mergeAccounts(nil, nil)) != 0 {
		t.Fatal("nothing merged into something")
	}
}

// The whole move, with accounts that exist only in a users file: their rows
// go to them, so the file's accounts reach the remap.
func TestOwnerMigrationUsesFileAccounts(t *testing.T) {
	p := openStore(t, "default")
	ctx := context.Background()
	x := strings.ToLower(testID(t, "u"))
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	rows := map[string][2]string{ // id → owner before, owner after
		testID(t, "sess-uf-"): {"founder" + x + "@example.test", "local:founder" + x},
		testID(t, "sess-uf-"): {"founder" + x, "local:founder" + x},
	}
	for id, r := range rows {
		if _, err := tx.Exec(ctx, `INSERT INTO sessions (id, tenant_id, user_id, workspace, model)
			VALUES ($1, 'default', $2, '/w', 'm')`, id, r[0]); err != nil {
			t.Fatal(err)
		}
	}
	var found int
	m := OwnerMigration{Policy: OwnersLocalOnly,
		Accounts: []*auth.User{{Username: "founder" + x, Email: "founder" + x + "@example.test"}},
		Found:    func(_, extra, _ int) { found = extra }}
	if _, _, err := runOwnerMigration(ctx, tx, m, OwnersLocalOnly); err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Errorf("reported %d file accounts, want 1", found)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	for id, r := range rows {
		var got string
		if err := tx.QueryRow(ctx, `SELECT user_id FROM sessions WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != r[1] {
			t.Errorf("%q became %q, want %q", r[0], got, r[1])
		}
	}
}

// A local-only move with no accounts refuses while old rows exist, unless
// forced; with no old rows it has nothing to strand.
func TestNoAccountsRefusesToStrandSessions(t *testing.T) {
	if err := noAccounts(3, false); !errors.Is(err, ErrNoOwnerAccounts) {
		t.Fatalf("3 stranded rows: %v", err)
	}
	if err := noAccounts(3, true); err != nil {
		t.Fatalf("forced: %v", err)
	}
	if err := noAccounts(0, false); err != nil {
		t.Fatalf("a fresh database: %v", err)
	}
}

// The count behind the refusal sees every tenant and skips owners no account
// ever held.
func TestLegacyOwnedRowsCountsEveryTenant(t *testing.T) {
	p := openStore(t, "default")
	ctx := context.Background()
	x := strings.ToLower(testID(t, "l"))
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before, err := legacyOwnedRows(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	for i, r := range [][2]string{{"default", "bob" + x}, {"t2" + x, "ann" + x + "@x.test"},
		{"default", "anonymous"}, {"default", "agent"}, {"default", "local:bob" + x}, {"default", "oidc:" + x}} {
		if _, err := tx.Exec(ctx, `INSERT INTO sessions (id, tenant_id, user_id, workspace, model)
			VALUES ($1, $2, $3, '/w', 'm')`, testID(t, "sess-leg-")+strconv.Itoa(i), r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	after, err := legacyOwnedRows(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if after-before != 2 {
		t.Fatalf("counted %d new legacy rows, want 2", after-before)
	}
}

// A single-role start's accounts outside the table are read when the move
// runs, and a failure to read them stops it.
func TestOwnerMigrationLoadsAccountsLate(t *testing.T) {
	p := openStore(t, "default")
	ctx := context.Background()
	x := strings.ToLower(testID(t, "s"))
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	id := testID(t, "sess-late-")
	if _, err := tx.Exec(ctx, `INSERT INTO sessions (id, tenant_id, user_id, workspace, model)
		VALUES ($1, 'default', $2, '/w', 'm')`, id, "solo"+x+"@example.test"); err != nil {
		t.Fatal(err)
	}
	m := OwnerMigration{Policy: OwnersLocalOnly, AllowNoAccounts: true, LoadAccounts: func() ([]*auth.User, error) {
		return []*auth.User{{Username: "solo" + x, Email: "solo" + x + "@example.test"}}, nil
	}}
	if _, _, err := runOwnerMigration(ctx, tx, m, OwnersLocalOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := tx.QueryRow(ctx, `SELECT user_id FROM sessions WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "local:solo"+x {
		t.Fatalf("owner %q, want local:solo%s", got, x)
	}
	m.LoadAccounts = func() ([]*auth.User, error) { return nil, errors.New("users.json: no such file") }
	if _, _, err := runOwnerMigration(ctx, tx, m, OwnersLocalOnly); err == nil {
		t.Fatal("a failed account read did not stop the move")
	}
}
