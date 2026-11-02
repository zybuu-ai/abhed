package ollama

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress/upstreamtest"
)

// Ollama's registry and download requests go out through the upstream
// proxy, and a server Abhed starts is given it when it needs no credentials.
func TestOllamaUsesTheUpstreamProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	corp := upstreamtest.Start(t)
	corp.Use(t, "", "")
	resp, err := (Env{}).web().Get("http://registry.ollama.test:" + u.Port() + "/v2/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !corp.Saw("registry.ollama.test") {
		t.Fatal("the registry request did not go through the upstream proxy")
	}
	env := strings.Join((Env{}).serveEnv("127.0.0.1:11434"), " ")
	if !strings.Contains(env, "HTTPS_PROXY=http://127.0.0.1:") || !strings.Contains(env, "NO_PROXY=localhost") {
		t.Fatalf("serve env %s", env)
	}
	corp.Use(t, "svc", "pw")
	if env := strings.Join((Env{}).serveEnv("127.0.0.1:11434"), " "); strings.Contains(env, "PROXY") {
		t.Fatalf("a proxy needing credentials was passed to the server: %s", env)
	}
}

// The API client of the default environment sends a remote server's calls
// through the upstream proxy, and a loopback one's directly.
func TestOllamaAPIFollowsTheUpstreamProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"0.9.0"}`))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	corp := upstreamtest.Start(t)
	corp.Use(t, "", "")
	c := DefaultEnv().client()
	for _, target := range []string{"http://ollama.corp.test:" + u.Port() + "/api/version", srv.URL + "/api/version"} {
		resp, err := c.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if !corp.Saw("ollama.corp.test") || corp.Saw("127.0.0.1") {
		t.Fatalf("the proxy saw the remote server %v, the loopback one %v", corp.Saw("ollama.corp.test"), corp.Saw("127.0.0.1"))
	}
}
