package mcpauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/mcpauth/mcpauthtest"
	"github.com/zybuu-ai/abhed/internal/secrets"
)

func testStore(t *testing.T) *secrets.Store {
	t.Helper()
	return secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
}

func noRedirect() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func login(t *testing.T, s *mcpauthtest.Server, store *secrets.Store, mod func(*LoginOptions)) error {
	t.Helper()
	o := LoginOptions{Server: "corp", URL: s.MCP.URL + "/mcp", HTTP: noRedirect(), Open: mcpauthtest.Browser(t), Timeout: 10 * time.Second}
	if mod != nil {
		mod(&o)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return Login(ctx, store, o)
}

// A login discovers the authorization server from the server's challenge,
// registers a client, signs in with PKCE and the resource, and keeps the
// tokens bound to the server under names never offered or handed out.
func TestLoginDiscoversRegistersAndKeepsBoundTokens(t *testing.T) {
	s := mcpauthtest.New(t)
	s.ISS = true
	store := testStore(t)
	if err := login(t, s, store, nil); err != nil {
		t.Fatal(err)
	}
	if res, _, _ := s.Get(); res != s.MCP.URL+"/mcp" {
		t.Fatalf("resource %q", res)
	}
	rec, err := load(store, "corp")
	if err != nil || rec.AccessToken != "access-1" || rec.RefreshToken != "refresh-1" || rec.URL != s.MCP.URL+"/mcp" || rec.ClientID != "cid-1" {
		t.Fatalf("record %+v %v", rec, err)
	}
	if offered := store.Offered(); len(offered) != 0 {
		t.Fatalf("held names offered: %v", offered)
	}
	r, a, _, _ := names("corp")
	if _, err := store.Env([]string{a}); err == nil {
		t.Fatal("a held token was handed out by name")
	}
	if err := store.Set(r, "planted"); err == nil {
		t.Fatal("a held name was set by hand")
	}
	// The stored token values are redacted from what the record keeps.
	red := store.Redactor()
	if out := string(red.Redact([]byte(`{"x":"access-1 and refresh-1"}`))); strings.Contains(out, "access-1") || strings.Contains(out, "refresh-1") {
		t.Fatalf("not redacted: %s", out)
	}
	a2, err := Load(store, "corp", s.MCP.URL+"/mcp/", noRedirect())
	if err != nil || a2 == nil {
		t.Fatalf("load: %v", err)
	}
	if tok, err := a2.Token(context.Background()); err != nil || tok != "access-1" {
		t.Fatalf("token %q %v", tok, err)
	}
	// Bound: the same name at another URL has no login.
	if _, err := Load(store, "corp", s.MCP.URL+"/other", noRedirect()); err == nil || !strings.Contains(err.Error(), "abhed mcp login corp") {
		t.Fatalf("another URL: %v", err)
	}
	if a, err := Load(store, "other", s.MCP.URL+"/mcp", noRedirect()); a != nil || err != nil {
		t.Fatalf("another name: %v %v", a, err)
	}
}

// A token near its end is refreshed, the rotated refresh token kept; a
// refused refresh says to sign in again; a token the server refused is
// refreshed once.
func TestRefresh(t *testing.T) {
	s := mcpauthtest.New(t)
	store := testStore(t)
	if err := login(t, s, store, nil); err != nil {
		t.Fatal(err)
	}
	a, err := Load(store, "corp", s.MCP.URL+"/mcp", noRedirect())
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	tok, err := a.Token(context.Background())
	if err != nil || tok != "access-2" {
		t.Fatalf("refresh: %q %v", tok, err)
	}
	if rec, _ := load(store, "corp"); rec.RefreshToken != "refresh-2" || rec.AccessToken != "access-2" {
		t.Fatalf("stored %+v", rec)
	}
	a.now = time.Now
	if tok, err := a.Refresh(context.Background(), "access-1"); err != nil || tok != "access-2" {
		t.Fatalf("a refresh for a token already replaced: %q %v", tok, err)
	}
	if tok, err := a.Refresh(context.Background(), "access-2"); err != nil || tok != "access-3" {
		t.Fatalf("refresh after a 401: %q %v", tok, err)
	}
	if st, err := State(store, "corp", s.MCP.URL+"/mcp"); err != nil || !st.Usable(time.Now()) || !st.RefreshRefused.IsZero() {
		t.Fatalf("a live login reads as %+v %v", st, err)
	}
	s.SetDenyRefresh(true)
	if _, err := a.Refresh(context.Background(), "access-3"); err == nil || !strings.Contains(err.Error(), "abhed mcp login corp") {
		t.Fatalf("a refused refresh: %v", err)
	}
	// The refusal is kept: the login reads as dead, and the refused token is
	// not sent again, by this process or another.
	st, err := State(store, "corp", s.MCP.URL+"/mcp")
	if err != nil || !st.Held || st.RefreshRefused.IsZero() || st.Usable(time.Now()) {
		t.Fatalf("after a refused refresh the login reads as %+v %v", st, err)
	}
	other, _ := Load(store, "corp", s.MCP.URL+"/mcp", noRedirect())
	if _, err := other.Refresh(context.Background(), "access-3"); err == nil || !strings.Contains(err.Error(), "refused at refresh") {
		t.Fatalf("a second refresh: %v", err)
	}
	if n := s.RefreshesRefused(); n != 1 {
		t.Fatalf("a refused refresh token was sent %d times", n)
	}
	// Signing in again clears it.
	s.SetDenyRefresh(false)
	if err := login(t, s, store, nil); err != nil {
		t.Fatal(err)
	}
	if st, err := State(store, "corp", s.MCP.URL+"/mcp"); err != nil || !st.Usable(time.Now()) || !st.RefreshRefused.IsZero() {
		t.Fatalf("a new login reads as %+v %v", st, err)
	}
}

// A refusal the store could not keep is said, and still never sent again by this process.
func TestRefusalKeptInMemoryWhenTheStoreFails(t *testing.T) {
	s := mcpauthtest.New(t)
	store := testStore(t)
	if err := login(t, s, store, nil); err != nil {
		t.Fatal(err)
	}
	a, _ := Load(store, "corp", s.MCP.URL+"/mcp", noRedirect())
	old := keepRefusal
	keepRefusal = func(*secrets.Store, Record) error { return errors.New("disk full") }
	t.Cleanup(func() { keepRefusal = old })
	s.SetDenyRefresh(true)
	if _, err := a.Refresh(context.Background(), "access-1"); err == nil || !strings.Contains(err.Error(), "could not be kept") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("a lost refusal was not said: %v", err)
	}
	if _, err := a.Refresh(context.Background(), "access-1"); err == nil || !strings.Contains(err.Error(), "refused at refresh") {
		t.Fatalf("second refresh: %v", err)
	}
	if n := s.RefreshesRefused(); n != 1 {
		t.Fatalf("the refused token was sent %d times", n)
	}
}

// State says apart a login for another URL, one ended with no refresh
// token, and none.
func TestLoginState(t *testing.T) {
	s := mcpauthtest.New(t)
	store := testStore(t)
	if st, err := State(store, "corp", s.MCP.URL+"/mcp"); err != nil || st.Held {
		t.Fatalf("no login: %+v %v", st, err)
	}
	if err := login(t, s, store, nil); err != nil {
		t.Fatal(err)
	}
	if st, err := State(store, "corp", s.MCP.URL+"/other"); err != nil || !st.OtherURL || st.Usable(time.Now()) {
		t.Fatalf("another URL: %+v %v", st, err)
	}
	ended := LoginState{Held: true, Expiry: time.Now().Add(-time.Minute)}
	if ended.Usable(time.Now()) {
		t.Fatal("an ended login with no refresh token reads as usable")
	}
	ended.Refreshable = true
	if !ended.Usable(time.Now()) {
		t.Fatal("an ended login with a refresh token reads as dead")
	}
}

// Another process's refresh is taken up rather than refreshing again.
func TestRefreshTakesAnotherProcesssTokens(t *testing.T) {
	s := mcpauthtest.New(t)
	store := testStore(t)
	if err := login(t, s, store, nil); err != nil {
		t.Fatal(err)
	}
	one, _ := Load(store, "corp", s.MCP.URL+"/mcp", noRedirect())
	two, _ := Load(store, "corp", s.MCP.URL+"/mcp", noRedirect())
	if tok, err := one.Refresh(context.Background(), "access-1"); err != nil || tok != "access-2" {
		t.Fatal(tok, err)
	}
	if _, issued, _ := s.Get(); issued != 2 {
		t.Fatalf("issued %d", issued)
	}
	if tok, err := two.Refresh(context.Background(), "access-1"); err != nil || tok != "access-2" {
		t.Fatalf("the second process: %q %v", tok, err)
	}
	if _, issued, _ := s.Get(); issued != 2 {
		t.Fatalf("the second process refreshed again: issued %d", issued)
	}
}

// Discovery refuses what would send the login elsewhere or weaken it.
func TestDiscoveryRefuses(t *testing.T) {
	for name, set := range map[string]func(*mcpauthtest.Server){
		"resource of another server": func(s *mcpauthtest.Server) { s.WrongResource = true },
		"issuer mismatch":            func(s *mcpauthtest.Server) { s.WrongIssuer = true },
		"no PKCE S256":               func(s *mcpauthtest.Server) { s.NoS256 = true },
		"no registration":            func(s *mcpauthtest.Server) { s.NoRegister = true },
	} {
		t.Run(name, func(t *testing.T) {
			s := mcpauthtest.New(t)
			set(s)
			store := testStore(t)
			if err := login(t, s, store, func(o *LoginOptions) { o.Open = nil; o.Timeout = time.Second }); err == nil {
				t.Fatal("signed in")
			}
			if _, err := load(store, "corp"); !errors.Is(err, ErrNoLogin) {
				t.Fatalf("something was kept: %v", err)
			}
		})
	}
	// A server in the clear is never signed in to.
	if _, err := Discover(context.Background(), noRedirect(), "http://mcp.example.com/mcp"); err == nil || !strings.Contains(err.Error(), "not https") {
		t.Fatalf("http: %v", err)
	}
}

// A client registered ahead of time is used as configured, with its secret.
func TestPreRegisteredClient(t *testing.T) {
	s := mcpauthtest.New(t)
	s.NoRegister, s.ClientSecret = true, "shh-client"
	store := testStore(t)
	if err := login(t, s, store, func(o *LoginOptions) { o.ClientID, o.ClientSecret = "pre", "shh-client" }); err != nil {
		t.Fatal(err)
	}
	if rec, _ := load(store, "corp"); rec.ClientID != "pre" || rec.ClientSecret != "shh-client" {
		t.Fatalf("record %+v", rec)
	}
}

// Only the redirect that carries the login's state ends the wait: a stray
// request is refused and the login goes on.
func TestCallbackNeedsTheState(t *testing.T) {
	results := make(chan callbackResult, 1)
	h := callbackHandler("good-state", Metadata{}, results)
	for _, target := range []string{"/callback?code=x&state=bad", "/callback?code=x", "/other?code=x&state=good-state"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("%s accepted", target)
		}
	}
	select {
	case r := <-results:
		t.Fatalf("a stray request ended the wait: %+v", r)
	default:
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/callback?error=access_denied&state=good-state", nil))
	if r := <-results; r.err == nil || !strings.Contains(r.err.Error(), "access_denied") {
		t.Fatalf("error: %+v", r)
	}
	h = callbackHandler("s", Metadata{ISSResponse: true, Issuer: "https://as.example"}, results)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/callback?code=x&state=s&iss=https://evil.example", nil))
	if r := <-results; r.err == nil {
		t.Fatal("a sign-in from another issuer was taken")
	}
}

// Logout revokes both tokens and forgets the login.
func TestLogout(t *testing.T) {
	s := mcpauthtest.New(t)
	store := testStore(t)
	if err := login(t, s, store, nil); err != nil {
		t.Fatal(err)
	}
	found, err := Logout(context.Background(), store, "corp", noRedirect())
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if _, _, revoked := s.Get(); len(revoked) != 2 {
		t.Fatalf("revoked %v", revoked)
	}
	if names, _ := store.Names(); len(names) != 0 {
		t.Fatalf("left %v", names)
	}
	if found, err := Logout(context.Background(), store, "corp", noRedirect()); found || err != nil {
		t.Fatal(found, err)
	}
}

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"HTTPS://MCP.Example.com:443/mcp/": "https://mcp.example.com/mcp",
		"http://127.0.0.1:80/a#frag":       "http://127.0.0.1/a",
		"https://[::1]:8443/x":             "https://[::1]:8443/x",
	} {
		if got, err := Canonical(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"ftp://x/y", "https://user:pw@x/y", "/relative", "mailto:a@b"} {
		if _, err := Canonical(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if bearerParams(`Bearer realm="x", resource_metadata="https://a/b", scope="r w"`)["scope"] != "r w" {
		t.Error("bearer params")
	}
}

// A login removed while a session holds it stays removed: the next refresh
// says it was signed out, writes nothing back, and the token is not used again.
func TestLogoutSticksAgainstARunningSession(t *testing.T) {
	s := mcpauthtest.New(t)
	store := testStore(t)
	if err := login(t, s, store, nil); err != nil {
		t.Fatal(err)
	}
	a, err := Load(store, "corp", s.URL(), noRedirect())
	if err != nil {
		t.Fatal(err)
	}
	if found, err := Remove(context.Background(), store, "corp"); !found || err != nil {
		t.Fatal(found, err)
	}
	if _, err := a.Refresh(context.Background(), "access-1"); err == nil || !strings.Contains(err.Error(), "signed out") {
		t.Fatalf("refresh after logout: %v", err)
	}
	if in, _, _, _ := Status(store, "corp", s.URL()); in {
		t.Fatal("the removed login was written back")
	}
	if _, err := a.Token(context.Background()); err == nil {
		t.Fatal("the removed login's token was used again")
	}
	if _, issued, _ := s.Get(); issued != 1 {
		t.Fatalf("the token endpoint was asked again: %d", issued)
	}
}

// A login made again with another client while a session held the old one
// is not overwritten by the old one's refresh.
func TestRefreshDoesNotOverwriteANewLogin(t *testing.T) {
	s := mcpauthtest.New(t)
	store := testStore(t)
	if err := login(t, s, store, nil); err != nil {
		t.Fatal(err)
	}
	a, _ := Load(store, "corp", s.URL(), noRedirect())
	rec, _ := load(store, "corp")
	rec.ClientID, rec.AccessToken = "another-client", "new-login-token"
	if err := save(context.Background(), store, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Refresh(context.Background(), "access-1"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("refresh over a new login: %v", err)
	}
	if got, _ := load(store, "corp"); got.AccessToken != "new-login-token" || got.ClientID != "another-client" {
		t.Fatalf("the new login was overwritten: %+v", got)
	}
}

// http on loopback is taken only for a server that is itself on loopback:
// a remote server cannot point the sign-in at a local service.
func TestLoopbackHTTPOnlyForALocalServer(t *testing.T) {
	if _, err := endpointURL("token endpoint", "http://127.0.0.1:9/token", false); err == nil {
		t.Fatal("a remote server's loopback http endpoint was taken")
	}
	if _, err := endpointURL("token endpoint", "http://127.0.0.1:9/token", true); err != nil {
		t.Fatal(err)
	}
	if _, err := endpointURL("token endpoint", "http://10.0.0.1/token", true); err == nil {
		t.Fatal("http off this machine was taken")
	}
}

// An iss on the redirect is compared even when the server did not say it sends one.
func TestCallbackComparesAnISSPresent(t *testing.T) {
	results := make(chan callbackResult, 1)
	h := callbackHandler("s", Metadata{Issuer: "https://as.example"}, results)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/callback?code=x&state=s&iss=https://evil.example", nil))
	if r := <-results; r.err == nil {
		t.Fatal("a sign-in from another issuer was taken")
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/callback?code=x&state=s&iss=https://as.example", nil))
	if r := <-results; r.err != nil || r.code != "x" {
		t.Fatalf("%+v", r)
	}
}

// A logout reaches a running session within storeCheck, refresh or not.
func TestLogoutReachesARunningSessionsToken(t *testing.T) {
	s := mcpauthtest.New(t)
	store := testStore(t)
	if err := login(t, s, store, nil); err != nil {
		t.Fatal(err)
	}
	a, _ := Load(store, "corp", s.URL(), noRedirect())
	start := time.Now()
	a.now = func() time.Time { return start }
	if tok, err := a.Token(context.Background()); err != nil || tok != "access-1" {
		t.Fatal(tok, err)
	}
	if _, err := Remove(context.Background(), store, "corp"); err != nil {
		t.Fatal(err)
	}
	if tok, err := a.Token(context.Background()); err != nil || tok != "access-1" {
		t.Fatalf("within the check's interval the token stands: %q %v", tok, err)
	}
	a.now = func() time.Time { return start.Add(storeCheck) }
	if _, err := a.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "signed out") {
		t.Fatalf("after logout: %v", err)
	}
}

// A store that cannot be read fails every call until it reads again, not one
// call in each interval.
func TestUnreadableStoreFailsEveryCall(t *testing.T) {
	s := mcpauthtest.New(t)
	store := testStore(t)
	if err := login(t, s, store, nil); err != nil {
		t.Fatal(err)
	}
	a, _ := Load(store, "corp", s.URL(), noRedirect())
	if err := os.Chmod(store.Path(), 0o644); err != nil { // readable by others: refused
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := a.Token(context.Background()); err == nil {
			t.Fatalf("call %d used the token while the store could not be read", i)
		}
	}
	if err := os.Chmod(store.Path(), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := a.Token(context.Background()); err != nil || tok != "access-1" {
		t.Fatalf("once it reads: %q %v", tok, err)
	}
}
