package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A console or IDE session labels its answers unless configured otherwise;
// a session may ask for strict, and a managed mode may only be raised.
func TestSessionVerifyMode(t *testing.T) {
	isolateAgents(t)
	cfg := config.Default()
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{})})
	verifier := func(id string) *agent.Verifier {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.running[id].Loop.Verify
	}
	if v := verifier(startSession(t, s)); v == nil || v.Mode != agent.VerifyLabel {
		t.Fatalf("a person's session labels answers by default: %+v", v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"hi","verify":"maybe"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown verify mode: %d %s", rec.Code, rec.Body)
	}

	cfg.Verify.Mode = "strict"
	cfg.ManagedKeys = []string{"verify.mode"}
	s = New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{})})
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"hi","verify":"off"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "managed") {
		t.Fatalf("a session lowered the managed verify mode: %d %s", rec.Code, rec.Body)
	}
}
