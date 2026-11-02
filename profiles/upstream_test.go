package profiles

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress/upstreamtest"
)

// `abhed login` and the profile calls go out through the configured
// upstream proxy, with its credentials, in place of the environment's.
func TestProfileClientUsesTheUpstreamProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	corp := upstreamtest.Start(t)
	corp.Use(t, "svc", "pw")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	c, err := (&Profile{URL: "http://abhed.corp.test:" + u.Port()}).HTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(fmt.Sprintf("http://abhed.corp.test:%s/v1/whoami", u.Port()))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !corp.Saw("abhed.corp.test") || len(corp.Auth()) == 0 || corp.Auth()[0] == "" {
		t.Fatal("the profile client did not go through the upstream proxy with its credentials")
	}
}
