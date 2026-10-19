// Package testenv keeps a test binary off what is this machine's own: the
// developer's ~/.abhed, the managed files under /etc/abhed, the variables
// that choose a model or point at a file of secrets, and the model services
// a developer runs on loopback (Ollama on 11434, a LiteLLM proxy on 4000).
//
// Only tests import it. A package whose tests load a configuration or build
// a session calls Main from its TestMain.
package testenv

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/defaults"
	"github.com/zybuu-ai/abhed/internal/managed"
)

// Marker is set in the environment of an isolated test binary, so a helper
// process it runs keeps the home and variables its parent test prepared.
const Marker = "ABHED_TESTENV"

// scrubbed are variables that choose a model, a key, or a file Abhed reads
// in place of the home folder's. A test that needs one sets its own.
var scrubbed = []string{
	"ABHED_BASE_URL", "ABHED_MODEL", "ABHED_API_KEY", "ABHED_SECRETS_FILE", "ABHED_USERS_FILE",
	"ABHED_PROFILES_FILE", "ABHED_DATABASE_URL", "ABHED_MIGRATE_DATABASE_URL", "ABHED_TRUST_WORKSPACE",
	"ABHED_ANTHROPIC_OAUTH_TOKEN",
	"OLLAMA_HOST", "OPENAI_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL",
	"GEMINI_API_KEY", "GOOGLE_API_KEY", "WATSONX_APIKEY", "WATSONX_URL",
}

// realPorts are the loopback ports of model services a developer runs.
var realPorts = []int{11434, 4000}

// Guard records what reached a default or real model endpoint.
type Guard struct {
	// OrigHome is the home the binary started with, for a test that must
	// reach the host's container engine through its connection there.
	OrigHome string
	// Helper is set in a helper process of an isolated test binary.
	Helper bool
	mu     sync.Mutex
	hits   []string
	srv    *httptest.Server
	home   string
}

func (g *Guard) note(s string) {
	g.mu.Lock()
	g.hits = append(g.hits, s)
	g.mu.Unlock()
}

// Hits are what reached a model endpoint no test may use.
func (g *Guard) Hits() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.hits)
}

// Isolate points the managed files at a folder that does not exist, the
// default model endpoints at a stub that answers every call with an error
// and notes it, and refuses a dial to a real local model service made
// through http.DefaultTransport. A client with a transport of its own, such
// as egress's for Abhed's own requests, is not guarded; the stub endpoints
// keep the defaults off a real service. Unless
// this is a helper process of an isolated parent, it also gives the binary
// an empty home and clears the variables in scrubbed. Close undoes nothing
// a test relies on: it stops the stub and removes the home.
func Isolate() *Guard {
	g := &Guard{OrigHome: os.Getenv("HOME")}
	none := filepath.Join(os.TempDir(), fmt.Sprintf("abhed-no-managed-%d", os.Getpid()))
	managed.ConfigFile, managed.AgentsDir = filepath.Join(none, "config.json"), filepath.Join(none, "agents")

	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.note("the default model endpoint: " + r.Method + " " + r.URL.Path)
		http.Error(w, `{"error":{"message":"a test reached the default model endpoint; give it a stub"}}`, http.StatusServiceUnavailable)
	}))
	defaults.LocalModelURL = g.srv.URL + "/v1"
	defaults.OllamaURL = g.srv.URL + "/v1"

	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		dial := t.DialContext
		if dial == nil {
			dial = (&net.Dialer{}).DialContext
		}
		t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if Real(addr) {
				g.note("a real model service at " + addr)
				return nil, fmt.Errorf("testenv: %s is a real local model service; tests use a stub", addr)
			}
			return dial(ctx, network, addr)
		}
	}

	if os.Getenv(Marker) != "" {
		g.Helper = true
		return g // a helper keeps what its parent test set
	}
	keepGoCaches()
	home, err := os.MkdirTemp("", "abhed-home-*")
	if err != nil {
		panic(err)
	}
	g.home = home
	for k, v := range map[string]string{"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_STATE_HOME": filepath.Join(home, ".local", "state"), Marker: "1"} {
		_ = os.Setenv(k, v)
	}
	for _, k := range scrubbed {
		_ = os.Unsetenv(k)
	}
	return g
}

// keepGoCaches pins the Go caches where they are before the home moves, so
// a test that builds a binary does not start from an empty cache.
func keepGoCaches() {
	if os.Getenv("GOENV") == "" {
		if d, err := os.UserConfigDir(); err == nil {
			_ = os.Setenv("GOENV", filepath.Join(d, "go", "env"))
		}
	}
	if os.Getenv("GOCACHE") == "" {
		if d, err := os.UserCacheDir(); err == nil {
			_ = os.Setenv("GOCACHE", filepath.Join(d, "go-build"))
		}
	}
	if os.Getenv("GOPATH") == "" {
		if h, err := os.UserHomeDir(); err == nil {
			_ = os.Setenv("GOPATH", filepath.Join(h, "go"))
		}
	}
}

// Real reports whether addr is a loopback address on a real model
// service's port.
func Real(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	p, err := strconv.Atoi(port)
	if err != nil || !slices.Contains(realPorts, p) {
		return false
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// Close stops the stub and removes the home, and returns code, or 1 when
// anything reached a model endpoint no test may use.
func (g *Guard) Close(code int) int {
	g.srv.Close()
	if g.home != "" {
		_ = os.RemoveAll(g.home)
	}
	if hits := g.Hits(); len(hits) > 0 {
		fmt.Fprintf(os.Stderr, "testenv: tests reached a model endpoint they may not use:\n  %s\n", strings.Join(hits, "\n  "))
		return 1
	}
	return code
}

// Main is a TestMain: Isolate, run the tests, Close.
func Main(m *testing.M) {
	g := Isolate()
	os.Exit(g.Close(m.Run()))
}
