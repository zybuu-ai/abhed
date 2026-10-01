package store

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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

// Sign out everywhere on one server ends the user's sessions on a second
// server over the same database within the recheck, and no one else's.
func TestRevokeReachesOtherServersOverPostgres(t *testing.T) {
	a, b := runtimeStore(t, "default"), runtimeStore(t, "default")
	ctx := context.Background()
	lou, other := testID(t, "lou-"), testID(t, "by-")
	la, lb := auth.NewLocalAuth(a, time.Hour, false), auth.NewLocalAuth(b, time.Hour, false)
	for _, name := range []string{lou, other} {
		if err := la.CreateUser(ctx, auth.User{Username: name}, "correct-horse-1"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Delete(context.Background(), name) })
	}
	signIn := func(l *auth.LocalAuth, user string) *http.Request {
		t.Helper()
		rec := httptest.NewRecorder()
		l.SignInHandler(rec, httptest.NewRequest("POST", "/v1/signin",
			strings.NewReader(`{"username":"`+user+`","password":"correct-horse-1"}`)))
		req := httptest.NewRequest("GET", "/", nil)
		for _, c := range rec.Result().Cookies() {
			req.AddCookie(c)
		}
		if _, ok := l.FromCookie(req); !ok {
			t.Fatalf("sign-in as %s is not live: %d", user, rec.Code)
		}
		return req
	}
	onB, otherOnB := signIn(lb, lou), signIn(lb, other)
	stale, err := b.Get(ctx, lou)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := la.RevokeUserContext(ctx, lou); err != nil {
		t.Fatal(err)
	}
	// A write from a read taken before the revocation cannot lower its count.
	if err := b.Put(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(ctx, lou); got.Revocations != stale.Revocations+1 {
		t.Fatalf("revocations = %d, want %d", got.Revocations, stale.Revocations+1)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := lb.FromCookie(onB); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session on B survived sign-out everywhere on A")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := lb.FromCookie(otherOnB); !ok {
		t.Fatal("signing one user out everywhere ended another's session")
	}
	signIn(lb, lou)
	signIn(la, lou)
}

// Removing an account hands its sessions in its tenant to no one, as the
// runtime role, so an account made later under the same name starts empty.
func TestRemoveUserUnclaimsItsSessions(t *testing.T) {
	p := runtimeStore(t, "t-rm")
	other := runtimeStore(t, "t-rm-other")
	ctx := context.Background()
	name := strings.ToLower(testID(t, "bob"))
	if err := p.Put(ctx, &auth.User{Username: name, Tenant: "t-rm", Hash: "x"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	mk := func(s *Postgres, tenant, user string) string {
		id := testID(t, "sess-rm-")
		if err := s.CreateSession(ctx, SessionRecord{ID: id, Tenant: tenant, User: user,
			Workspace: "/w", Model: "m", Mode: "default", StartedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("create: %v", err)
		}
		return id
	}
	owner := auth.LocalOwner(name)
	mine1, mine2 := mk(p, "t-rm", owner), mk(p, "t-rm", owner)
	elsewhere := mk(other, "t-rm-other", owner)
	carol := mk(p, "t-rm", "local:carol")

	moved, err := p.RemoveUser(ctx, name)
	if err != nil || moved != 2 {
		t.Fatalf("remove: moved %d, %v", moved, err)
	}
	if _, err := p.Get(ctx, name); !errors.Is(err, auth.ErrNoSuchUser) {
		t.Fatalf("account still there: %v", err)
	}
	want := map[string]string{mine1: auth.UnclaimedOwner(owner), mine2: auth.UnclaimedOwner(owner), carol: "local:carol"}
	for id, w := range want {
		if rec, err := p.GetSession(ctx, id); err != nil || rec.User != w {
			t.Errorf("session %s: owner %q, %v; want %q", id, rec.User, err, w)
		}
	}
	if rec, err := other.GetSession(ctx, elsewhere); err != nil || rec.User != owner {
		t.Errorf("another tenant's row moved: %q, %v", rec.User, err)
	}

	// The same name again owns none of the old rows.
	if err := p.Put(ctx, &auth.User{Username: name, Tenant: "t-rm", Hash: "y"}); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	list, err := p.ListSessions(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range list {
		if rec.User == owner {
			t.Errorf("recreated %s owns %s", name, rec.ID)
		}
	}
	if _, err := p.RemoveUser(ctx, name); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RemoveUser(ctx, name); !errors.Is(err, auth.ErrNoSuchUser) {
		t.Errorf("removing a missing account: %v", err)
	}
}

// Two processes creating the same account: exactly one wins and the other is
// told why, for a duplicate username and for emails differing only in case.
func TestConcurrentCreateOneWins(t *testing.T) {
	dsn := os.Getenv("ABHED_TEST_DSN")
	if dsn == "" {
		t.Skip("set ABHED_TEST_DSN to run store integration tests")
	}
	ctx := context.Background()
	var stores [2]*Postgres
	for i := range stores {
		pg, err := Open(ctx, singleRoleConfig(dsn))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer pg.Close()
		stores[i] = pg
	}
	stamp := strings.ToLower(time.Now().Format("150405.000000"))
	cases := []struct {
		name    string
		users   [2]auth.User
		wantErr error
	}{
		{"username", [2]auth.User{
			{Username: "race-" + stamp, Tenant: "t-race", Hash: "a"},
			{Username: "RACE-" + stamp, Tenant: "t-race", Hash: "b"},
		}, auth.ErrUserExists},
		{"email", [2]auth.User{
			{Username: "twin1-" + stamp, Email: "same-" + stamp + "@example.test", Tenant: "t-race", Hash: "a"},
			{Username: "twin2-" + stamp, Email: "SAME-" + stamp + "@Example.Test", Tenant: "t-race", Hash: "b"},
		}, auth.ErrEmailTaken},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, u := range c.users {
				defer stores[0].Delete(ctx, u.Username)
			}
			var wg sync.WaitGroup
			var errs [2]error
			start := make(chan struct{})
			for i := range stores {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					u := c.users[i]
					errs[i] = stores[i].Create(ctx, &u)
				}(i)
			}
			close(start)
			wg.Wait()
			wins := 0
			for _, err := range errs {
				switch {
				case err == nil:
					wins++
				case !errors.Is(err, c.wantErr):
					t.Fatalf("loser's error = %v, want %v", err, c.wantErr)
				}
			}
			if wins != 1 {
				t.Fatalf("%d creates succeeded (%v); want exactly one", wins, errs)
			}
		})
	}
}

// Accounts that already share a username or an email, in any case, stop the
// version 5 migration with their names; without them it adds the indexes.
// Run in a transaction that is rolled back, indexes included.
func TestAccountKeysRefuseExistingClashes(t *testing.T) {
	p := openStore(t, "default")
	ctx := context.Background()
	if err := p.MigrateUsers(ctx); err != nil {
		t.Fatal(err)
	}
	x := strings.ToLower(testID(t, "k"))
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DROP INDEX IF EXISTS users_username_key; DROP INDEX IF EXISTS users_email_key`); err != nil {
		t.Fatal(err)
	}
	for _, u := range [][2]string{
		{"dupa" + x, "shared" + x + "@example.test"}, {"DUPA" + x, ""},
		{"other" + x, "SHARED" + x + "@Example.test"},
	} {
		if _, err := tx.Exec(ctx, `INSERT INTO users (username, email, hash) VALUES ($1, $2, 'x')`, u[0], u[1]); err != nil {
			t.Fatal(err)
		}
	}
	err = applyAccountKeys(ctx, tx)
	if err == nil {
		t.Fatal("clashing accounts were accepted")
	}
	for _, want := range []string{"DUPA" + x, "other" + x, "share the username", "share the email"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE username IN ($1, $2)`, "DUPA"+x, "other"+x); err != nil {
		t.Fatal(err)
	}
	if err := applyAccountKeys(ctx, tx); err != nil {
		t.Fatalf("no clashes left: %v", err)
	}
}
