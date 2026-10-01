package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flipVerifier accepts "good" while allow is set.
type flipVerifier struct{ allow *atomic.Bool }

func (v flipVerifier) Verify(_ context.Context, token string) (*Identity, error) {
	if token == "good" && v.allow.Load() {
		return &Identity{Subject: "user-42", Tenant: "acme"}, nil
	}
	return nil, errors.New("token revoked")
}

// captureCtx runs one request through m and returns the handler's context.
func captureCtx(t *testing.T, m Middleware, req *http.Request) context.Context {
	t.Helper()
	var got context.Context
	rec := httptest.NewRecorder()
	m.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Context() })).ServeHTTP(rec, req)
	if got == nil {
		t.Fatalf("request refused: %d %s", rec.Code, rec.Body.String())
	}
	return got
}

// Recheck reruns the same authentication a new request gets: a bearer token
// that stops verifying, or a Check that starts refusing, fails it.
func TestRecheckFollowsTheVerifierAndCheck(t *testing.T) {
	allow := &atomic.Bool{}
	allow.Store(true)
	var refuse atomic.Bool
	m := Middleware{Verifier: flipVerifier{allow}, Check: func(context.Context, *Identity) error {
		if refuse.Load() {
			return errors.New("access revoked")
		}
		return nil
	}}
	req := httptest.NewRequest("GET", "/v1/sessions/x/events", nil)
	req.Header.Set("Authorization", "Bearer good")
	ctx := captureCtx(t, m, req)

	if id, err := Recheck(ctx); err != nil || id.Subject != "user-42" {
		t.Fatalf("still authorised: %v, %v", id, err)
	}
	refuse.Store(true)
	if _, err := Recheck(ctx); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("Check refuses now, Recheck: %v", err)
	}
	refuse.Store(false)
	allow.Store(false)
	if _, err := Recheck(ctx); err == nil {
		t.Fatal("the token no longer verifies, yet Recheck passed")
	}
}

// A local session signed out everywhere fails its recheck, and the account
// layer says so to whoever is listening, for each way a session ends.
func TestRecheckFollowsLocalSignOut(t *testing.T) {
	local := NewLocalAuth(NewMemoryUserStore(), time.Hour, false)
	if err := local.CreateUser(context.Background(), User{Username: "bob"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	var told []string
	local.OnChange(func(u string) { told = append(told, u) })
	rec := httptest.NewRecorder()
	local.SignInHandler(rec, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"bob","password":"correct-horse-1"}`)))
	// A sign-in is reported too, so what an older account left held is let go.
	if len(told) != 1 || told[0] != "bob" {
		t.Fatalf("a sign-in was not reported: %q", told)
	}
	told = nil
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == local.CookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("sign-in: %d", rec.Code)
	}
	m := Middleware{Providers: []Provider{local}}
	req := httptest.NewRequest("GET", "/v1/sessions/x/events", nil)
	req.AddCookie(cookie)
	ctx := captureCtx(t, m, req)
	if _, err := Recheck(ctx); err != nil {
		t.Fatalf("still signed in: %v", err)
	}

	if err := local.SetGroups(context.Background(), "bob", "ops", true); err != nil {
		t.Fatal(err)
	}
	if len(told) != 1 || told[0] != "bob" {
		t.Fatalf("a group change was not reported: %q", told)
	}
	local.RevokeUser("bob")
	if len(told) < 2 || told[len(told)-1] != "bob" {
		t.Fatalf("a sign-out everywhere was not reported: %q", told)
	}
	if _, err := Recheck(ctx); err == nil {
		t.Fatal("signed out everywhere, yet the stream's recheck passed")
	}
}

// A provider may rewrite the request it is shown; the recheck shows it a
// copy, so the next recheck still sees the credentials the stream opened with.
func TestRecheckDoesNotChangeTheOriginalRequest(t *testing.T) {
	p := &mutatingProvider{}
	m := Middleware{Providers: []Provider{p}}
	req := httptest.NewRequest("GET", "/v1/sessions/x/events", nil)
	req.AddCookie(&http.Cookie{Name: "fake", Value: "s1"})
	ctx := captureCtx(t, m, req)
	for range 3 {
		if _, err := Recheck(ctx); err != nil {
			t.Fatalf("recheck: %v", err)
		}
	}
	if c, err := req.Cookie("fake"); err != nil || c.Value != "s1" {
		t.Fatalf("the original request's cookie was changed: %v %v", c, err)
	}
}

// mutatingProvider identifies the "fake" cookie and, from its second call
// on, renames it in the request it was given, as a guard that hides a session
// does when its check cannot be made.
type mutatingProvider struct {
	fakeProvider
	calls atomic.Int32
}

func (p *mutatingProvider) Identify(r *http.Request) (*Identity, bool) {
	c, err := r.Cookie("fake")
	if err != nil || c.Value != "s1" {
		return nil, false
	}
	if p.calls.Add(1) > 1 {
		r.Header.Set("Cookie", "held=s1")
	}
	return &Identity{Subject: "cookie-user", Tenant: "acme"}, true
}

// Without a middleware rerun to call, Recheck answers with the identity the
// context already holds: no auth, or an outer router that did not set one.
func TestRecheckWithoutARerun(t *testing.T) {
	ctx := WithIdentity(context.Background(), &Identity{Subject: "anonymous"})
	if id, err := Recheck(ctx); err != nil || id.Subject != "anonymous" {
		t.Fatalf("got %v, %v", id, err)
	}
	ctx = captureCtx(t, Middleware{}, httptest.NewRequest("GET", "/v1/sessions/x/events", nil))
	if id, err := Recheck(ctx); err != nil || id.Subject != "anonymous" {
		t.Fatalf("no-auth recheck: %v, %v", id, err)
	}
}
