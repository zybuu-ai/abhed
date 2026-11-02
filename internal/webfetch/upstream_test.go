package webfetch

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress/upstreamtest"
)

// Outside the allowlist web_fetch goes out through the upstream proxy, its
// own dialer reaching the proxy as the operator named it. A name that does
// not resolve here is refused by default; with resolve_via_proxy it is sent
// only when allowed_hosts names it exactly. A name that resolves here to an
// internal address is refused before the proxy.
func TestWebFetchThroughTheUpstreamProxy(t *testing.T) {
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "via the corporate proxy")
	}))
	corp := upstreamtest.Start(t)
	up := corp.Use(t, "svc", "pw")
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	page := "http://public.example.test:" + port + "/p"
	if res := run(t, tool, page); !res.IsError || corp.Saw("public.example.test") {
		t.Fatalf("an unresolved name with the switch off: %s", res.Content)
	}
	up.ResolveViaProxy = true
	if res := run(t, tool, page); !res.IsError || corp.Saw("public.example.test") {
		t.Fatalf("an unresolved name with no allowed_hosts: %s", res.Content)
	}
	tool.AllowedHosts = []string{"*.example.test"}
	if res := run(t, tool, page); !res.IsError || corp.Saw("public.example.test") {
		t.Fatalf("an unresolved name allowed only by a wildcard: %s", res.Content)
	}
	tool.AllowedHosts = []string{"public.example.test", "internal.test"}
	res := run(t, tool, page)
	if res.IsError || !strings.Contains(res.Content, "via the corporate proxy") || !corp.Saw("public.example.test") {
		t.Fatalf("through the proxy: %s", res.Content)
	}
	res = run(t, tool, "http://internal.test:"+port+"/")
	if !res.IsError || corp.Saw("internal.test") {
		t.Fatalf("an internal name through the proxy: %s", res.Content)
	}
}
