package webfetch

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress/upstreamtest"
)

// Outside the allowlist web_fetch goes out through the upstream proxy, its
// own dialer reaching the proxy as the operator named it; a name that
// resolves here to an internal address is still refused, before the proxy.
func TestWebFetchThroughTheUpstreamProxy(t *testing.T) {
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "via the corporate proxy")
	}))
	corp := upstreamtest.Start(t)
	corp.Use(t, "svc", "pw")
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	res := run(t, tool, "http://public.example.test:"+port+"/p")
	if res.IsError || !strings.Contains(res.Content, "via the corporate proxy") || !corp.Saw("public.example.test") {
		t.Fatalf("through the proxy: %s", res.Content)
	}
	res = run(t, tool, "http://internal.test:"+port+"/")
	if !res.IsError || corp.Saw("internal.test") {
		t.Fatalf("an internal name through the proxy: %s", res.Content)
	}
}
