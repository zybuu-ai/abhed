package server

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/webfetch"
)

// A server session's policy asks before web_fetch when no host list is set.
func TestServerPolicyAsksBeforeWebFetchWithoutAHostList(t *testing.T) {
	cfg := config.Default()
	cfg.WebFetch.Enabled = true
	s := &Server{opts: Options{Config: cfg, Workspace: t.TempDir()}}
	args := []byte(`{"url":"https://example.com/"}`)
	// Plan too: a console client may narrow any session to plan, and that must
	// not turn an ask into a silent request.
	for _, mode := range []policy.Mode{policy.ModeAuto, policy.ModeDefault, policy.ModePlan} {
		if got := s.newPolicy(mode).Evaluate("web_fetch", false, args); got.Decision != policy.Ask {
			t.Fatalf("%s, no host list: %+v", mode, got)
		}
	}
	s.opts.Config.WebFetch.AllowedHosts = []string{"example.com"}
	if got := s.newPolicy(policy.ModeAuto).Evaluate("web_fetch", false, args); got.Decision != policy.Allow {
		t.Fatalf("with a host list: %+v", got)
	}
	// Another port on a listed host is another service: it asks.
	if got := s.newPolicy(policy.ModeAuto).Evaluate("web_fetch", false, []byte(`{"url":"https://example.com:8443/"}`)); got.Decision != policy.Ask {
		t.Fatalf("a listed host on another port: %+v", got)
	}
}

// systemAdapter keeps the system prompt of each request and answers at once.
type systemAdapter struct {
	mu   sync.Mutex
	seen []string
}

func (*systemAdapter) Name() string { return "system" }
func (*systemAdapter) Profile() model.Profile {
	return model.Profile{Name: "system", ContextWindow: 32000}
}
func (*systemAdapter) CountTokens(model.Request) (int, error) { return 10, nil }
func (a *systemAdapter) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	a.mu.Lock()
	a.seen = append(a.seen, req.System)
	a.mu.Unlock()
	ch := make(chan model.Chunk, 2)
	ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

// A console session's prompt names web_fetch only when its registry has it.
func TestServerPromptNamesWebFetchOnlyWhenRegistered(t *testing.T) {
	const line = "web_fetch reads one page in full"
	for _, fetch := range []bool{true, false} {
		reg := tools.NewRegistry(tools.Read{})
		if fetch {
			reg.Add(&webfetch.Tool{})
		}
		cfg := config.Default()
		cfg.Auth.Mode = "proxy"
		ad := &systemAdapter{}
		s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: ad, Registry: reg,
			Store: agent.NewMemStore(), Redact: noRedact{}})
		id := sessionOf(t, callAs(t, s, "alice", "", "POST", "/v1/sessions", `{"prompt":"hi"}`))
		eventsUntil(t, s, id, agent.EvSessionEnded)
		ad.mu.Lock()
		sent := strings.Join(ad.seen, "\n")
		ad.mu.Unlock()
		if strings.Contains(sent, line) != fetch {
			t.Errorf("web_fetch registered %v: the prompt names it = %v", fetch, !fetch)
		}
		if !fetch && !strings.Contains(sent, "this session has no web tool") {
			t.Errorf("no web tool: the prompt does not say so")
		}
	}
}
