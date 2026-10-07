package websearch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// Under the allowlist a search is judged by the egress rules: an allowed
// instance answers, another is refused unsent, and both are recorded.
func TestSearchUnderTheAllowlist(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"results":[{"title":"T","url":"https://example.com/","content":"c"}]}`)
	}))
	defer srv.Close()
	port := int(netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://")).Port())
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{
		{Host: "127.0.0.1", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	g := egress.NewGuard(egress.GuardOptions{Policy: pol})
	defer g.Close()
	defer egress.Install(g)()

	var mu sync.Mutex
	var got []map[string]any
	ctx := egress.WithCaller(context.Background(), egress.Caller{Session: "s1", CallID: "call-s",
		Record: func(_ string, m map[string]any) error { mu.Lock(); got = append(got, m); mu.Unlock(); return nil }})
	p, err := New(Config{Provider: "searxng", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := p.Search(ctx, "q", 5); err != nil || len(res) != 1 {
		t.Fatalf("allowed search: %v %v", res, err)
	}
	other, _ := New(Config{Provider: "searxng", BaseURL: fmt.Sprintf("http://localhost:%d", port)})
	if _, err := other.Search(ctx, "q", 5); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("denied search: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits %d", hits.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0]["decision"] != "allow" || got[0]["kind"] != egress.KindWebSearch || got[0]["call_id"] != "call-s" ||
		got[1]["decision"] != "deny" || got[1]["host"] != "localhost" {
		t.Fatalf("records: %v", got)
	}
}
