package auth

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Revocation has to end the session that exists, not only prevent the next
// sign-in. A revoked user who keeps working until their cookie expires has
// not been revoked.
func TestRevokeUserEndsLiveSessions(t *testing.T) {
	l := &LocalAuth{
		sessions:   map[string]*browserSession{},
		SessionTTL: time.Hour,
		CookieName: "abhed_session",
	}
	// Two sessions for the person being revoked, one for somebody else.
	l.issue(httptest.NewRecorder(), &User{Username: "evicted", Tenant: "default"})
	l.issue(httptest.NewRecorder(), &User{Username: "evicted", Tenant: "default"})
	l.issue(httptest.NewRecorder(), &User{Username: "bystander", Tenant: "default"})

	if got := len(l.sessions); got != 3 {
		t.Fatalf("setup: expected 3 sessions, got %d", got)
	}

	if n := l.RevokeUser("evicted"); n != 2 {
		t.Errorf("RevokeUser reported %d sessions ended, want 2", n)
	}
	if got := len(l.sessions); got != 1 {
		t.Errorf("%d sessions remain, want 1 (the bystander's)", got)
	}
	for _, s := range l.sessions {
		if s.Identity.Subject != "bystander" {
			t.Errorf("revoking one user ended another's session: %q", s.Identity.Subject)
		}
	}

	// Revoking somebody with no sessions is not an error, and must not touch
	// anyone else's.
	if n := l.RevokeUser("nobody"); n != 0 {
		t.Errorf("revoking an absent user reported %d", n)
	}
	if len(l.sessions) != 1 {
		t.Error("revoking an absent user disturbed the remaining session")
	}
	if n := l.RevokeUser(""); n != 0 {
		t.Errorf("revoking the empty username reported %d — that would match every session", n)
	}
}

// node is one server's LocalAuth over an account store other servers share.
type node struct{ l *LocalAuth }

func (n node) signIn(t *testing.T, user string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	n.l.SignInHandler(rec, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"`+user+`","password":"correct-horse-1"}`)))
	for _, c := range rec.Result().Cookies() {
		if c.Name == n.l.CookieName {
			return c
		}
	}
	t.Fatalf("sign-in as %s: %d", user, rec.Code)
	return nil
}

func (n node) live(c *http.Cookie) bool {
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.AddCookie(c)
	_, ok := n.l.FromCookie(req)
	return ok
}

func twoNodes(t *testing.T, st UserStore) (node, node) {
	t.Helper()
	a, b := node{NewLocalAuth(st, time.Hour, false)}, node{NewLocalAuth(st, time.Hour, false)}
	for _, name := range []string{"lou", "bystander"} {
		if err := a.l.CreateUser(context.Background(), User{Username: name}, "correct-horse-1"); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

// Sign out everywhere on one server ends the user's sessions on another that
// shares the account store, and no one else's; signing in again still works.
func TestRevokeReachesOtherServers(t *testing.T) {
	file, err := NewFileUserStore(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]UserStore{
		"memory": NewMemoryUserStore(), "file": file,
		"unversioned": &plainStore{inner: NewMemoryUserStore()},
	}
	for name, st := range stores {
		t.Run(name, func(t *testing.T) {
			a, b := twoNodes(t, st)
			onA, onB, other := a.signIn(t, "lou"), b.signIn(t, "lou"), b.signIn(t, "bystander")
			for _, c := range []*http.Cookie{onB, other} {
				if !b.live(c) {
					t.Fatal("setup: a session on B is not live")
				}
			}
			n, err := a.l.RevokeUserContext(context.Background(), "lou")
			if err != nil || n != 1 {
				t.Fatalf("RevokeUserContext = %d, %v; want 1 session here, no error", n, err)
			}
			if a.live(onA) {
				t.Fatal("the session on A survived")
			}
			// A store without a version is re-read within accountRecheck.
			deadline := time.Now().Add(accountRecheck + time.Second)
			for b.live(onB) {
				if time.Now().After(deadline) {
					t.Fatalf("lou's session on B survived sign-out everywhere for %s", accountRecheck+time.Second)
				}
				time.Sleep(50 * time.Millisecond)
			}
			if !b.live(other) {
				t.Fatal("signing lou out everywhere ended another user's session")
			}
			for _, n := range []node{a, b} {
				if c := n.signIn(t, "lou"); !n.live(c) {
					t.Fatal("a fresh sign-in after sign-out everywhere is not live")
				}
			}
		})
	}
}

// A write from a read taken before the revocation cannot lower its count.
func TestRevocationSurvivesAStaleWrite(t *testing.T) {
	file, err := NewFileUserStore(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, st := range map[string]UserStore{"memory": NewMemoryUserStore(), "file": file} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			a, b := twoNodes(t, st)
			onB := b.signIn(t, "lou")
			stale, _ := st.Get(ctx, "lou")
			if _, err := a.l.RevokeUserContext(ctx, "lou"); err != nil {
				t.Fatal(err)
			}
			stale.Name = "Lou"
			if err := st.Put(ctx, stale); err != nil {
				t.Fatal(err)
			}
			if got, _ := st.Get(ctx, "lou"); got.Revocations != 1 {
				t.Fatalf("revocations = %d after a stale write, want 1", got.Revocations)
			}
			if b.live(onB) {
				t.Fatal("a stale write brought the revoked session back")
			}
		})
	}
}

// A store that cannot record the revocation is reported; local sessions end anyway.
func TestRevokeReportsAStoreFailure(t *testing.T) {
	st := &plainStore{inner: NewMemoryUserStore()}
	a, _ := twoNodes(t, st)
	c := a.signIn(t, "lou")
	st.fail.Store(true)
	n, err := a.l.RevokeUserContext(context.Background(), "lou")
	if err == nil || n != 1 {
		t.Fatalf("RevokeUserContext = %d, %v; want 1 and an error", n, err)
	}
	st.fail.Store(false)
	if a.live(c) {
		t.Fatal("the local session survived a failed store write")
	}
}

// gatedStore holds one Get after reading, so a read can be left in flight.
type gatedStore struct {
	*MemoryUserStore
	arm     atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *gatedStore) Get(ctx context.Context, name string) (*User, error) {
	u, err := g.MemoryUserStore.Get(ctx, name)
	if g.arm.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
	return u, err
}

// A read in flight across an administrator's reset cannot clear must-change.
func TestResetOutlastsAReadInFlight(t *testing.T) {
	st := &gatedStore{MemoryUserStore: NewMemoryUserStore(),
		entered: make(chan struct{}), release: make(chan struct{})}
	a, _ := twoNodes(t, st)
	c := a.signIn(t, "lou")
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(c)
	a.l.forget("lou")
	st.arm.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.l.FromCookie(req)
	}()
	<-st.entered
	u, _ := st.MemoryUserStore.Get(context.Background(), "lou")
	if err := a.l.CreateUserOrReset(context.Background(), u, "temporary-pw-9"); err != nil {
		t.Fatal(err)
	}
	close(st.release)
	<-done
	if !a.l.MustChangePassword(req) {
		t.Fatal("a read taken before the reset cleared must-change")
	}
}

// droppingStore loses User.Revocations on every write, against the contract.
type droppingStore struct{ plainStore }

func (d *droppingStore) Put(ctx context.Context, u *User) error {
	c := *u
	c.Revocations = 0
	return d.inner.Put(ctx, &c)
}

// A store that does not keep the count is reported, not taken as success.
func TestRevokeCatchesAStoreThatDropsTheCount(t *testing.T) {
	st := &droppingStore{plainStore{inner: NewMemoryUserStore()}}
	a, _ := twoNodes(t, st)
	if _, err := a.l.RevokeUserContext(context.Background(), "lou"); err == nil {
		t.Fatal("a store that dropped the revocation count was reported as success")
	}
}

// Revoking a removed account is not a failure: no session anywhere can read it.
func TestRevokeAfterRemovalIsNotAnError(t *testing.T) {
	for name, st := range map[string]UserStore{
		"memory": NewMemoryUserStore(), "unversioned": &plainStore{inner: NewMemoryUserStore()},
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := twoNodes(t, st)
			c := a.signIn(t, "lou")
			if err := st.Delete(context.Background(), "lou"); err != nil {
				t.Fatal(err)
			}
			n, err := a.l.RevokeUserContext(context.Background(), "lou")
			if err != nil || n != 1 {
				t.Fatalf("RevokeUserContext after removal = %d, %v; want 1, no error", n, err)
			}
			if a.live(c) {
				t.Fatal("the removed account's session survived")
			}
		})
	}
}

// RevokeUser logs a sign-out it could not record, since it cannot return it.
func TestRevokeUserLogsAFailure(t *testing.T) {
	st := &plainStore{inner: NewMemoryUserStore()}
	a, _ := twoNodes(t, st)
	var buf bytes.Buffer
	a.l.Log = slog.New(slog.NewTextHandler(&buf, nil))
	a.signIn(t, "lou")
	st.fail.Store(true)
	if n := a.l.RevokeUser("lou"); n != 1 {
		t.Fatalf("RevokeUser = %d, want 1", n)
	}
	out := buf.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "user=lou") ||
		!strings.Contains(out, "database is down") {
		t.Fatalf("log = %q; want an error naming lou and the cause", out)
	}
}
