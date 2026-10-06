package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/server"
)

type doneAdapter struct{}

func (doneAdapter) Name() string                           { return "done" }
func (doneAdapter) Profile() model.Profile                 { return model.Profile{Name: "done", ContextWindow: 32000} }
func (doneAdapter) CountTokens(model.Request) (int, error) { return 10, nil }
func (doneAdapter) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 2)
	ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

// Served sessions each run their commands under a fence of their own, in
// its mount mode, with a cgroup of the session's: the record of each holds
// its own fence.qualified and its commands' launches, and the workspace's
// .abhed is out of the commands' sight. It needs what the fence's own tests
// need, and runs with ABHED_REQUIRE_FENCE=1.
func TestServeRunsEachSessionUnderItsOwnFence(t *testing.T) {
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		t.Skip("set ABHED_REQUIRE_FENCE=1 in a delegated cgroup to fence real commands")
	}
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "users.json"), []byte(`{"users":[{"name":"owner"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	cfg.Sandbox.Tier = string(sandbox.TierFence)
	cfg.Sandbox.MaxProcs, cfg.Sandbox.MaxMemoryMB = 128, 512
	sb, err := buildSandbox(cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	fence := fenceOf(sb)
	if fence == nil || fence.Mode() != sandbox.FenceModeMounts {
		t.Fatalf("the startup fence: %v", sb.Describe())
	}
	t.Cleanup(func() { _ = fence.Close() })
	s := server.New(server.Options{
		Workspace: ws, Config: cfg, Adapter: doneAdapter{},
		Registry:    tools.NewRegistry(tools.Read{}, tools.Bash{Sandbox: fence.Command, Isolation: tools.Isolation{Tier: "fence"}}),
		SessionBash: fenceSessionBash(cfg, ws),
	})
	h := s.Handler()
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Abhed-Tenant", "acme")
		h.ServeHTTP(rec, req)
		return rec
	}
	sandboxes := map[string]bool{}
	for range 2 {
		rec := do("POST", "/v1/sessions", `{"prompt":"work"}`)
		var created struct {
			SessionID string `json:"session_id"`
		}
		if rec.Code/100 != 2 || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.SessionID == "" {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		id := created.SessionID
		eventually := func(ok func() bool) {
			for range 100 {
				if ok() {
					return
				}
				_ = do("GET", "/v1/sessions/"+id+"/replay", "")
			}
		}
		eventually(func() bool { return do("GET", "/v1/sessions/"+id+"/replay", "").Code == 200 })
		rec = do("POST", "/v1/sessions/"+id+"/exec", `{"command":"cat .abhed/users.json; echo planted > .abhed/users.json; echo ran"}`)
		var out struct {
			Output string `json:"output"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != 200 || !strings.Contains(out.Output, "ran") || strings.Contains(out.Output, "owner") {
			t.Fatalf("exec: %d %s", rec.Code, rec.Body.String())
		}
		var evs []agent.Event
		_ = json.Unmarshal(do("GET", "/v1/sessions/"+id+"/replay", "").Body.Bytes(), &evs)
		var q, l map[string]any
		for _, e := range evs {
			switch e.Type {
			case agent.EvFenceQualified:
				_ = json.Unmarshal(e.Payload, &q)
			case agent.EvProcessLaunched:
				_ = json.Unmarshal(e.Payload, &l)
			}
		}
		if q == nil || l == nil || q["mode"] != sandbox.FenceModeMounts || l["mode"] != sandbox.FenceModeMounts {
			t.Fatalf("session %s record: qualified %v launched %v", id, q, l)
		}
		sbx, _ := q["sandbox"].(string)
		if cg, _ := l["cgroup"].(string); sbx == "" || !strings.Contains(cg, sbx) || sandboxes[sbx] {
			t.Fatalf("session %s: sandbox %q cgroup %q, seen %v", id, sbx, cg, sandboxes)
		}
		sandboxes[sbx] = true
		if del := do("DELETE", "/v1/sessions/"+id, ""); del.Code/100 != 2 {
			t.Fatalf("delete: %d %s", del.Code, del.Body.String())
		}
		if cg, _ := q["cgroup"].(string); cg == "" {
			t.Fatalf("no session cgroup in %v", q)
		} else if _, err := os.Stat(cg); err == nil {
			t.Fatalf("the session's cgroup %s outlived its deletion", cg)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(ws, ".abhed", "users.json")); !strings.Contains(string(b), "owner") {
		t.Fatalf("users.json changed: %q", b)
	}
}
