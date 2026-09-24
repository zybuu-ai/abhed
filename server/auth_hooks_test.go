package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/tools"
)

type auditEntry struct {
	action, target string
	detail         map[string]any
	by             string
}

// hookRig is a local-accounts server with alice (an administrator with an
// email address) and bob, and a record of what AdminAudit was told.
type hookRig struct {
	h     http.Handler
	local *auth.LocalAuth
	mu    sync.Mutex
	audit []auditEntry
}

func newHookRig(t *testing.T, check func(context.Context, *auth.Identity) error, opt func(*Options)) *hookRig {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	ctx := context.Background()
	for _, u := range []auth.User{
		{Username: "alice", Email: "alice@example.com", Groups: []string{DefaultAdminGroup}},
		{Username: "bob", Email: "bob@example.com"},
	} {
		if err := local.CreateUser(ctx, u, "correct-horse-1"); err != nil {
			t.Fatal(err)
		}
	}
	rig := &hookRig{local: local}
	opts := Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}),
		Auth: &auth.Middleware{Providers: []auth.Provider{local}, Check: check,
			PublicPaths: append(PublicPaths(), local.PublicPaths()...)},
		AdminAudit: func(ctx context.Context, action, target string, detail map[string]any) {
			rig.mu.Lock()
			rig.audit = append(rig.audit, auditEntry{action, target, detail, UserOf(ctx)})
			rig.mu.Unlock()
		},
	}
	if opt != nil {
		opt(&opts)
	}
	rig.h = New(opts).Handler()
	return rig
}

func (g *hookRig) signIn(t *testing.T, user string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"`+user+`","password":"correct-horse-1"}`)))
	for _, c := range rec.Result().Cookies() {
		if c.Name == "abhed_session" {
			return c
		}
	}
	t.Fatalf("sign-in as %s: %d %s", user, rec.Code, rec.Body)
	return nil
}

func (g *hookRig) do(c *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, req)
	return rec
}

func TestAdminMutationsReachTheAuditHook(t *testing.T) {
	roots := t.TempDir()
	g := newHookRig(t, nil, func(o *Options) { o.SkillRoots = []string{roots} })
	alice := g.signIn(t, "alice")
	g.do(alice, "POST", "/v1/admin/users/admin", `{"username":"bob","admin":true}`)
	g.do(alice, "POST", "/v1/admin/users/admin", `{"username":"bob","admin":false}`)
	if rec := g.do(alice, "POST", "/v1/admin/skills/reload", ``); rec.Code != http.StatusOK {
		t.Fatalf("reload = %d %s", rec.Code, rec.Body)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	want := []string{"user.admin_granted", "user.admin_revoked", "skills.reloaded"}
	if len(g.audit) != len(want) {
		t.Fatalf("audit = %+v", g.audit)
	}
	for i, a := range want {
		if g.audit[i].action != a || g.audit[i].by != "alice@example.com" {
			t.Errorf("audit[%d] = %+v, want %s by alice", i, g.audit[i], a)
		}
	}
	if g.audit[0].target != "bob" {
		t.Errorf("grant target = %q", g.audit[0].target)
	}
}

// MCP and reindex need a gateway and an index to succeed, so their handlers
// are checked in source, as the privilege tests do.
func TestEveryAdminMutationIsAudited(t *testing.T) {
	src := readSource(t, "server.go") + readSource(t, "settings.go") + readSource(t, "users.go")
	handlers := regexp.MustCompile(`mux\.Handle\("POST /v1/admin/[^"]+", s\.Admin\(s\.(\w+)\)\)`).FindAllStringSubmatch(src, -1)
	// Four today; fewer means the pattern stopped matching, not that the routes went.
	if len(handlers) < 4 {
		t.Fatalf("found %d admin mutation routes, want at least 4", len(handlers))
	}
	for _, h := range handlers {
		body := regexp.MustCompile(`(?s)func \(s \*Server\) ` + h[1] + `\(.*?\n\}`).FindString(src)
		if body == "" {
			t.Errorf("cannot find the handler %s", h[1])
			continue
		}
		if !strings.Contains(body, "s.adminAudit(") {
			t.Errorf("%s changes the deployment without calling adminAudit", h[1])
		}
	}
}

// An MCP URL or command can carry a credential; neither the audit record nor
// the log may hold it.
func TestMCPAuditOmitsCredentials(t *testing.T) {
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := `{"protocolVersion":"2024-11-05","serverInfo":{"name":"t","version":"1"},"capabilities":{"tools":{}}}`
		if req.Method == "tools/list" {
			result = `{"tools":[{"name":"search","inputSchema":{"type":"object"}}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	}))
	defer mcpSrv.Close()

	var logs bytes.Buffer
	var logMu sync.Mutex
	g := newHookRig(t, nil, func(o *Options) {
		o.Gateway = mcp.NewGateway()
		o.Logger = slog.New(slog.NewTextHandler(lockedWriter{&logs, &logMu}, nil))
	})
	alice := g.signIn(t, "alice")
	u := strings.Replace(mcpSrv.URL, "http://", "http://user:secret-pass@", 1) + "/mcp?token=secret-tok&x=1"
	body, _ := json.Marshal(map[string]string{"name": "corpus", "url": u})
	if rec := g.do(alice, "POST", "/v1/admin/mcp", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("add MCP = %d %s", rec.Code, rec.Body)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.audit) != 1 || g.audit[0].action != "mcp.added" {
		t.Fatalf("audit = %+v", g.audit)
	}
	rec := fmt.Sprint(g.audit[0].detail)
	logMu.Lock()
	logged := logs.String()
	logMu.Unlock()
	for _, secret := range []string{"secret-pass", "secret-tok", "user:"} {
		if strings.Contains(rec, secret) || strings.Contains(logged, secret) {
			t.Errorf("%q reached the audit (%s) or the log", secret, rec)
		}
	}
	if want := mcpSrv.URL + "/mcp"; g.audit[0].detail["url"] != want {
		t.Errorf("url = %v, want %s", g.audit[0].detail["url"], want)
	}

	d := mcpAuditDetail(mcpRequest{Command: "/usr/bin/tool --api-key=abc"}, nil)
	if d["command"] != "/usr/bin/tool" {
		t.Errorf("command = %v", d["command"])
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func refuseBob(_ context.Context, id *auth.Identity) error {
	if id.Subject == "bob" {
		return errors.New("bob's access was withdrawn")
	}
	return nil
}

func TestCheckRefusesAndEndsTheSession(t *testing.T) {
	g := newHookRig(t, refuseBob, nil)
	bob := g.signIn(t, "bob")

	rec := g.do(bob, "GET", "/v1/sessions", "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "withdrawn") {
		t.Fatalf("API call = %d %s, want 403 with the reason", rec.Code, rec.Body)
	}
	if len(g.local.Sessions()) != 0 {
		t.Fatal("the refused session is still live")
	}

	bob = g.signIn(t, "bob")
	req := httptest.NewRequest("GET", "/ide", nil)
	req.Header.Set("Accept", "text/html")
	req.AddCookie(bob)
	rec = httptest.NewRecorder()
	g.h.ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusFound || !strings.HasPrefix(loc, "/?refused=") {
		t.Fatalf("navigation = %d %q, want a redirect to sign-in with the reason", rec.Code, loc)
	}

	// alice is unaffected.
	if rec := g.do(g.signIn(t, "alice"), "GET", "/v1/sessions", ""); rec.Code != http.StatusOK {
		t.Fatalf("alice = %d", rec.Code)
	}
}

func TestWhoamiAndOverviewHonourCheck(t *testing.T) {
	g := newHookRig(t, refuseBob, nil)
	var me map[string]any
	_ = json.Unmarshal(g.do(g.signIn(t, "bob"), "GET", "/v1/whoami", "").Body.Bytes(), &me)
	if me["authenticated"] != false || !strings.Contains(me["reason"].(string), "withdrawn") {
		t.Fatalf("whoami = %v", me)
	}
	var o map[string]any
	_ = json.Unmarshal(g.do(g.signIn(t, "bob"), "GET", "/v1/overview", "").Body.Bytes(), &o)
	if o["authenticated"] != false || o["workspace"] != nil {
		t.Fatalf("overview = %v", o)
	}
	_ = json.Unmarshal(g.do(g.signIn(t, "alice"), "GET", "/v1/overview", "").Body.Bytes(), &o)
	if o["authenticated"] != true || o["admin"] != true {
		t.Fatalf("alice overview = %v", o)
	}
}

func TestMustChangeConfinesTheSession(t *testing.T) {
	g := newHookRig(t, nil, nil)
	ctx := context.Background()
	u, _ := g.local.Store.Get(ctx, "bob")
	if err := g.local.CreateUserOrReset(ctx, u, "temporary-pw-1"); err != nil {
		t.Fatal(err)
	}
	rec := g.do(nil, "POST", "/v1/signin", `{"username":"bob","password":"temporary-pw-1"}`)
	var bob *http.Cookie
	for _, c := range rec.Result().Cookies() {
		bob = c
	}

	if rec := g.do(bob, "GET", "/v1/sessions", ""); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "password change required") {
		t.Fatalf("API = %d %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest("GET", "/ide", nil)
	req.Header.Set("Accept", "text/html")
	req.AddCookie(bob)
	nav := httptest.NewRecorder()
	g.h.ServeHTTP(nav, req)
	if nav.Code != http.StatusFound || nav.Header().Get("Location") != "/account?must_change=1" {
		t.Fatalf("navigation = %d %q", nav.Code, nav.Header().Get("Location"))
	}
	for _, p := range []string{"/account", "/v1/whoami", "/favicon.svg", "/ide/vendor/editor.js"} {
		if rec := g.do(bob, "GET", p, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", p, rec.Code)
		}
	}
	var me map[string]any
	_ = json.Unmarshal(g.do(bob, "GET", "/v1/whoami", "").Body.Bytes(), &me)
	if me["must_change_password"] != true {
		t.Errorf("whoami = %v", me)
	}

	if rec := g.do(bob, "POST", "/v1/password",
		`{"current_password":"temporary-pw-1","new_password":"a-new-password-2"}`); rec.Code != http.StatusOK {
		t.Fatalf("change = %d %s", rec.Code, rec.Body)
	}
	if rec := g.do(bob, "GET", "/v1/sessions", ""); rec.Code != http.StatusOK {
		t.Fatalf("after the change = %d", rec.Code)
	}
	if rec := g.do(bob, "GET", "/logout", ""); rec.Code != http.StatusFound {
		t.Fatalf("logout = %d", rec.Code)
	}
}

func TestProxyWhoamiReportsTheProxyIdentity(t *testing.T) {
	s := proxyServer(t)
	req := httptest.NewRequest("GET", "/v1/whoami", nil)
	req.Header.Set("X-Abhed-User", "carol")
	req.Header.Set("X-Abhed-Groups", "eng,ops")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var me map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me["authenticated"] != true || me["auth_mode"] != "proxy" || me["subject"] != "carol" {
		t.Fatalf("whoami = %v", me)
	}
	if g, _ := me["groups"].([]any); len(g) != 2 {
		t.Errorf("groups = %v", me["groups"])
	}
	for _, k := range []string{"switch_url", "sign_out_url", "password_url"} {
		if _, ok := me[k]; ok {
			t.Errorf("proxy whoami offers %s: %v", k, me)
		}
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/whoami", nil))
	me = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me["authenticated"] != false {
		t.Fatalf("no proxy user, whoami = %v", me)
	}

	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	cfg.Auth.ProxyLogoutURL = "https://sso.example.com/logout"
	h := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{})}).Handler()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	me = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me["sign_out_url"] != cfg.Auth.ProxyLogoutURL {
		t.Fatalf("with a logout URL, whoami = %v", me)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/logout", nil))
	if rec.Header().Get("Location") != cfg.Auth.ProxyLogoutURL {
		t.Fatalf("/logout = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// The chips draw Sign out only from whoami, so a proxy without a logout URL
// shows none.
func TestChipsTakeSignOutFromWhoami(t *testing.T) {
	for name, src := range map[string]string{"console": consoleHTML, "workbench": readSource(t, "ide.html")} {
		if !strings.Contains(src, "if(me.sign_out_url){ $('signout').href = me.sign_out_url; $('signout').hidden = false; }") {
			t.Errorf("%s does not take Sign out from whoami", name)
		}
	}
	if !strings.Contains(consoleHTML, `id="signout" href="/logout" hidden`) {
		t.Error("the console shows Sign out before whoami answers")
	}
}
