package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress"
)

// searchSet builds a tool set offering web_search against base, under the
// allowlist with rules when allow is non-nil, or with the network setting off.
func searchSet(t *testing.T, base string, rules []egress.Rule, allowlist bool) *Set {
	t.Helper()
	var cfg config.Config
	cfg.WebSearch.Enabled, cfg.WebSearch.Provider, cfg.WebSearch.BaseURL = true, "searxng", base
	if allowlist {
		cfg.Sandbox.Network = config.NetworkAllowlist
		cfg.Egress.Rules = rules
	}
	s := Build(context.Background(), cfg, Options{Workspace: t.TempDir(), Parts: WebSearch, Vault: storeWith(t, "unused-secret-value")})
	t.Cleanup(s.Close)
	return s
}

func search(t *testing.T, s *Set) string {
	t.Helper()
	tool, ok := s.Registry.Get("web_search")
	if !ok {
		t.Fatal("no web_search")
	}
	raw, _ := json.Marshal(map[string]any{"query": "q"})
	return tool.Run(context.Background(), nil, raw).Content
}

// Sets open at once each judge their own requests by their own rules: an
// allowlist set's guard never judges another set's, allowlist or not.
func TestTwoSetsKeepTheirOwnGuards(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"results":[{"title":"Found","url":"https://example.com/","content":"c"}]}`)
	}))
	defer srv.Close()
	port := int(netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://")).Port())
	allow := []egress.Rule{{Host: "127.0.0.1", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}

	open := searchSet(t, srv.URL, nil, false)
	allowing := searchSet(t, srv.URL, allow, true)
	if out := search(t, open); !strings.Contains(out, "Found") {
		t.Fatalf("the set outside the allowlist was judged by another's rules: %s", out)
	}
	if out := search(t, allowing); !strings.Contains(out, "Found") {
		t.Fatalf("the allowlist set's own rule did not allow it: %s", out)
	}
	denying := searchSet(t, srv.URL, nil, true)
	if out := search(t, denying); strings.Contains(out, "Found") || !strings.Contains(out, "refused") {
		t.Fatalf("a set with no rule was judged by another set's rules: %s", out)
	}
	if out := search(t, allowing); !strings.Contains(out, "Found") {
		t.Fatalf("the allowing set was judged by the denying set's rules: %s", out)
	}
	if out := search(t, open); !strings.Contains(out, "Found") {
		t.Fatalf("the set outside the allowlist was judged with two guards open: %s", out)
	}
	if hits.Load() != 4 {
		t.Fatalf("hits %d", hits.Load())
	}

	// A request naming no set is judged by the one guard installed, and
	// refused while two are.
	tr := &egress.Transport{Kind: egress.KindWebSearch}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "several are in force") {
		t.Fatalf("a request naming no set with two guards in force: %v", err)
	}
	// One naming its set is judged by it alone.
	ctx := egress.WithGuard(context.Background(), allowing.Guard())
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err = (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("a request naming the allowing set: %v", err)
	}
	_ = resp.Body.Close()
	ctx = egress.WithGuard(context.Background(), denying.Guard())
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err = (&http.Client{Transport: tr}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	var de *egress.DeniedError
	if !errors.As(err, &de) {
		t.Fatalf("a request naming the denying set: %v", err)
	}
	if open.Guard() != egress.Unguarded {
		t.Fatal("the set outside the allowlist has a guard")
	}
}
