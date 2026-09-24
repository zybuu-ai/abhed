package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// adminRig is a local-accounts server with alice, an administrator whose
// account has an email address, and bob.
type adminRig struct {
	h     http.Handler
	local *auth.LocalAuth
}

func newAdminRig(t *testing.T) *adminRig {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	for _, u := range []auth.User{
		{Username: "alice", Email: "alice@example.com", Groups: []string{DefaultAdminGroup}},
		{Username: "bob", Email: "bob@example.com"},
	} {
		if err := local.CreateUser(context.Background(), u, "correct-horse-1"); err != nil {
			t.Fatal(err)
		}
	}
	h := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}),
		Auth: &auth.Middleware{Providers: []auth.Provider{local},
			PublicPaths: append(PublicPaths(), local.PublicPaths()...)}}).Handler()
	return &adminRig{h: h, local: local}
}

func (g *adminRig) signIn(t *testing.T, user string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"`+user+`","password":"correct-horse-1"}`)))
	for _, c := range rec.Result().Cookies() {
		if c.Name == "abhed_session" {
			return c
		}
	}
	t.Fatalf("sign-in as %s: %d %s", user, rec.Code, rec.Body)
	return nil
}

// setAdmin asks, as the holder of c, for username's admin rights to be set.
func (g *adminRig) setAdmin(c *http.Cookie, username string, admin bool) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"username":%q,"admin":%t}`, username, admin)
	req := httptest.NewRequest("POST", "/v1/admin/users/admin", strings.NewReader(body))
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, req)
	return rec
}

func (g *adminRig) isAdmin(t *testing.T, name string) bool {
	t.Helper()
	u, err := g.local.Store.Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(u.Groups, DefaultAdminGroup)
}

// UserOf is the email when an account has one, so comparing it to a username
// let an administrator with an email demote themselves.
func TestAdminWithEmailCannotDemoteThemselves(t *testing.T) {
	g := newAdminRig(t)
	alice := g.signIn(t, "alice")
	// Another administrator, so only the self-demotion rule can refuse.
	if rec := g.setAdmin(alice, "bob", true); rec.Code != http.StatusNoContent {
		t.Fatalf("promote bob = %d %s", rec.Code, rec.Body)
	}
	for _, name := range []string{"alice", "ALICE"} {
		rec := g.setAdmin(alice, name, false)
		if rec.Code != http.StatusConflict {
			t.Fatalf("self-demotion as %q = %d, want 409", name, rec.Code)
		}
	}
	if !g.isAdmin(t, "alice") {
		t.Fatal("alice demoted herself")
	}
}

func TestLastAdministratorCannotBeRemoved(t *testing.T) {
	g := newAdminRig(t)
	alice := g.signIn(t, "alice")
	if rec := g.setAdmin(alice, "bob", true); rec.Code != http.StatusNoContent {
		t.Fatalf("promote bob = %d", rec.Code)
	}
	// bob signs in after the promotion, so his session carries the group.
	bob := g.signIn(t, "bob")
	if rec := g.setAdmin(bob, "alice", false); rec.Code != http.StatusNoContent {
		t.Fatalf("bob demotes alice = %d %s", rec.Code, rec.Body)
	}
	// alice's old session still carries the group; bob is now the only admin.
	rec := g.setAdmin(alice, "bob", false)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "last administrator") {
		t.Fatalf("removing the last admin = %d %s, want 409", rec.Code, rec.Body)
	}
	if !g.isAdmin(t, "bob") {
		t.Fatal("the last administrator was removed")
	}
}

// Two administrators removing each other at once: exactly one succeeds.
func TestConcurrentDemotionsLeaveAnAdministrator(t *testing.T) {
	for range 5 {
		g := newAdminRig(t)
		alice := g.signIn(t, "alice")
		if rec := g.setAdmin(alice, "bob", true); rec.Code != http.StatusNoContent {
			t.Fatalf("promote bob = %d", rec.Code)
		}
		bob := g.signIn(t, "bob")
		codes := make(chan int, 2)
		var start sync.WaitGroup
		start.Add(1)
		for _, req := range []struct {
			c      *http.Cookie
			target string
		}{{alice, "bob"}, {bob, "alice"}} {
			go func() {
				start.Wait()
				codes <- g.setAdmin(req.c, req.target, false).Code
			}()
		}
		start.Done()
		a, b := <-codes, <-codes
		if a+b != http.StatusNoContent+http.StatusConflict {
			t.Fatalf("results %d and %d, want one 204 and one 409", a, b)
		}
		if g.isAdmin(t, "alice") == g.isAdmin(t, "bob") {
			t.Fatal("not exactly one administrator left")
		}
	}
}
