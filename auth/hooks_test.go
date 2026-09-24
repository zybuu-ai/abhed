package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func signedIn(t *testing.T, l *LocalAuth, user string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	l.SignInHandler(rec, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"`+user+`","password":"correct-horse-1"}`)))
	for _, c := range rec.Result().Cookies() {
		if c.Name == l.CookieName {
			return c
		}
	}
	t.Fatalf("sign-in as %s: %d %s", user, rec.Code, rec.Body)
	return nil
}

func twoUsers(t *testing.T) *LocalAuth {
	t.Helper()
	l := NewLocalAuth(NewMemoryUserStore(), time.Hour, false)
	for _, u := range []string{"alice", "bob"} {
		if err := l.CreateUser(context.Background(), User{Username: u, Email: u + "@example.com"}, "correct-horse-1"); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func TestAdmitRefusesSignInWithItsReason(t *testing.T) {
	l := twoUsers(t)
	var asked []string
	l.Admit = func(_ context.Context, u *User) error {
		asked = append(asked, u.Username)
		if u.Username == "bob" {
			return errors.New("bob is not licensed here")
		}
		return nil
	}
	rec := httptest.NewRecorder()
	l.SignInHandler(rec, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"bob","password":"correct-horse-1"}`)))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not licensed") {
		t.Fatalf("refused sign-in = %d %s", rec.Code, rec.Body)
	}
	if len(rec.Result().Cookies()) != 0 || len(l.Sessions()) != 0 {
		t.Fatal("a refused sign-in was given a session")
	}
	// A wrong password never reaches Admit.
	rec = httptest.NewRecorder()
	l.SignInHandler(rec, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"alice","password":"wrong-password"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", rec.Code)
	}
	signedIn(t, l, "alice")
	if strings.Join(asked, ",") != "bob,alice" {
		t.Fatalf("Admit asked about %v", asked)
	}
}

func TestSessionsListWithoutCookiesAndEnd(t *testing.T) {
	l := twoUsers(t)
	a := signedIn(t, l, "alice")
	b := signedIn(t, l, "bob")
	list := l.Sessions()
	if len(list) != 2 {
		t.Fatalf("sessions = %+v", list)
	}
	raw, _ := json.Marshal(list)
	for _, c := range []*http.Cookie{a, b} {
		if strings.Contains(string(raw), c.Value) {
			t.Fatal("a listed session exposes its cookie")
		}
	}
	var bobs SessionInfo
	for _, s := range list {
		if s.Created.IsZero() || s.LastSeen.IsZero() || !s.Expires.After(s.Created) || s.Provider != "local" {
			t.Errorf("incomplete entry %+v", s)
		}
		if s.Subject == "bob" {
			bobs = s
		}
	}
	if l.EndSession("not-a-session") || !l.EndSession(bobs.ID) {
		t.Fatal("EndSession did not end exactly the named session")
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(b)
	if _, ok := l.FromCookie(req); ok {
		t.Fatal("bob's ended session still resolves")
	}
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(a)
	if _, ok := l.FromCookie(req); !ok {
		t.Fatal("alice's session was ended too")
	}
}

func TestMustChangeFollowsTheAccount(t *testing.T) {
	l := twoUsers(t)
	ctx := context.Background()
	a := signedIn(t, l, "alice")
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(a)
	if l.MustChangePassword(req) {
		t.Fatal("an ordinary session must change its password")
	}
	u, _ := l.Store.Get(ctx, "alice")
	if err := l.CreateUserOrReset(ctx, u, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	if !l.MustChangePassword(req) {
		t.Fatal("a reset did not reach the live session")
	}
	if err := l.ChangePassword(ctx, "alice", "correct-horse-1", "another-pass-9"); err != nil {
		t.Fatal(err)
	}
	if l.MustChangePassword(req) {
		t.Fatal("changing the password left the session confined")
	}
}

func refuse(sub string) func(context.Context, *Identity) error {
	return func(_ context.Context, id *Identity) error {
		if id.Subject == sub {
			return errors.New("access withdrawn")
		}
		return nil
	}
}

// A provider with no SessionEnder is signed out through SignOut, keeping only
// the cookie it clears.
type signOutProvider struct{ fakeProvider }

func (signOutProvider) SignOut(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "fake", MaxAge: -1})
	http.Redirect(w, r, "https://idp.example.com/logout", http.StatusFound)
}

func TestCheckRefusesEveryWayIn(t *testing.T) {
	// Provider session, API call: 403 JSON, and the provider's cookie cleared.
	mw := Middleware{Providers: []Provider{signOutProvider{fakeProvider{name: "fake", held: "s1"}}},
		Check: refuse("cookie-user")}
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.AddCookie(&http.Cookie{Name: "fake", Value: "s1"})
	rec := httptest.NewRecorder()
	mw.Wrap(echoSubject()).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "access withdrawn") {
		t.Fatalf("provider = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "fake=") || rec.Header().Get("Location") != "" {
		t.Fatalf("headers = %v", rec.Header())
	}

	// Browser navigation: to the front door with the reason.
	req = httptest.NewRequest("GET", "/console", nil)
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: "fake", Value: "s1"})
	rec = httptest.NewRecorder()
	mw.Wrap(echoSubject()).ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/?refused=access+withdrawn" {
		t.Fatalf("navigation = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// Bearer token.
	mw = Middleware{Verifier: fakeVerifier{}, Check: refuse("user-42")}
	req = httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer good")
	rec = httptest.NewRecorder()
	mw.Wrap(echoSubject()).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("token = %d", rec.Code)
	}

	// Trusted proxy.
	mw = Middleware{TrustHeaders: true, Check: refuse("carol")}
	req = httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("X-Abhed-User", "carol")
	rec = httptest.NewRecorder()
	mw.Wrap(echoSubject()).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("proxy = %d", rec.Code)
	}
}

func TestLocalSessionIsEndedWhenRefused(t *testing.T) {
	l := twoUsers(t)
	mw := Middleware{Providers: []Provider{l}, Check: refuse("bob")}
	b := signedIn(t, l, "bob")
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.AddCookie(b)
	rec := httptest.NewRecorder()
	mw.Wrap(echoSubject()).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(l.Sessions()) != 0 {
		t.Fatalf("refused local session = %d, %d live", rec.Code, len(l.Sessions()))
	}

	// Identify, for public handlers, applies the same check.
	b = signedIn(t, l, "bob")
	req = httptest.NewRequest("GET", "/v1/whoami", nil)
	req.AddCookie(b)
	id, mode, err := mw.Identify(httptest.NewRecorder(), req)
	if id != nil || mode != "local" || err == nil || len(l.Sessions()) != 0 {
		t.Fatalf("Identify = %v %q %v", id, mode, err)
	}
	a := signedIn(t, l, "alice")
	req = httptest.NewRequest("GET", "/v1/whoami", nil)
	req.AddCookie(a)
	if id, _, err := mw.Identify(httptest.NewRecorder(), req); id == nil || err != nil {
		t.Fatalf("alice Identify = %v %v", id, err)
	}
}

// Without a Check nothing changes.
func TestNoCheckAdmitsAsBefore(t *testing.T) {
	l := twoUsers(t)
	mw := Middleware{Providers: []Provider{l}}
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.AddCookie(signedIn(t, l, "bob"))
	rec := httptest.NewRecorder()
	mw.Wrap(echoSubject()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "bob" {
		t.Fatalf("= %d %q", rec.Code, rec.Body)
	}
}

// Check is skipped only when the proxy names nobody; the anonymous identity
// then carries no groups, whatever the headers say.
func TestProxyAnonymousHasNoGroupsAndNamedIsChecked(t *testing.T) {
	var checked []string
	mw := Middleware{TrustHeaders: true, Check: func(_ context.Context, id *Identity) error {
		checked = append(checked, id.Subject)
		return nil
	}}
	var groups []string
	h := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := FromContext(r.Context())
		groups = id.Groups
	}))

	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("X-Abhed-Groups", "abhed-admin")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if len(groups) != 0 || len(checked) != 0 {
		t.Fatalf("no user: groups %v, checked %v", groups, checked)
	}

	req.Header.Set("X-Abhed-User", "anonymous")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if len(checked) != 1 || checked[0] != "anonymous" {
		t.Fatalf("a proxy that names anonymous was not checked: %v", checked)
	}
}

func TestRefusalReasonIsClipped(t *testing.T) {
	mw := Middleware{TrustHeaders: true, Check: func(context.Context, *Identity) error {
		return errors.New(strings.Repeat("x", 500))
	}}
	req := httptest.NewRequest("GET", "/ide", nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("X-Abhed-User", "carol")
	rec := httptest.NewRecorder()
	mw.Wrap(echoSubject()).ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); len(loc) != len("/?refused=")+200 {
		t.Fatalf("redirect = %d bytes", len(loc))
	}
}
