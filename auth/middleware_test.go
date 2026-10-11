package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The middleware is tested against a fake verifier and a fake provider, not
// against OIDC: what it guarantees — a token is required, a public path is
// not, a session takes precedence, a browser is sent to sign in — holds for
// any provider, and the OIDC implementation proves its own crypto elsewhere.

type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, token string) (*Identity, error) {
	if token == "good" {
		return &Identity{Subject: "user-42", Tenant: "acme"}, nil
	}
	return nil, errors.New("bad token")
}

// fakeProvider holds one session, keyed by a cookie value, and has a page of
// its own at /fake/login.
type fakeProvider struct {
	name  string
	entry string
	held  string
}

func (p fakeProvider) Name() string { return p.name }
func (p fakeProvider) Identify(r *http.Request) (*Identity, bool) {
	c, err := r.Cookie("fake")
	if err != nil || c.Value != p.held {
		return nil, false
	}
	return &Identity{Subject: "cookie-user", Tenant: "acme"}, true
}
func (p fakeProvider) Routes(*http.ServeMux)                      {}
func (p fakeProvider) PublicPaths() []string                      { return nil }
func (p fakeProvider) SignIn() (string, string)                   { return p.entry, "Fake" }
func (p fakeProvider) SignOut(http.ResponseWriter, *http.Request) {}

func echoSubject() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := FromContext(r.Context())
		fmt.Fprint(w, id.Subject)
	})
}

func TestMiddlewareRequiresToken(t *testing.T) {
	mw := Middleware{Verifier: fakeVerifier{}, PublicPaths: []string{"/v1/health"}}
	handler := mw.Wrap(echoSubject())

	// No token → 401.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token should 401, got %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 should carry WWW-Authenticate")
	}

	// Valid token → identity in context.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer good")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "user-42" {
		t.Fatalf("valid token failed: %d %q", rec.Code, rec.Body.String())
	}

	// A bad token is refused with the verifier's reason.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer forged")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "bad token") {
		t.Fatalf("bad token: %d %q", rec.Code, rec.Body.String())
	}

	// Public path bypasses auth, and still carries an identity.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/health", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "anonymous" {
		t.Fatalf("public path: %d %q", rec.Code, rec.Body.String())
	}
}

// A session is checked before a token, so the console works without every
// request carrying one; a browser with neither is sent to the front door,
// which offers the local form beside the provider's page, keeping where it
// was going.
func TestMiddlewarePrefersSessionAndRedirectsBrowsers(t *testing.T) {
	local := fakeProvider{name: "local", held: "s1"}
	sso := fakeProvider{name: "sso", entry: "/fake/login", held: "s2"}
	mw := Middleware{Providers: []Provider{local, sso}, Verifier: fakeVerifier{}}
	handler := mw.Wrap(echoSubject())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/console", nil)
	req.AddCookie(&http.Cookie{Name: "fake", Value: "s2"})
	handler.ServeHTTP(rec, req)
	if rec.Body.String() != "cookie-user" {
		t.Fatalf("session not honoured: %d %q", rec.Code, rec.Body.String())
	}

	// A browser navigation with nothing → redirect to the front door.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/console?tab=2", nil)
	req.Header.Set("Accept", "text/html")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("browser should be redirected, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/?return=%2Fconsole%3Ftab%3D2" {
		t.Errorf("redirect target %q", loc)
	}

	// An API call with nothing → 401, never a redirect.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("API caller should get 401, got %d", rec.Code)
	}

	// With no provider owning a page, the front door is where the form is.
	only := Middleware{Providers: []Provider{local}}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/console", nil)
	req.Header.Set("Accept", "text/html")
	only.Wrap(echoSubject()).ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusFound || loc != "/?return=%2Fconsole" {
		t.Errorf("local-only redirect: %d %q", rec.Code, loc)
	}
}

// An identity already in the context is the answer of whoever dispatched
// here, and is not re-examined: that is what lets an outer router
// authenticate once and hand the request to a server of its choosing.
func TestMiddlewarePassesThroughAnEstablishedIdentity(t *testing.T) {
	mw := Middleware{Verifier: fakeVerifier{}}
	handler := mw.Wrap(echoSubject())

	req := httptest.NewRequest("GET", "/v1/sessions", nil).WithContext(
		WithIdentity(context.Background(), &Identity{Subject: "outer", Tenant: "t"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "outer" {
		t.Fatalf("established identity was not honoured: %d %q", rec.Code, rec.Body.String())
	}
}

func TestRequireGroup(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin", nil).WithContext(
		WithIdentity(context.Background(), &Identity{Subject: "u", Groups: []string{"engineering"}}))
	RequireGroup("abhed-admins", ok).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-member should be forbidden, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/admin", nil).WithContext(
		WithIdentity(context.Background(), &Identity{Subject: "u", Groups: []string{"abhed-admins"}}))
	RequireGroup("abhed-admins", ok).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("member should be allowed, got %d", rec.Code)
	}
}

// With local accounts beside a provider that has its own page, a browser that
// must sign in goes to the front door, which offers both, not to the provider.
func TestChallengeGoesToTheFrontDoorWithLocalAccounts(t *testing.T) {
	for _, order := range [][]Provider{
		{fakeProvider{name: "sso", entry: "/fake/login"}, NewLocalAuth(NewMemoryUserStore(), time.Hour, false)},
		{NewLocalAuth(NewMemoryUserStore(), time.Hour, false), fakeProvider{name: "sso", entry: "/fake/login"}},
	} {
		mw := Middleware{Providers: order}
		req := httptest.NewRequest("GET", "/console", nil)
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		mw.Wrap(echoSubject()).ServeHTTP(rec, req)
		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/?return=") {
			t.Fatalf("%s first: sent to %q, want the front door", order[0].Name(), loc)
		}
	}
	mw := Middleware{Providers: []Provider{fakeProvider{name: "sso", entry: "/fake/login"}}}
	req := httptest.NewRequest("GET", "/console", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	mw.Wrap(echoSubject()).ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/fake/login?return=") {
		t.Fatalf("single sign-on alone: sent to %q", loc)
	}
}
