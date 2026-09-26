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
	bob := g.signIn(t, "bob")
	if rec := g.setAdmin(bob, "alice", false); rec.Code != http.StatusNoContent {
		t.Fatalf("bob demotes alice = %d %s", rec.Code, rec.Body)
	}
	// Demotion ends alice's sessions here; an identity a front router still
	// holds for her, from before, is what can still ask.
	body := `{"username":"bob","admin":false}`
	req := httptest.NewRequest("POST", "/v1/admin/users/admin", strings.NewReader(body))
	req = req.WithContext(auth.WithIdentity(req.Context(),
		&auth.Identity{Subject: "alice", Tenant: "default", Groups: []string{DefaultAdminGroup}}))
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "last administrator") {
		t.Fatalf("removing the last admin = %d %s, want 409", rec.Code, rec.Body)
	}
	if !g.isAdmin(t, "bob") {
		t.Fatal("the last administrator was removed")
	}
}

// do sends a request as the holder of c.
func (g *adminRig) do(c *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, req)
	return rec
}

// Removing administrator rights through the API ends the person's sessions:
// the old cookie can no longer re-grant itself, list users or add an MCP server.
func TestDemotionEndsTheAdministratorsSession(t *testing.T) {
	g := newAdminRig(t)
	alice := g.signIn(t, "alice")
	if rec := g.setAdmin(alice, "bob", true); rec.Code != http.StatusNoContent {
		t.Fatalf("promote bob = %d", rec.Code)
	}
	bob := g.signIn(t, "bob")
	if rec := g.do(bob, "GET", "/v1/admin/users", ""); rec.Code != http.StatusOK {
		t.Fatalf("bob as an administrator = %d", rec.Code)
	}
	if rec := g.setAdmin(alice, "bob", false); rec.Code != http.StatusNoContent {
		t.Fatalf("demote bob = %d %s", rec.Code, rec.Body)
	}
	for _, si := range g.local.Sessions() {
		if si.Subject == "bob" {
			t.Fatal("the demoted administrator still has a session")
		}
	}
	// 401, not 403: the session is gone, not merely re-read without the group.
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/admin/users/admin", `{"username":"bob","admin":true}`},
		{"POST", "/v1/admin/mcp", `{"name":"x","command":"true"}`},
		{"GET", "/v1/admin/users", ""},
	} {
		if rec := g.do(bob, c.method, c.path, c.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with the demoted session = %d, want 401", c.method, c.path, rec.Code)
		}
	}
	if g.isAdmin(t, "bob") {
		t.Fatal("the demoted session granted itself administrator rights again")
	}
}

// A group change made outside this process, as the CLI makes it, applies to a
// live session on its next request, both ways.
func TestGroupChangesApplyOnTheNextRequest(t *testing.T) {
	g := newAdminRig(t)
	ctx := context.Background()
	bob := g.signIn(t, "bob")
	if rec := g.do(bob, "GET", "/v1/admin/users", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("bob before = %d, want 403", rec.Code)
	}
	set := func(groups []string) {
		u, err := g.local.Store.Get(ctx, "bob")
		if err != nil {
			t.Fatal(err)
		}
		u.Groups = groups
		if err := g.local.Store.Put(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	set([]string{DefaultAdminGroup})
	if rec := g.do(bob, "GET", "/v1/admin/users", ""); rec.Code != http.StatusOK {
		t.Fatalf("bob after a grant = %d, want 200", rec.Code)
	}
	set(nil)
	if rec := g.do(bob, "POST", "/v1/admin/mcp", `{"name":"x","command":"true"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("bob after the grant was taken back = %d, want 403", rec.Code)
	}
}

// An account removed from the store, as `abhed user remove` removes it, has no
// session left on its next request.
func TestRemovedAccountLosesItsSession(t *testing.T) {
	g := newAdminRig(t)
	alice := g.signIn(t, "alice")
	if rec := g.do(alice, "GET", "/v1/sessions", ""); rec.Code != http.StatusOK {
		t.Fatalf("alice before = %d", rec.Code)
	}
	if err := g.local.Store.Delete(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	if rec := g.do(alice, "GET", "/v1/sessions", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a removed account's session = %d, want 401", rec.Code)
	}
	if n := len(g.local.Sessions()); n != 0 {
		t.Fatalf("%d sessions left for a removed account", n)
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
		// The loser is refused as the last administrator, or, when the
		// winner's demotion landed first, as no longer signed in.
		if a != http.StatusNoContent {
			a, b = b, a
		}
		if a != http.StatusNoContent || (b != http.StatusConflict && b != http.StatusUnauthorized && b != http.StatusForbidden) {
			t.Fatalf("results %d and %d, want one 204 and one refusal", a, b)
		}
		if g.isAdmin(t, "alice") == g.isAdmin(t, "bob") {
			t.Fatal("not exactly one administrator left")
		}
	}
}
