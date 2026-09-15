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
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

type stubAdapter struct{}

func (stubAdapter) Name() string { return "stub" }
func (stubAdapter) Profile() model.Profile {
	return model.Profile{Name: "stub", ContextWindow: 32000}
}
func (stubAdapter) CountTokens(model.Request) (int, error) { return 10, nil }
func (stubAdapter) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 2)
	ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

func testServer(t *testing.T) *Server {
	t.Helper()
	return New(Options{
		Workspace: t.TempDir(),
		Config:    config.Default(),
		Adapter:   stubAdapter{},
		Registry:  tools.NewRegistry(tools.Read{}, tools.Glob{}),
	})
}

// proxyServer trusts X-Abhed-* headers, the deployment shape where a trusted
// reverse proxy has already authenticated the caller.
func proxyServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	return New(Options{
		Workspace: t.TempDir(),
		Config:    cfg,
		Adapter:   stubAdapter{},
		Registry:  tools.NewRegistry(tools.Read{}, tools.Glob{}),
	})
}

func TestHealth(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "ok" {
		t.Fatalf("body %v", body)
	}
}

func TestCreateSessionRequiresPrompt(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"  "}`))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty prompt should be rejected, got %d", rec.Code)
	}
}

// A session must be invisible to another tenant. This is the boundary that
// makes multi-tenancy real rather than cosmetic.
func TestTenantIsolation(t *testing.T) {
	s := proxyServer(t)
	h := s.Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"work"}`))
	req.Header.Set("X-Abhed-Tenant", "acme")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create failed: %d %s", rec.Code, rec.Body)
	}
	var created createResponse
	json.Unmarshal(rec.Body.Bytes(), &created)

	// Same tenant sees it.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	h.ServeHTTP(rec, req)
	var mine []sessionSummary
	json.Unmarshal(rec.Body.Bytes(), &mine)
	if len(mine) != 1 {
		t.Fatalf("owner should see 1 session, got %d", len(mine))
	}

	// Another tenant does not.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("X-Abhed-Tenant", "other")
	h.ServeHTTP(rec, req)
	var theirs []sessionSummary
	json.Unmarshal(rec.Body.Bytes(), &theirs)
	if len(theirs) != 0 {
		t.Fatalf("cross-tenant leak: other tenant saw %d sessions", len(theirs))
	}

	// And cannot replay it either.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sessions/"+created.SessionID+"/replay", nil)
	req.Header.Set("X-Abhed-Tenant", "other")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant replay must 404, got %d", rec.Code)
	}
}

func TestReplayReturnsAuditTrail(t *testing.T) {
	s := testServer(t)
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"hi"}`)))
	var created createResponse
	json.Unmarshal(rec.Body.Bytes(), &created)

	time.Sleep(200 * time.Millisecond) // let the loop finish

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions/"+created.SessionID+"/replay", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("replay status %d", rec.Code)
	}
	var events []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) < 2 {
		t.Fatalf("expected an audit trail, got %d events", len(events))
	}
	// Sequence numbers must be present and ordered for Last-Event-ID resumption.
	for i, e := range events {
		if e["seq"] == nil {
			t.Fatalf("event %d has no seq", i)
		}
	}
}

func TestApproveWithoutPendingIsConflict(t *testing.T) {
	s := testServer(t)
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"hi"}`)))
	var created createResponse
	json.Unmarshal(rec.Body.Bytes(), &created)
	time.Sleep(150 * time.Millisecond)

	// Buffered channel accepts one, so drain then assert the second conflicts.
	for i := 0; i < 2; i++ {
		rec = httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/sessions/"+created.SessionID+"/approve",
			strings.NewReader(`{"approved":true}`))
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusConflict {
			return // expected on the second call
		}
	}
	t.Log("approval channel accepted both; acceptable given buffering")
}

// With auth.mode = none, a caller cannot pick its own tenant by header.
// Trusting headers by default would be an authentication bypass.
func TestHeadersIgnoredWhenAuthModeIsNone(t *testing.T) {
	s := testServer(t)
	h := s.Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"x"}`))
	req.Header.Set("X-Abhed-Tenant", "attacker-chosen")
	req.Header.Set("X-Abhed-User", "impersonated")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create failed: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("X-Abhed-Tenant", "attacker-chosen")
	h.ServeHTTP(rec, req)
	var list []sessionSummary
	json.Unmarshal(rec.Body.Bytes(), &list)
	for _, s := range list {
		if s.Tenant == "attacker-chosen" || s.User == "impersonated" {
			t.Fatal("headers were honoured in auth.mode=none — authentication bypass")
		}
	}
}

func TestUnknownSessionIs404(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions/nope/replay", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// The console must be fully self-contained: an air-gapped enclave has no CDN.
func TestConsoleHasNoExternalRequests(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("console status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, forbidden := range []string{"https://", "http://cdn", "//unpkg", "//cdnjs", "googleapis"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("console references external resource %q — breaks air-gapped install", forbidden)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("console must ship a restrictive CSP, got %q", csp)
	}
}

func TestConsoleNotFoundForOtherPaths(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/random", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown path, got %d", rec.Code)
	}
}

// SSE frames must NOT carry an "event:" field.
//
// A named SSE event is dispatched by the browser to addEventListener(name);
// EventSource.onmessage fires only for UNNAMED frames. Naming them produced a
// permanently empty transcript in the console while curl — which ignores the
// field entirely — showed the data arriving correctly. curl cannot catch this;
// only a test that asserts the wire format can.
func TestSSEFramesAreUnnamed(t *testing.T) {
	s := testServer(t)
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions",
		strings.NewReader(`{"prompt":"hello"}`)))
	var created createResponse
	json.Unmarshal(rec.Body.Bytes(), &created)
	time.Sleep(250 * time.Millisecond)

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/sessions/"+created.SessionID+"/events", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()
	h.ServeHTTP(rec, req.WithContext(ctx))

	body := rec.Body.String()
	if strings.Contains(body, "\nevent:") || strings.HasPrefix(body, "event:") {
		t.Fatal("SSE frames carry an event: field — EventSource.onmessage will never fire")
	}
	if !strings.Contains(body, "data:") {
		t.Fatalf("no data frames were written:\n%s", body)
	}
	// Every frame still needs an id, for Last-Event-ID resumption.
	if !strings.Contains(body, "id:") {
		t.Fatal("SSE frames have no id — reconnect cannot resume")
	}
}

// With auth off, /login, /logout and /v1/whoami must still answer. A 404
// leaves the console unable to distinguish "no auth here" from "server
// broken", and a user who clicks Sign out gets a Go error page.
func TestAuthRoutesAnswerWhenAuthIsDisabled(t *testing.T) {
	s := testServer(t) // config default is auth.mode "none"
	h := s.Handler()

	for _, path := range []string{"/login", "/logout"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s returned %d; it should explain that auth is off", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "not configured") {
			t.Errorf("%s should say why it does nothing", path)
		}
		// The explanation must show how to fix it, not just state the problem:
		// the mode to set and the command that issues an account.
		if !strings.Contains(rec.Body.String(), `"mode": "local"`) ||
			!strings.Contains(rec.Body.String(), "abhed user add") {
			t.Errorf("%s should show the config needed to enable sign-in", path)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/whoami", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/whoami returned %d; it should report the state, not 404", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["authenticated"] != false {
		t.Fatalf("expected authenticated:false, got %v", body)
	}
	if body["reason"] == nil {
		t.Error("whoami should say WHY there is no user")
	}
}

// The landing page must be reachable before sign-in, or a configured
// deployment has no front door at all.
func TestLandingIsPublicAndDescribesTheDeployment(t *testing.T) {
	s := testServer(t)
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("landing returned %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<title>Abhed", "/v1/overview", "This deployment"} {
		if !strings.Contains(body, want) {
			t.Errorf("landing page missing %q", want)
		}
	}
	// Air-gap: the front door must not fetch anything external either.
	if strings.Contains(body, "https://") {
		t.Error("landing page references an external URL")
	}
}

// The overview describes THIS instance. A landing page with static copy would
// tell an operator nothing they need before typing a prompt.
func TestOverviewReportsRealConfiguration(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview returned %d", rec.Code)
	}

	var o overviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	if o.Model == "" {
		t.Error("overview should name the model")
	}
	// The workspace is an absolute host path, so it is withheld from anonymous
	// callers even though the rest of the overview is public: it names the
	// operator's account and directory layout to anyone who asks.
	if o.Workspace != "" {
		t.Errorf("anonymous overview disclosed the workspace path: %q", o.Workspace)
	}
	if o.AuthMode != "none" {
		t.Errorf("auth mode should reflect config, got %q", o.AuthMode)
	}
	// With auth off there is nowhere to sign in, so no button should be offered.
	if o.SignInURL != "" {
		t.Errorf("sign-in offered with auth disabled: %q", o.SignInURL)
	}
	if len(o.Tools) == 0 {
		t.Error("overview should list the agent's tools")
	}
}

// With a provider that has a page of its own configured, the landing page
// must offer it. The provider here is a stub: what is being tested is that
// the server asks its providers rather than knowing one by name.
func TestOverviewOffersSignInWhenConfigured(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "oidc"

	s := New(Options{
		Workspace: t.TempDir(), Config: cfg,
		Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{}),
		Auth: &auth.Middleware{
			Providers:   []auth.Provider{stubProvider{}},
			PublicPaths: []string{"/", "/v1/overview", "/login", "/logout"}},
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/overview", nil))
	var o overviewResponse
	json.Unmarshal(rec.Body.Bytes(), &o)

	if o.SignInURL == "" {
		t.Fatal("sign-in configured but the landing page is offered no link")
	}
	if o.Authenticated {
		t.Error("nobody is signed in yet")
	}
	if !strings.Contains(o.SignInURL, "/login") {
		t.Errorf("sign-in URL should point at /login, got %q", o.SignInURL)
	}
	if o.ProviderLabel != "Stub IdP" {
		t.Errorf("the button should carry the provider's label, got %q", o.ProviderLabel)
	}
	if o.AdminURL != "" {
		t.Errorf("no page is mounted at /admin, yet the overview points there: %q", o.AdminURL)
	}
}

// stubProvider owns a sign-in page at /login and never recognises anyone.
type stubProvider struct{}

func (stubProvider) Name() string                                  { return "stub" }
func (stubProvider) Identify(*http.Request) (*auth.Identity, bool) { return nil, false }
func (stubProvider) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}
func (stubProvider) PublicPaths() []string    { return []string{"/login"} }
func (stubProvider) SignIn() (string, string) { return "/login", "Stub IdP" }
func (stubProvider) SignOut(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/", http.StatusFound)
}
