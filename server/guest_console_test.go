package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A console guest (auth.guest) starts a session as an owner of its own; no
// other guest sees it; signing out removes it, running or kept.
func TestConsoleGuestKeepsNothingAfterSigningOut(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	cfg.Auth.Guest = true
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	g := auth.NewGuestAuth(time.Hour, false, "default", nil)
	defer g.Close()
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}),
		Auth: &auth.Middleware{Providers: []auth.Provider{local, g},
			PublicPaths: append(append([]string{"/", "/v1/whoami"}, local.PublicPaths()...), g.PublicPaths()...)}})
	ended := make(chan int, 1)
	g.OnEnd = func(owner string) { ended <- s.RemoveOwnerSessions(context.Background(), "", owner) }
	h := s.Handler()
	guest := func() *http.Cookie {
		req := httptest.NewRequest("POST", auth.GuestPath, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		for _, c := range rec.Result().Cookies() {
			if c.Name == g.CookieName {
				return c
			}
		}
		t.Fatalf("no guest cookie: %d %s", rec.Code, rec.Body)
		return nil
	}
	do := func(c *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(c)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	a, b := guest(), guest()
	rec := do(a, "POST", "/v1/sessions", `{"prompt":"hello"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a guest's session: %d %s", rec.Code, rec.Body)
	}
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if do(b, "GET", "/v1/sessions/"+created.SessionID+"/events", "").Code != http.StatusNotFound {
		t.Error("another guest reached the session")
	}
	if code := do(a, "GET", "/v1/sessions/"+created.SessionID+"/events", "").Code; code != http.StatusOK {
		t.Fatalf("the guest's own session: %d", code)
	}
	so := httptest.NewRequest("POST", "/logout", nil)
	so.AddCookie(a)
	so.Header.Set("Origin", "http://example.com")
	so.Host = "example.com"
	h.ServeHTTP(httptest.NewRecorder(), so)
	select {
	case n := <-ended:
		if n != 1 {
			t.Fatalf("removed %d sessions, want 1", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("signing out did not end the guest")
	}
	if events, _ := s.under().Events(created.SessionID); len(events) != 0 {
		t.Fatalf("%d events of the guest's session are kept", len(events))
	}
}
