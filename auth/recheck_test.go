package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fileRig is a server's LocalAuth over a users file, and a second store over
// the same file standing in for `abhed user` run while the server is up.
type fileRig struct {
	local *LocalAuth
	cli   *FileUserStore
}

func newFileRig(t *testing.T) *fileRig {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".abhed", "users.json")
	server, err := NewFileUserStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := NewFileUserStore(path)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLocalAuth(server, time.Hour, false)
	for _, u := range []User{{Username: "admin1", Groups: []string{"abhed-admin"}}, {Username: "dave"}} {
		if err := l.CreateUser(context.Background(), u, "correct-horse-1"); err != nil {
			t.Fatal(err)
		}
	}
	return &fileRig{local: l, cli: cli}
}

func (g *fileRig) signIn(t *testing.T, user string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	g.local.SignInHandler(rec, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"`+user+`","password":"correct-horse-1"}`)))
	for _, c := range rec.Result().Cookies() {
		if c.Name == g.local.CookieName {
			return c
		}
	}
	t.Fatalf("sign-in as %s: %d", user, rec.Code)
	return nil
}

func (g *fileRig) who(c *http.Cookie) (*Identity, bool) {
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.AddCookie(c)
	return g.local.FromCookie(req)
}

// An account removed by another process loses its live sessions on their
// next request, an administrator's included.
func TestRemovedAccountSessionEnds(t *testing.T) {
	g := newFileRig(t)
	ctx := context.Background()
	for _, name := range []string{"dave", "admin1"} {
		c := g.signIn(t, name)
		if _, ok := g.who(c); !ok {
			t.Fatalf("%s is not signed in", name)
		}
		if err := g.cli.Delete(ctx, name); err != nil {
			t.Fatal(err)
		}
		if id, ok := g.who(c); ok {
			t.Fatalf("%s's session survived the account's removal: %+v", name, id)
		}
	}
	if n := len(g.local.Sessions()); n != 0 {
		t.Fatalf("%d sessions left after their accounts were removed", n)
	}
}

// Groups changed by another process apply on the next request, both ways.
func TestGroupChangeReachesTheLiveSession(t *testing.T) {
	g := newFileRig(t)
	ctx := context.Background()
	c := g.signIn(t, "admin1")
	u, _ := g.cli.Get(ctx, "admin1")
	u.Groups = nil
	if err := g.cli.Put(ctx, u); err != nil {
		t.Fatal(err)
	}
	if id, ok := g.who(c); !ok || slices.Contains(id.Groups, "abhed-admin") {
		t.Fatalf("after demotion the session = %+v, %v; want signed in without the group", id, ok)
	}
	u.Groups = []string{"abhed-admin"}
	if err := g.cli.Put(ctx, u); err != nil {
		t.Fatal(err)
	}
	if id, ok := g.who(c); !ok || !slices.Contains(id.Groups, "abhed-admin") {
		t.Fatalf("after promotion the session = %+v, %v; want the group", id, ok)
	}
}

// A password reset by another process confines the live session at once.
func TestResetElsewhereConfinesTheSession(t *testing.T) {
	g := newFileRig(t)
	ctx := context.Background()
	c := g.signIn(t, "dave")
	// Read once first, so the reset has to displace a cached reading.
	if _, ok := g.who(c); !ok {
		t.Fatal("dave is not signed in")
	}
	u, _ := g.cli.Get(ctx, "dave")
	other := NewLocalAuth(g.cli, time.Hour, false)
	if err := other.CreateUserOrReset(ctx, u, "temporary-pw-9"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.AddCookie(c)
	if _, ok := g.local.FromCookie(req); !ok {
		t.Fatal("the session ended; a reset should confine it, not end it")
	}
	if !g.local.MustChangePassword(req) {
		t.Fatal("a reset made by another process did not confine the live session")
	}
}

// A store that cannot answer refuses the request but keeps the session.
func TestUnreadableStoreRefusesWithoutEnding(t *testing.T) {
	st := &flakyStore{MemoryUserStore: NewMemoryUserStore()}
	l := NewLocalAuth(st, time.Hour, false)
	if err := l.CreateUser(context.Background(), User{Username: "erin"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	l.SignInHandler(rec, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"erin","password":"correct-horse-1"}`)))
	req := httptest.NewRequest("GET", "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	st.fail = true
	st.gen.Add(1)
	if _, ok := l.FromCookie(req); ok {
		t.Fatal("a session was admitted while its account could not be read")
	}
	st.fail = false
	if _, ok := l.FromCookie(req); !ok {
		t.Fatal("a store outage ended the session")
	}
}

type flakyStore struct {
	*MemoryUserStore
	fail bool
}

func (f *flakyStore) Get(ctx context.Context, name string) (*User, error) {
	if f.fail {
		return nil, errors.New("database is down")
	}
	return f.MemoryUserStore.Get(ctx, name)
}

// Removing an account that does not exist says so.
func TestDeleteUnknownAccount(t *testing.T) {
	file, err := NewFileUserStore(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, st := range map[string]UserStore{"memory": NewMemoryUserStore(), "file": file} {
		if err := st.Delete(context.Background(), "nosuch"); !errors.Is(err, ErrNoSuchUser) {
			t.Errorf("%s: Delete(unknown) = %v, want ErrNoSuchUser", name, err)
		}
	}
}

// plainStore has no Version, as the Postgres store has none.
type plainStore struct {
	inner *MemoryUserStore
	fail  atomic.Bool
}

func (p *plainStore) Get(ctx context.Context, name string) (*User, error) {
	if p.fail.Load() {
		return nil, errors.New("database is down")
	}
	return p.inner.Get(ctx, name)
}
func (p *plainStore) Put(ctx context.Context, u *User) error { return p.inner.Put(ctx, u) }
func (p *plainStore) List(ctx context.Context) ([]*User, error) {
	return p.inner.List(ctx)
}
func (p *plainStore) Delete(ctx context.Context, name string) error { return p.inner.Delete(ctx, name) }

// Without a version, a change made elsewhere arrives within accountRecheck, one
// made through this process at once, and an outage refuses without ending.
func TestUnversionedStoreRechecks(t *testing.T) {
	st := &plainStore{inner: NewMemoryUserStore()}
	l := NewLocalAuth(st, time.Hour, false)
	ctx := context.Background()
	if err := l.CreateUser(ctx, User{Username: "erin"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	l.SignInHandler(rec, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"erin","password":"correct-horse-1"}`)))
	req := httptest.NewRequest("GET", "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	groups := func() []string {
		t.Helper()
		id, ok := l.FromCookie(req)
		if !ok {
			t.Fatal("erin is not signed in")
		}
		return id.Groups
	}
	groups()

	// In this process: SetGroups reaches the session on its next request.
	if err := l.SetGroups(ctx, "erin", "abhed-admin", true); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(groups(), "abhed-admin") {
		t.Fatal("SetGroups did not reach the live session on its next request")
	}

	// Elsewhere, as another node writes: seen once accountRecheck has passed.
	u, _ := st.inner.Get(ctx, "erin")
	u.Groups = nil
	if err := st.inner.Put(ctx, u); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(accountRecheck + time.Second)
	for slices.Contains(groups(), "abhed-admin") {
		if time.Now().After(deadline) {
			t.Fatalf("a change made elsewhere was not seen within %s", accountRecheck)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// An outage refuses the request, says so, and keeps the session.
	st.fail.Store(true)
	l.forget("erin")
	if _, ok := l.FromCookie(req); ok {
		t.Fatal("a session was admitted while its account could not be read")
	}
	if err := l.Verify(req); !errors.Is(err, ErrAccountUnchecked) {
		t.Fatalf("Verify during an outage = %v, want ErrAccountUnchecked", err)
	}
	st.fail.Store(false)
	if _, ok := l.FromCookie(req); !ok {
		t.Fatal("a store outage ended the session")
	}
}
