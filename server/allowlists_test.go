package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// The managed allowed models and MCP lists hold in the console: a model
// outside the list is neither offered nor run, a session asked to start on
// it is refused and audited, an MCP server the lists refuse is not added,
// and the policy view shows each list.
func TestServerHoldsTheAllowLists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"model":{"allowed":["openai-compatible/corp-*"]},"mcp":{"denied":[{"name":"bad-*"}]}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
	home := t.TempDir()
	t.Setenv("HOME", home)
	user := `{"model":{"default":"corp","providers":{
	  "corp":{"type":"openai-compatible","base_url":"http://127.0.0.1:1/v1","model":"corp-model","context_window":8192},
	  "mine":{"type":"openai-compatible","base_url":"http://127.0.0.1:1/v1","model":"mine-model","context_window":8192}}}}`
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wb := shellBench(t, func(c *config.Config) {
		auth := c.Auth
		*c = loaded
		c.Auth = auth
	})
	var mu sync.Mutex
	var audited []string
	wb.s.opts.AdminAudit = func(_ context.Context, action, target string, detail map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		audited = append(audited, action+" "+target)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/providers", nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	wb.h.ServeHTTP(rec, req)
	var got []providerInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(got) != 1 || got[0].Name != "corp" {
		t.Fatalf("providers %+v, want corp alone", got)
	}
	if _, err := wb.s.resolveProvider("mine"); err == nil || !strings.Contains(err.Error(), "model.allowed admits only openai-compatible/corp-*") {
		t.Fatalf("a session on mine: %v", err)
	}
	if _, err := wb.s.StartSession(context.Background(), StartSpec{Prompt: "hi", Provider: "mine"}); err == nil {
		t.Fatal("a session started on a refused model")
	}
	mu.Lock()
	if len(audited) != 1 || audited[0] != "config.refused model.allowed" {
		t.Fatalf("audited %v", audited)
	}
	mu.Unlock()

	rows := allowListRows(wb.s.opts.Config)
	if len(rows) != 2 || rows[0].Key != "model.allowed" || rows[0].Layer != config.LayerManaged || rows[1].Key != "mcp.denied" {
		t.Fatalf("policy view rows %+v", rows)
	}

	gw := mcp.NewGateway()
	gw.Admit = toolset.MCPAdmit(wb.s.opts.Config)
	t.Cleanup(gw.Close)
	wb.s.state.mu.Lock()
	wb.s.state.gateway = gw
	wb.s.state.mu.Unlock()
	rec = httptest.NewRecorder()
	wb.s.addMCP(rec, httptest.NewRequest("POST", "/v1/admin/mcp", strings.NewReader(`{"name":"bad-one","url":"https://mcp.example/x"}`)))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "mcp.denied") {
		t.Fatalf("adding a denied server: %d %s", rec.Code, rec.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if audited[len(audited)-1] != "mcp.refused bad-one" {
		t.Fatalf("audited %v", audited)
	}
}
