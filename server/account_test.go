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

func localServer(t *testing.T) (http.Handler, *http.Cookie) {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	if err := local.CreateUser(context.Background(), auth.User{Username: "alice"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}),
		Auth: &auth.Middleware{Providers: []auth.Provider{local},
			PublicPaths: append([]string{"/", "/v1/whoami", "/favicon.ico", "/favicon.svg"}, local.PublicPaths()...)}})
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"alice","password":"correct-horse-1"}`)))
	for _, c := range rec.Result().Cookies() {
		if c.Name == "abhed_session" {
			return h, c
		}
	}
	t.Fatalf("sign-in set no session cookie: %d %s", rec.Code, rec.Body)
	return nil, nil
}

// Switch was a link to a route only single sign-on serves, so on local
// accounts it was a 404. whoami now names a route that exists, or none.
func TestWhoamiNamesSwitchAndPasswordRoutesThatExist(t *testing.T) {
	h, cookie := localServer(t)
	req := httptest.NewRequest("GET", "/v1/whoami", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var me map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me["switch_url"] != "/logout" || me["password_url"] != "/account" {
		t.Fatalf("whoami = %v", me)
	}
	for _, path := range []string{me["switch_url"].(string), me["password_url"].(string)} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code == http.StatusNotFound {
			t.Errorf("GET %s = 404", path)
		}
	}
}

func TestAccountPageNeedsLocalAccounts(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/account", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /account without local accounts = %d, want a redirect", rec.Code)
	}
	h, cookie := localServer(t)
	req := httptest.NewRequest("GET", "/account", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/v1/password") {
		t.Fatalf("GET /account = %d", rec.Code)
	}
}

func TestFaviconIsServedBeforeSignIn(t *testing.T) {
	h, _ := localServer(t)
	for _, path := range []string{"/favicon.ico", "/favicon.svg"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml" {
			t.Errorf("GET %s = %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

// An upgraded binary must not serve last hour's editor to a new page.
func TestIDEVendorRevalidates(t *testing.T) {
	h := testServer(t).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ide/vendor/editor.js", nil))
	etag := rec.Header().Get("ETag")
	if etag == "" || !strings.Contains(rec.Header().Get("Cache-Control"), "no-cache") {
		t.Fatalf("headers = %v", rec.Header())
	}
	req := httptest.NewRequest("GET", "/ide/vendor/editor.js", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("revalidation = %d, want 304", rec.Code)
	}
}

// Nothing linked to the workbench, so it could only be reached by typing it.
func TestWorkbenchIsTheDestinationAndLinked(t *testing.T) {
	if strings.Contains(landingHTML, "location.href = '/console'") {
		t.Error("sign-in still lands on the classic console")
	}
	if !strings.Contains(consoleHTML, `href="/ide"`) {
		t.Error("the console does not link to the workbench")
	}
	if !strings.Contains(consoleHTML, "o.admin && o.admin_url") {
		t.Error("the console shows Admin without an admin page to open")
	}
}
