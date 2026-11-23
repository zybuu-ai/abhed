package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func guestStart(t *testing.T, g *GuestAuth) *http.Cookie {
	t.Helper()
	mux := http.NewServeMux()
	g.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, GuestPath, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == g.CookieName {
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Errorf("cookie %+v", c)
			}
			return c
		}
	}
	t.Fatal("no guest cookie")
	return nil
}

// A console guest is an identity of its own with no groups; each guest is a
// different owner; signing out ends it and says whose sessions to remove.
func TestGuestConsoleSessions(t *testing.T) {
	var mu sync.Mutex
	var ended []string
	g := NewGuestAuth(time.Hour, true, "default", func(o string) { mu.Lock(); ended = append(ended, o); mu.Unlock() })
	defer g.Close()
	c1, c2 := guestStart(t, g), guestStart(t, g)
	req := func(c *http.Cookie) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(c)
		return r
	}
	id1, ok1 := g.Identify(req(c1))
	id2, ok2 := g.Identify(req(c2))
	if !ok1 || !ok2 || id1.Owner() == id2.Owner() || !strings.HasPrefix(id1.Owner(), "guest:") || len(id1.Groups) != 0 {
		t.Fatalf("identities %+v %+v", id1, id2)
	}
	g.EndRequestSession(httptest.NewRecorder(), req(c1))
	if _, ok := g.Identify(req(c1)); ok {
		t.Fatal("a signed-out guest is still known")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(ended)
		mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ended) != 1 || ended[0] != id1.Owner() {
		t.Fatalf("ended %v, want %s", ended, id1.Owner())
	}
}

// An expired guest is forgotten and ended; a start that is not JSON (a
// cross-site form) is refused.
func TestGuestConsoleExpiryAndForms(t *testing.T) {
	ended := make(chan string, 1)
	g := NewGuestAuth(time.Hour, true, "default", func(o string) { ended <- o })
	defer g.Close()
	now := time.Now()
	g.now = func() time.Time { return now }
	c := guestStart(t, g)
	now = now.Add(2 * time.Hour)
	g.reap()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("an expired guest was not ended")
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(c)
	if _, ok := g.Identify(r); ok {
		t.Fatal("an expired guest is still known")
	}
	mux := http.NewServeMux()
	g.Routes(mux)
	form := httptest.NewRequest(http.MethodPost, GuestPath, strings.NewReader("x=1"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, form)
	if rec.Code == http.StatusOK {
		t.Fatal("a form started a guest")
	}
}
