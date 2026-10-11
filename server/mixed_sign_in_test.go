package server

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// ssoProvider holds one single sign-on session, by the cookie sso=held, and
// records being asked to sign out.
type ssoProvider struct {
	signedOut *atomic.Bool
}

func (ssoProvider) Name() string { return "oidc" }
func (ssoProvider) Identify(r *http.Request) (*auth.Identity, bool) {
	if c, err := r.Cookie("sso"); err == nil && c.Value == "held" {
		return &auth.Identity{Subject: "dana", Tenant: "default", Provider: "oidc"}, true
	}
	return nil, false
}
func (ssoProvider) Routes(*http.ServeMux)    {}
func (ssoProvider) PublicPaths() []string    { return nil }
func (ssoProvider) SignIn() (string, string) { return "/auth/login", "Company sign-in" }
func (p ssoProvider) SignOut(w http.ResponseWriter, r *http.Request) {
	p.signedOut.Store(true)
	http.Redirect(w, r, "https://idp.example/logout", http.StatusFound)
}

// newMixedRig is a server with single sign-on listed first and local accounts
// beside it, over an account store that can be taken down.
func newMixedRig(t *testing.T) (*gateRig, *outageStore, *atomic.Bool) {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	st := &outageStore{MemoryUserStore: auth.NewMemoryUserStore()}
	local := auth.NewLocalAuth(st, time.Hour, false)
	if err := local.CreateUser(context.Background(), auth.User{Username: "alice"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	out := &atomic.Bool{}
	h := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}),
		Auth: &auth.Middleware{Providers: []auth.Provider{ssoProvider{signedOut: out}, local},
			PublicPaths: append(PublicPaths(), local.PublicPaths()...)}}).Handler()
	return &gateRig{h: h, local: local}, st, out
}

// A browser holding both a local and a single sign-on session is signed out
// of both: the local one ends, then the provider's own sign-out runs.
func TestSignOutEndsBothSessions(t *testing.T) {
	g, _, out := newMixedRig(t)
	alice := g.signIn(t, "alice")
	req := g.do(alice, "POST", "/logout", "", "Cookie", "sso=held; abhed_session="+alice.Value)
	if req.Code != http.StatusFound || req.Header().Get("Location") != "https://idp.example/logout" {
		t.Fatalf("sign-out = %d %q, want the provider's sign-out", req.Code, req.Header().Get("Location"))
	}
	if !out.Load() {
		t.Fatal("the single sign-on session was not signed out")
	}
	if rec := g.do(alice, "GET", "/v1/sessions", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the local session after sign-out = %d, want 401", rec.Code)
	}
	// A reload afterwards lands on the front door, not at the provider.
	nav := g.do(nil, "GET", "/console", "", "Accept", "text/html")
	if loc := nav.Header().Get("Location"); !strings.HasPrefix(loc, "/?return=") {
		t.Fatalf("a signed-out reload went to %q", loc)
	}
}
