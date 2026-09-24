package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
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

type namedProvider struct {
	auth.Provider
	name string
}

func (p namedProvider) Name() string { return p.name }
func (p namedProvider) Identify(*http.Request) (*auth.Identity, bool) {
	return &auth.Identity{Subject: "bob", Tenant: "default"}, true
}

// Single sign-on has its own switch route and no password here; a deployment
// without sign-in offers neither.
func TestWhoamiLinksFollowTheMechanism(t *testing.T) {
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	cfg := config.Default()
	cfg.Auth.Mode = "oidc"
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{}),
		Auth: &auth.Middleware{Providers: []auth.Provider{namedProvider{local, "oidc"}}, PublicPaths: []string{"/v1/whoami"}}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/whoami", nil))
	var me map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me["switch_url"] != "/switch-user" || me["password_url"] != nil {
		t.Errorf("oidc whoami = %v", me)
	}
	rec = httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/whoami", nil))
	me = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me["switch_url"] != nil || me["password_url"] != nil {
		t.Errorf("no-auth whoami = %v", me)
	}
}

func TestServerPublicPathsIncludeTheIcon(t *testing.T) {
	for _, p := range []string{"/favicon.ico", "/favicon.svg"} {
		found := false
		for _, q := range PublicPaths() {
			found = found || q == p
		}
		if !found {
			t.Errorf("%s missing from PublicPaths", p)
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
	if strings.Count(landingHTML, "'/ide'") < 3 || strings.Contains(landingHTML, "'/console'") {
		t.Error("sign-in, sign-up and the landing button do not all open the workbench")
	}
	if !strings.Contains(consoleHTML, `href="/ide"`) {
		t.Error("the console does not link to the workbench")
	}
	if !strings.Contains(consoleHTML, "o.admin && o.admin_url") {
		t.Error("the console shows Admin without an admin page to open")
	}
}

// Without sign-in the console removes #whobox, so the way back to the
// workbench must not be inside it, nor hidden on a phone.
func TestConsoleLinksToTheWorkbenchWithoutSignIn(t *testing.T) {
	start := strings.Index(consoleHTML, `<div class="stat" id="whobox"`)
	if start < 0 {
		t.Fatal("the console has no #whobox")
	}
	end := start + strings.Index(consoleHTML[start:], "</div>")
	outside := consoleHTML[:start] + consoleHTML[end:]
	link := regexp.MustCompile(`<a class="ghost wblink" id="wblink" href="/ide"[^>]*>Workbench</a>`)
	if !link.MatchString(outside) || strings.Contains(consoleHTML[start:end], `href="/ide"`) {
		t.Fatal("the workbench link is inside #whobox, which whoami removes without sign-in")
	}
	if !strings.Contains(consoleHTML, `box.parentNode.removeChild(box)`) {
		t.Fatal("the fixture no longer removes #whobox; this test would prove nothing")
	}
	for _, hide := range []string{"#wblink{display:none", ".wblink{display:none"} {
		if strings.Contains(consoleHTML, hide) {
			t.Fatalf("the workbench link is hidden somewhere: %s", hide)
		}
	}
}
