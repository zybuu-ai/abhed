package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signInAs posts a sign-in, sending prior when set, and returns the cookie issued.
func signInAs(t *testing.T, l *LocalAuth, name, pw string, prior *http.Cookie) *http.Cookie {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/signin", strings.NewReader(`{"username":"`+name+`","password":"`+pw+`"}`))
	if prior != nil {
		r.AddCookie(prior)
	}
	w := httptest.NewRecorder()
	l.SignInHandler(w, r)
	for _, c := range w.Result().Cookies() {
		if c.Name == l.CookieName {
			return c
		}
	}
	t.Fatalf("sign-in as %s: %d %s", name, w.Code, w.Body)
	return nil
}

func resolves(l *LocalAuth, c *http.Cookie) bool {
	r := httptest.NewRequest("GET", "/v1/whoami", nil)
	r.AddCookie(c)
	_, ok := l.FromCookie(r)
	return ok
}

// An account removed and made again under its name, from another process
// whose writes this one does not see as a change of its own, starts afresh:
// the old account's cookie does not reach the new one.
func TestRecreatedAccountDoesNotInheritTheCookie(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryUserStore()
	l := NewLocalAuth(store, time.Hour, false)
	mustCreate(t, l, "bob", "correct-horse-1")
	old := signInAs(t, l, "bob", "correct-horse-1", nil)
	u, _ := store.Get(ctx, "bob")
	// Same password, so only the creation time tells the two accounts apart.
	time.Sleep(2 * time.Millisecond)
	if err := store.Delete(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	u.CreatedAt = time.Now().UTC()
	if err := store.Put(ctx, u); err != nil {
		t.Fatal(err)
	}
	if resolves(l, old) {
		t.Fatal("the removed account's cookie still works for the account made again")
	}
}

// Signing in as someone else from the same browser ends the session the
// browser sent, rather than leaving the earlier cookie valid.
func TestSignInEndsTheSessionTheBrowserHeld(t *testing.T) {
	l := newTestAuth(t)
	mustCreate(t, l, "ada", "correct-horse-1")
	mustCreate(t, l, "bob", "correct-horse-1")
	a := signInAs(t, l, "ada", "correct-horse-1", nil)
	b := signInAs(t, l, "bob", "correct-horse-1", a)
	if resolves(l, a) {
		t.Fatal("ada's cookie still works after bob signed in from the same browser")
	}
	if !resolves(l, b) {
		t.Fatal("bob's new cookie does not work")
	}
}

// An address with a character that shows as nothing, or as a plain space, is
// refused, so it cannot pose as another account's address.
func TestEmailRefusesInvisibleCharacters(t *testing.T) {
	for _, e := range []string{
		"b‍ob@example.com", "bob​@example.com", "bo­b@example.com",
		"bob️@example.com", "bob@exam⁠ple.com", "bㅤob@example.com",
	} {
		if ValidEmail(e) == nil {
			t.Errorf("%q was accepted", e)
		}
	}
	for _, e := range []string{"bob@example.com", "zoë@example.com"} {
		if err := ValidEmail(e); err != nil {
			t.Errorf("%q was refused: %v", e, err)
		}
	}
	l := newTestAuth(t)
	if err := l.CreateUser(context.Background(), User{Username: "bob2", Email: "b‍ob@example.com"}, "correct-horse-1"); err == nil {
		t.Fatal("an account was made with a zero-width character in its email")
	}
}
