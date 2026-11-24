package server

import (
	"context"
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

// A session whose answers cannot be checked as configured is refused, not
// run unchecked.
func TestSessionRefusedWhenAnswersCannotBeChecked(t *testing.T) {
	isolateAgents(t)
	cfg := config.Default()
	cfg.Verify.Mode = "strict"
	cfg.Verify.Critic = config.VerifyCriticConfig{Enabled: true, Model: "ghost"}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{})})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"hi"}`)))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "cannot be checked") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	s.mu.Lock()
	n := len(s.running)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d sessions running unchecked", n)
	}
}

// An unattended run labels its answers too, unless it or the configuration
// turns checking off.
func TestUnattendedSessionLabelsByDefault(t *testing.T) {
	isolateAgents(t)
	for _, c := range []struct {
		name, cfgMode, verify string
		want                  agent.VerifyMode
	}{
		{"unset", "", "", agent.VerifyLabel},
		{"asked off", "", "off", ""},
		{"configured off", "off", "", ""},
		{"asked strict", "", "strict", agent.VerifyStrict},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Verify.Mode = c.cfgMode
			s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{})})
			id, err := s.StartSession(context.Background(), StartSpec{Prompt: "hello", Unattended: true, Verify: c.verify})
			if err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			v := s.running[id].Loop.Verify
			s.mu.Unlock()
			switch {
			case c.want == "" && v != nil:
				t.Fatalf("checked although turned off: %+v", v)
			case c.want != "" && (v == nil || v.Mode != c.want):
				t.Fatalf("want %s, got %+v", c.want, v)
			}
		})
	}
}
