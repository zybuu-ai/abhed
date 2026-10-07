package webfetch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// Under the allowlist web_fetch is judged by the egress rules and recorded,
// and its own guard still refuses a private address an egress rule allows.
func TestWebFetchUnderTheAllowlist(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("fetched under the allowlist"))
	}))
	ap := netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://"))
	port := int(ap.Port())
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{
		{Host: "site.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
		// The egress rule names the private range; web_fetch never reaches it.
		{Host: "internal.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"10.0.0.0/8"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	tool.Guard = egress.NewGuard(egress.GuardOptions{Policy: pol, Resolve: tool.lookup})
	t.Cleanup(tool.Guard.Close)

	var got []map[string]any
	ctx := egress.WithCaller(context.Background(), egress.Caller{Session: "s1", CallID: "call-f",
		Record: func(_ string, m map[string]any) error { mu.Lock(); got = append(got, m); mu.Unlock(); return nil }})
	fetch := func(u string) string {
		raw, _ := json.Marshal(map[string]any{"url": u})
		return tool.Run(ctx, nil, raw).Content
	}
	if out := fetch(siteURL(srv, "/ok")); !strings.Contains(out, "fetched under the allowlist") {
		t.Fatalf("allowed fetch: %s", out)
	}
	if out := fetch("http://internal.test:" + strings.TrimPrefix(siteURL(srv, ""), "http://site.test:") + "/"); !strings.Contains(out, "web_fetch never reaches") {
		t.Fatalf("a private address was fetched: %s", out)
	}
	if out := fetch("http://other.test/"); !strings.Contains(out, "egress rules") {
		t.Fatalf("a host no rule allows: %s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("hits %d", hits)
	}
	want := []string{"allow", "deny", "deny"}
	if len(got) != len(want) {
		t.Fatalf("records: %v", got)
	}
	for i, e := range got {
		if e["decision"] != want[i] || e["kind"] != egress.KindWebFetch || e["call_id"] != "call-f" || e["session"] != "s1" {
			t.Errorf("record %d: %v", i, e)
		}
	}
	if got[1]["rule"] != egress.KindWebFetch {
		t.Errorf("the private address was refused by %v, not web_fetch's own check", got[1]["rule"])
	}
}
