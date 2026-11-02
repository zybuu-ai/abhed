package toolchain

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress/upstreamtest"
)

// Toolchain downloads go out through the configured upstream proxy.
func TestSourceUsesTheUpstreamProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "dist") }))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	corp := upstreamtest.Start(t)
	corp.Use(t, "", "")
	resp, err := (&Source{}).client(nil).Get("http://dist.example.test:" + u.Port() + "/index.json")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "dist" || !corp.Saw("dist.example.test") {
		t.Fatalf("not through the upstream proxy: %q", b)
	}
}
