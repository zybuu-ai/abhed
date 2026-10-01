package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestAuth(t *testing.T) *LocalAuth {
	t.Helper()
	return NewLocalAuth(NewMemoryUserStore(), time.Hour, false)
}

func mustCreate(t *testing.T, l *LocalAuth, name, pw string) { //nolint:unparam // a fixture; the fixed argument documents what the tests rely on
	t.Helper()
	if err := l.CreateUser(context.Background(),
		User{Username: name, Email: name + "@example.com"}, pw); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

func TestAuthenticateAcceptsCorrectPassword(t *testing.T) {
	l := newTestAuth(t)
	mustCreate(t, l, "ada", "correct-horse-battery")

	u, err := l.Authenticate(context.Background(), "ada", "correct-horse-battery")
	if err != nil {
		t.Fatalf("valid credentials rejected: %v", err)
	}
	if u.Tenant != "default" {
		t.Errorf("tenant = %q, want the default assigned at creation", u.Tenant)
	}
	// The hash must never leave the process in a serialized form.
	blob, _ := json.Marshal(u)
	if strings.Contains(string(blob), "$2a$") {
		t.Errorf("password hash serialized into JSON: %s", blob)
	}
}

func TestAuthenticateRejectsWrongPassword(t *testing.T) {
	l := newTestAuth(t)
	mustCreate(t, l, "ada", "correct-horse-battery")

	if _, err := l.Authenticate(context.Background(), "ada", "wrong-horse-battery"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("err = %v, want ErrBadCredentials", err)
	}
}

// An unknown user and a wrong password must be indistinguishable, or the
// sign-in form becomes a username oracle.
func TestUnknownUserIsIndistinguishable(t *testing.T) {
	l := newTestAuth(t)
	mustCreate(t, l, "ada", "correct-horse-battery")

	_, missing := l.Authenticate(context.Background(), "nobody", "whatever-at-all")
	_, wrong := l.Authenticate(context.Background(), "ada", "whatever-at-all")

	if missing == nil || wrong == nil {
		t.Fatal("both lookups should fail")
	}
	if missing.Error() != wrong.Error() {
		t.Errorf("distinguishable errors leak which usernames exist:\n missing: %v\n wrong:   %v",
			missing, wrong)
	}
}

// The dummy-hash path exists so a missing user costs the same as a real one.
// Without it, response time enumerates accounts.
func TestMissingUserStillCostsBcryptTime(t *testing.T) {
	l := newTestAuth(t)
	mustCreate(t, l, "ada", "correct-horse-battery")

	start := time.Now()
	_, _ = l.Authenticate(context.Background(), "nobody", "whatever-at-all")
	missing := time.Since(start)

	start = time.Now()
	_, _ = l.Authenticate(context.Background(), "ada", "whatever-at-all")
	wrong := time.Since(start)

	// Not a tight bound — CI timing is noisy. This catches the real bug,
	// which is returning immediately (microseconds) versus hashing (millis).
	if missing < wrong/4 {
		t.Errorf("missing user returned in %v vs %v for a wrong password: "+
			"timing enumerates usernames", missing, wrong)
	}
}

func TestWeakPasswordRejected(t *testing.T) {
	l := newTestAuth(t)
	err := l.CreateUser(context.Background(), User{Username: "ada"}, "short")
	if !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("err = %v, want ErrWeakPassword", err)
	}
}

func TestDuplicateUsernameRejected(t *testing.T) {
	l := newTestAuth(t)
	mustCreate(t, l, "ada", "correct-horse-battery")
	err := l.CreateUser(context.Background(),
		User{Username: "ADA"}, "another-long-password")
	if !errors.Is(err, ErrUserExists) {
		t.Fatalf("err = %v, want ErrUserExists — usernames are case-insensitive", err)
	}
}

func TestInvalidUsernameRejected(t *testing.T) {
	l := newTestAuth(t)
	for _, name := range []string{"a", "has space", "semi;colon", ""} {
		if err := l.CreateUser(context.Background(),
			User{Username: name}, "a-long-enough-password"); err == nil {
			t.Errorf("username %q accepted, want rejection", name)
		}
	}
}

func TestSignInIssuesUsableSession(t *testing.T) {
	l := newTestAuth(t)
	mustCreate(t, l, "ada", "correct-horse-battery")

	body := `{"username":"ada","password":"correct-horse-battery"}`
	r := httptest.NewRequest("POST", "/v1/signin", strings.NewReader(body))
	w := httptest.NewRecorder()
	l.SignInHandler(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie set")
	}
	c := cookies[0]
	if !c.HttpOnly {
		t.Error("session cookie is not HttpOnly: readable by any injected script")
	}

	// The cookie must actually resolve to the identity.
	r2 := httptest.NewRequest("GET", "/v1/whoami", nil)
	r2.AddCookie(c)
	id, ok := l.FromCookie(r2)
	if !ok {
		t.Fatal("issued cookie does not resolve to a session")
	}
	if id.Subject != "ada" {
		t.Errorf("subject = %q, want ada", id.Subject)
	}
}

func TestSignInRejectsBadPasswordWithoutCookie(t *testing.T) {
	l := newTestAuth(t)
	mustCreate(t, l, "ada", "correct-horse-battery")

	body := `{"username":"ada","password":"nope-nope-nope"}`
	r := httptest.NewRequest("POST", "/v1/signin", strings.NewReader(body))
	w := httptest.NewRecorder()
	l.SignInHandler(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("a failed sign-in set a session cookie")
	}
}

func TestSignOutInvalidatesSession(t *testing.T) {
	l := newTestAuth(t)
	mustCreate(t, l, "ada", "correct-horse-battery")

	r := httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"ada","password":"correct-horse-battery"}`))
	w := httptest.NewRecorder()
	l.SignInHandler(w, r)
	c := w.Result().Cookies()[0]

	out := httptest.NewRequest("GET", "/logout", nil)
	out.AddCookie(c)
	l.SignOut(httptest.NewRecorder(), out)

	check := httptest.NewRequest("GET", "/v1/whoami", nil)
	check.AddCookie(c)
	if _, ok := l.FromCookie(check); ok {
		t.Error("session still valid after sign out")
	}
}

func TestLocalExpiredSessionRejected(t *testing.T) {
	l := NewLocalAuth(NewMemoryUserStore(), time.Millisecond, false)
	mustCreate(t, l, "ada", "correct-horse-battery")

	w := httptest.NewRecorder()
	l.SignInHandler(w, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"ada","password":"correct-horse-battery"}`)))
	c := w.Result().Cookies()[0]

	time.Sleep(5 * time.Millisecond)
	r := httptest.NewRequest("GET", "/v1/whoami", nil)
	r.AddCookie(c)
	if _, ok := l.FromCookie(r); ok {
		t.Error("expired session accepted")
	}
}

func TestChangePassword(t *testing.T) {
	ctx := context.Background()
	l := newTestAuth(t)
	mustCreate(t, l, "ada", "correct-horse-battery")

	if err := l.ChangePassword(ctx, "ada", "wrong-password-here", "new-long-password"); err == nil {
		t.Error("changed password without the correct current one")
	}
	if err := l.ChangePassword(ctx, "ada", "correct-horse-battery", "short"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("err = %v, want ErrWeakPassword for a short replacement", err)
	}
	if err := l.ChangePassword(ctx, "ada", "correct-horse-battery", "new-long-password"); err != nil {
		t.Fatalf("legitimate change rejected: %v", err)
	}
	if _, err := l.Authenticate(ctx, "ada", "new-long-password"); err != nil {
		t.Error("new password does not work")
	}
	if _, err := l.Authenticate(ctx, "ada", "correct-horse-battery"); err == nil {
		t.Error("old password still works after a change")
	}
}

// An administratively reset password is temporary: the user must be told to
// replace it, or a generated password quietly becomes permanent.
func TestAdminResetForcesChange(t *testing.T) {
	ctx := context.Background()
	l := newTestAuth(t)
	mustCreate(t, l, "ada", "correct-horse-battery")

	u, _ := l.Store.Get(ctx, "ada")
	if err := l.CreateUserOrReset(ctx, u, "reset-by-the-admin"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	got, err := l.Authenticate(ctx, "ada", "reset-by-the-admin")
	if err != nil {
		t.Fatalf("reset password does not work: %v", err)
	}
	if !got.MustChange {
		t.Error("reset password not flagged as needing a change")
	}

	// Changing it clears the flag.
	if err := l.ChangePassword(ctx, "ada", "reset-by-the-admin", "chosen-by-the-user"); err != nil {
		t.Fatalf("change after reset: %v", err)
	}
	got, _ = l.Authenticate(ctx, "ada", "chosen-by-the-user")
	if got.MustChange {
		t.Error("MustChange still set after the user chose their own password")
	}
}

// Concurrent creates through two LocalAuths sharing a store: one wins.
func TestCreateUserAcrossInstancesOneWins(t *testing.T) {
	store := NewMemoryUserStore()
	a, b := NewLocalAuth(store, time.Hour, false), NewLocalAuth(store, time.Hour, false)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, la := range []*LocalAuth{a, b} {
		wg.Add(1)
		go func(i int, la *LocalAuth) {
			defer wg.Done()
			errs[i] = la.CreateUser(context.Background(), User{Username: "dup", Email: "d@example.test"}, "long enough pw")
		}(i, la)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("errs = %v; want exactly one success", errs)
	}
	for _, err := range errs {
		if err != nil && !errors.Is(err, ErrUserExists) {
			t.Fatalf("loser's error = %v, want ErrUserExists", err)
		}
	}
}
