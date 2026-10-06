package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Each session gets a bash of its own from SessionBash, started from the
// registry's, and releases it when the session is deleted, once.
func TestSessionBashIsTheSessionsOwn(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	var mu sync.Mutex
	made := map[string]*agent.Recorder{}
	released := map[string]int{}
	s := New(Options{
		Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}, tools.Bash{Isolation: tools.Isolation{Tier: "fence", Backend: "shared"}}),
		SessionBash: func(id string, rec *agent.Recorder, shared tools.Bash) (tools.Bash, func() error) {
			mu.Lock()
			made[id] = rec
			mu.Unlock()
			shared.Isolation.Backend = "own-" + id
			return shared, func() error {
				mu.Lock()
				released[id]++
				mu.Unlock()
				return nil
			}
		},
	})
	h := s.Handler()
	create := func() string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"work"}`))
		req.Header.Set("X-Abhed-Tenant", "acme")
		h.ServeHTTP(rec, req)
		var created createResponse
		if rec.Code/100 != 2 || json.Unmarshal(rec.Body.Bytes(), &created) != nil {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		return created.SessionID
	}
	a, b := create(), create()
	for _, id := range []string{a, b} {
		s.mu.RLock()
		live := s.running[id]
		s.mu.RUnlock()
		tool, _ := live.Loop.Tools.Get("bash")
		if bash, ok := tool.(tools.Bash); !ok || bash.Isolation.Backend != "own-"+id || bash.Isolation.Tier != "fence" {
			t.Fatalf("session %s runs %#v", id, tool)
		}
		mu.Lock()
		if made[id] == nil {
			t.Errorf("session %s was given no recorder", id)
		}
		mu.Unlock()
	}
	if shared, _ := s.opts.Registry.Get("bash"); shared.(tools.Bash).Isolation.Backend != "shared" {
		t.Fatal("the shared registry's bash was changed")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/v1/sessions/"+a, nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	h.ServeHTTP(rec, req)
	s.mu.RLock()
	live := s.running[b]
	s.mu.RUnlock()
	s.releaseBash(live)
	s.releaseBash(live)
	mu.Lock()
	defer mu.Unlock()
	if released[a] != 1 || released[b] != 1 {
		t.Fatalf("released %v (delete answered %d)", released, rec.Code)
	}
}
