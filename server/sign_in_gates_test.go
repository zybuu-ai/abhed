package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// gateRig is a local-accounts server with alice, a member of abhed-users,
// and bob, who is not.
type gateRig struct {
	h       http.Handler
	local   *auth.LocalAuth
	invites *oneUseInvites
}

func newGateRig(t *testing.T, requireGroup string) *gateRig {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	cfg.Auth.RequireGroup = requireGroup
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	for _, u := range []auth.User{
		{Username: "alice", Groups: []string{"abhed-users"}},
		{Username: "bob"},
	} {
		if err := local.CreateUser(context.Background(), u, "correct-horse-1"); err != nil {
			t.Fatal(err)
		}
	}
	inv := &oneUseInvites{codes: map[string]string{"code-1": ""}}
	h := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Invites: inv,
		Auth: &auth.Middleware{Providers: []auth.Provider{local},
			PublicPaths: append(PublicPaths(), local.PublicPaths()...)}}).Handler()
	return &gateRig{h: h, local: local, invites: inv}
}

func (g *gateRig) do(c *http.Cookie, method, path, body string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if c != nil {
		req.AddCookie(c)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, req)
	return rec
}

func (g *gateRig) signIn(t *testing.T, user string) *http.Cookie {
	t.Helper()
	rec := g.do(nil, "POST", "/v1/signin", `{"username":"`+user+`","password":"correct-horse-1"}`)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "abhed_session" {
			return c
		}
	}
	t.Fatalf("sign-in as %s: %d %s", user, rec.Code, rec.Body)
	return nil
}

// auth.require_group admits its members and turns away everyone else once
// they are known, without closing the paths needed to sign in.
func TestRequireGroupGatesAfterSignIn(t *testing.T) {
	g := newGateRig(t, "abhed-users")
	for _, p := range []string{"/v1/health", "/", "/v1/whoami", "/login", "/logout"} {
		if rec := g.do(nil, "GET", p, ""); rec.Code >= 400 {
			t.Errorf("GET %s before sign-in = %d", p, rec.Code)
		}
	}
	alice := g.signIn(t, "alice")
	if rec := g.do(alice, "GET", "/v1/sessions", ""); rec.Code != http.StatusOK {
		t.Fatalf("a member = %d %s", rec.Code, rec.Body)
	}
	if rec := g.do(alice, "GET", "/ide", "", "Accept", "text/html"); rec.Code != http.StatusOK {
		t.Fatalf("a member's workbench = %d", rec.Code)
	}

	// A non-member's API call is refused with the reason, and the session ends.
	bob := g.signIn(t, "bob")
	rec := g.do(bob, "GET", "/v1/sessions", "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not in the abhed-users group") {
		t.Fatalf("a non-member = %d %s, want 403 naming the group", rec.Code, rec.Body)
	}
	if rec := g.do(bob, "GET", "/v1/sessions", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a refused session afterwards = %d, want 401", rec.Code)
	}

	// A non-member's browser lands on the front door, told why.
	bob = g.signIn(t, "bob")
	nav := g.do(bob, "GET", "/ide", "", "Accept", "text/html")
	loc, _ := url.Parse(nav.Header().Get("Location"))
	if nav.Code != http.StatusFound || loc == nil || loc.Path != "/" ||
		!strings.Contains(loc.Query().Get("refused"), "abhed-users") {
		t.Fatalf("a non-member's navigation = %d %q", nav.Code, nav.Header().Get("Location"))
	}

	// whoami says so too, rather than naming a session nothing will serve.
	bob = g.signIn(t, "bob")
	var me map[string]any
	_ = json.Unmarshal(g.do(bob, "GET", "/v1/whoami", "").Body.Bytes(), &me)
	if me["authenticated"] != false || !strings.Contains(me["reason"].(string), "abhed-users") {
		t.Fatalf("whoami for a non-member = %v", me)
	}
	if rec := g.do(nil, "GET", "/v1/health", ""); rec.Code != http.StatusOK {
		t.Fatalf("health = %d", rec.Code)
	}
}

// An identity established before this server, with no group, is still gated.
func TestRequireGroupGatesAnIdentityFromOutside(t *testing.T) {
	g := newGateRig(t, "abhed-users")
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{Subject: "carol", Tenant: "default"}))
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an outside identity with no group = %d, want 403", rec.Code)
	}
}

// Signing out is a POST from this origin: a link, an image or another site's
// form does not end the session.
func TestSignOutNeedsAPostFromThisOrigin(t *testing.T) {
	g := newGateRig(t, "")
	alice := g.signIn(t, "alice")
	page := g.do(alice, "GET", "/logout", "", "Accept", "text/html")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `<form method="post" action="/logout">`) {
		t.Fatalf("GET /logout = %d, want a page that asks", page.Code)
	}
	if rec := g.do(alice, "GET", "/v1/sessions", ""); rec.Code != http.StatusOK {
		t.Fatalf("after GET /logout the session = %d, want it still signed in", rec.Code)
	}
	if rec := g.do(alice, "POST", "/logout", "", "Origin", "http://evil.example"); rec.Code != http.StatusForbidden {
		t.Fatalf("a cross-origin sign-out = %d, want 403", rec.Code)
	}
	if rec := g.do(alice, "GET", "/v1/sessions", ""); rec.Code != http.StatusOK {
		t.Fatalf("after a cross-origin sign-out the session = %d, want it still signed in", rec.Code)
	}
	if rec := g.do(alice, "POST", "/logout", "", "Origin", "http://example.com"); rec.Code != http.StatusFound {
		t.Fatalf("sign-out = %d, want 302", rec.Code)
	}
	if rec := g.do(alice, "GET", "/v1/sessions", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after signing out the session = %d, want 401", rec.Code)
	}
}

// The workbench, the console and the account page sign out with a POST.
func TestPagesSignOutWithAPost(t *testing.T) {
	for name, page := range map[string]string{"ide": ideHTML, "console": consoleHTML} {
		if !strings.Contains(page, "f.method = 'post'; f.action = '/logout';") ||
			!strings.Contains(page, "addEventListener('click', postSignOut)") {
			t.Errorf("the %s signs out without a POST", name)
		}
	}
	if !strings.Contains(accountHTML, `<form method="post" action="/logout">`) || strings.Contains(accountHTML, `href="/logout"`) {
		t.Error("the account page signs out with a link")
	}
}

// oneUseInvites is an invite issuer whose codes work once.
type oneUseInvites struct {
	mu    sync.Mutex
	codes map[string]string // code -> the username it was redeemed for
}

func (o *oneUseInvites) Redeem(_ context.Context, code, username string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	used, ok := o.codes[code]
	if !ok {
		return errors.New("unknown invite")
	}
	if used != "" {
		return errors.New("this invite has already been used")
	}
	o.codes[code] = username
	return nil
}

func (o *oneUseInvites) Redeemed(context.Context, string, string) error { return nil }

// A signup that fails for a taken name or a short password leaves the invite
// for the next attempt.
func TestFailedSignupKeepsTheInvite(t *testing.T) {
	g := newGateRig(t, "")
	for _, body := range []string{
		`{"username":"alice","password":"long-enough-pw","invite":"code-1"}`,
		`{"username":"carol","password":"short","invite":"code-1"}`,
		`{"username":"c","password":"long-enough-pw","invite":"code-1"}`,
	} {
		if rec := g.do(nil, "POST", "/v1/signup", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("signup %s = %d %s, want 400", body, rec.Code, rec.Body)
		}
	}
	if rec := g.do(nil, "POST", "/v1/signup", `{"username":"carol","password":"long-enough-pw","invite":"code-1"}`); rec.Code != http.StatusOK {
		t.Fatalf("signup after failures = %d %s, want the invite still usable", rec.Code, rec.Body)
	}
	if rec := g.do(nil, "POST", "/v1/signup", `{"username":"dave","password":"long-enough-pw","invite":"code-1"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("a spent invite = %d, want 403", rec.Code)
	}
}

// The console, like the workbench, says when its sign-in has ended.
func TestConsoleNoticesAnEndedSignIn(t *testing.T) {
	start := strings.Index(consoleHTML, "function signInEnded(){")
	if start < 0 {
		t.Fatal("the console has no signInEnded")
	}
	body := consoleHTML[start : start+strings.Index(consoleHTML[start:], "\n}\n")]
	for _, want := range []string{"Your sign-in ended. ", "'/?return=/console'", "hideThinking()", "es.close()"} {
		if !strings.Contains(body, want) {
			t.Errorf("signInEnded lacks %s", want)
		}
	}
	at := strings.Index(consoleHTML, "async function api(path, opts){")
	if at < 0 {
		t.Fatal("the console has no api helper")
	}
	api := consoleHTML[at:]
	if end := strings.Index(api, "\n}\n"); end < 0 || !strings.Contains(api[:end], "if(r.status === 401) signInEnded();") {
		t.Error("the console's api helper does not notice a 401")
	}
}

// outageStore is an account store that can stop answering.
type outageStore struct {
	*auth.MemoryUserStore
	down atomic.Bool
	gen  atomic.Int64
}

func (o *outageStore) Get(ctx context.Context, name string) (*auth.User, error) {
	if o.down.Load() {
		return nil, errors.New("database is down")
	}
	return o.MemoryUserStore.Get(ctx, name)
}

// Version changes with each outage, so the next request reads the store.
func (o *outageStore) Version() (string, error) { return strconv.FormatInt(o.gen.Load(), 10), nil }

func (o *outageStore) setDown(down bool) { o.down.Store(down); o.gen.Add(1) }

// otherProvider stands in for a single sign-on provider listed first, holding
// no session of this person's, and records being asked to sign out.
type otherProvider struct {
	auth.Provider
	signedOut *atomic.Bool
}

func (otherProvider) Name() string                                  { return "sso" }
func (otherProvider) Identify(*http.Request) (*auth.Identity, bool) { return nil, false }
func (otherProvider) Routes(*http.ServeMux)                         {}
func (otherProvider) PublicPaths() []string                         { return nil }
func (otherProvider) SignIn() (string, string)                      { return "", "" }
func (p otherProvider) SignOut(w http.ResponseWriter, r *http.Request) {
	p.signedOut.Store(true)
	http.Redirect(w, r, "/", http.StatusFound)
}

func newOutageRig(t *testing.T) (*gateRig, *outageStore, *atomic.Bool) {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	st := &outageStore{MemoryUserStore: auth.NewMemoryUserStore()}
	local := auth.NewLocalAuth(st, time.Hour, false)
	if err := local.CreateUser(context.Background(), auth.User{Username: "alice"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	other := &atomic.Bool{}
	h := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}),
		Auth: &auth.Middleware{Providers: []auth.Provider{otherProvider{signedOut: other}, local},
			PublicPaths: append(PublicPaths(), local.PublicPaths()...)}}).Handler()
	return &gateRig{h: h, local: local}, st, other
}

// An account store that cannot answer is a 503, not a sign-in that ended, and
// the session is still there once it answers again.
func TestAccountOutageIsNotASignOut(t *testing.T) {
	g, st, _ := newOutageRig(t)
	alice := g.signIn(t, "alice")
	st.setDown(true)
	if rec := g.do(alice, "GET", "/v1/sessions", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("during an outage = %d %s, want 503", rec.Code, rec.Body)
	}
	if rec := g.do(alice, "GET", "/v1/whoami", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("whoami during an outage = %d, want 503", rec.Code)
	}
	if rec := g.do(alice, "GET", "/v1/health", ""); rec.Code != http.StatusOK {
		t.Fatalf("health during an outage = %d, want 200", rec.Code)
	}
	st.setDown(false)
	if rec := g.do(alice, "GET", "/v1/sessions", ""); rec.Code != http.StatusOK {
		t.Fatalf("after the outage = %d, want the session intact", rec.Code)
	}
}

// Signing out ends the local session the cookie names even while its account
// cannot be read, rather than falling through to another provider.
func TestSignOutDuringAnOutageEndsTheLocalSession(t *testing.T) {
	g, st, other := newOutageRig(t)
	alice := g.signIn(t, "alice")
	st.setDown(true)
	if rec := g.do(alice, "POST", "/logout", ""); rec.Code != http.StatusFound {
		t.Fatalf("sign-out during an outage = %d", rec.Code)
	}
	if other.Load() {
		t.Error("sign-out went to another provider")
	}
	st.setDown(false)
	if rec := g.do(alice, "GET", "/v1/sessions", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after signing out during an outage the session = %d, want 401", rec.Code)
	}
}
