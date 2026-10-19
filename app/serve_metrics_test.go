package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store/local"
)

func TestServeMetricsOffByDefault(t *testing.T) {
	sm, err := newServeMetrics(config.Default())
	if err != nil || sm != nil {
		t.Fatalf("%v %v", sm, err)
	}
}

func TestServeMetricsListenerToken(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Metrics = config.ServerMetricsConfig{Enabled: true, Listen: "127.0.0.1:0", TokenEnv: "ABHED_TEST_METRICS"}
	if _, err := newServeMetrics(cfg); err == nil || !strings.Contains(err.Error(), "ABHED_TEST_METRICS") {
		t.Fatalf("an unset token was accepted: %v", err)
	}
	t.Setenv("ABHED_TEST_METRICS", "s3cret")
	sm, err := newServeMetrics(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sm.m.Observe(agent.Event{Type: agent.EvSessionStarted})
	h := sm.handler()
	for _, c := range []struct {
		auth string
		want int
	}{{"", 401}, {"Bearer nope", 401}, {"Bearer s3cret", 200}} {
		req := httptest.NewRequest("GET", MetricsPath, nil)
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("%q: %d, want %d", c.auth, rec.Code, c.want)
		}
		if c.want == 200 && !strings.Contains(rec.Body.String(), "abhed_sessions_started_total 1") {
			t.Fatalf("body %s", rec.Body)
		}
		if strings.Contains(rec.Body.String(), "s3cret") {
			t.Fatal("the token is in the response")
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("the metrics listener serves more than /metrics: %d", rec.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, err := sm.run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + MetricsPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("listener answered %d without the token", resp.StatusCode)
	}
}

// The control plane's audit is counted by operation and still read back.
func TestCountedAuditKeepsReader(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Metrics.Enabled = true
	sm, _ := newServeMetrics(cfg)
	mem := controlplane.NewMemory()
	c := controlplane.Config{Audit: mem}
	if err := sm.auditHook()(context.Background(), cfg, &c); err != nil {
		t.Fatal(err)
	}
	rd, ok := c.Audit.(controlplane.AuditReader)
	if !ok {
		t.Fatal("the wrapped audit cannot be read")
	}
	ctx := context.Background()
	_ = c.Audit.Append(ctx, controlplane.AuditEvent{Action: "create", SandboxID: "sb1", Outcome: controlplane.OutcomeRequested})
	_ = c.Audit.Append(ctx, controlplane.AuditEvent{Action: "create", SandboxID: "sb1", Outcome: controlplane.OutcomeOK})
	if evs, _ := rd.AuditFor(ctx, "sb1"); len(evs) != 2 {
		t.Fatalf("%d events read back", len(evs))
	}
	var b strings.Builder
	_ = sm.m.Registry.Write(&b)
	if !strings.Contains(b.String(), `abhed_sandbox_operations_total{action="create",outcome="ok"} 1`) {
		t.Fatalf("sandbox operation not counted:\n%s", b.String())
	}
}

func TestAPIAuditLocalFile(t *testing.T) {
	cfg := config.Default()
	cfg.Record.Dir, _ = filepath.EvalSymlinks(t.TempDir())
	a, label, closeFn, err := apiAudit(context.Background(), cfg, agent.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Append(context.Background(), controlplane.AuditEvent{Principal: "alice", Action: "api:POST /v1/sessions", Outcome: "requested"}); err != nil {
		t.Fatal(err)
	}
	closeFn()
	path := local.AuditPath(cfg.Record.Dir, "default")
	if !strings.Contains(label, path) {
		t.Fatalf("label %q does not name %s", label, path)
	}
	if r, err := local.VerifyAudit(path); err != nil || !r.OK() || r.Lines != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	cfg.Storage.Driver = "postgres"
	if _, _, _, err := apiAudit(context.Background(), cfg, agent.NewMemStore()); err == nil {
		t.Fatal("postgres storage without a postgres store was accepted")
	}
}
