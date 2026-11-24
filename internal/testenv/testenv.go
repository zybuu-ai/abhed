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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/defaults"
	"github.com/zybuu-ai/abhed/internal/managed"
)

// Marker is set in the environment of an isolated test binary, so a helper
// process it runs keeps the home and variables its parent test prepared.
const Marker = "ABHED_TESTENV"

// privateEnv names, for a helper, the parent's private folder, so the helper
// points its managed paths there and makes no folder of its own to leave
// behind: a helper usually exits from inside a test, before any Close.
const privateEnv = "ABHED_TESTENV_PRIVATE"

// fallbackAddr is where the defaults lead when no stub can listen, as in a
// helper running in a sandbox with no network.
const fallbackAddr = "127.0.0.1:9"

// privatePrefix and homeName name the private folder Isolate makes and the
// home inside it.
const (
	privatePrefix = "abhed-no-managed-"
	homeName      = "home"
)

// parentOfHome reports the private folder a home Isolate made lies in, when
// home is one, resolved and checked.
func parentOfHome(home string) (string, bool) {
	if home == "" || filepath.Base(home) != homeName || !strings.HasPrefix(filepath.Base(filepath.Dir(home)), privatePrefix) {
		return "", false
	}
	p, err := checkPrivate(filepath.Dir(home))
	return p, err == nil
}

// scrubbed are variables that choose a model, a key, or a file Abhed reads
// in place of the home folder's. A test that needs one sets its own.
var scrubbed = []string{
	"ABHED_BASE_URL", "ABHED_MODEL", "ABHED_API_KEY", "ABHED_SECRETS_FILE", "ABHED_USERS_FILE",
	"ABHED_PROFILES_FILE", "ABHED_DATABASE_URL", "ABHED_MIGRATE_DATABASE_URL", "ABHED_TRUST_WORKSPACE",
	"ABHED_ANTHROPIC_OAUTH_TOKEN",
	// The developer's own clusters and SSH keys: a test gives its own.
	"KUBECONFIG", "SSH_AUTH_SOCK", "ABHED_K8S_TOKEN",
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
	// private holds the managed paths' missing folder and the home; only
	// this process can make anything in it. resolved is its resolved path.
	private, resolved string
	// fallback is set when no stub could listen and the defaults lead to
	// fallbackAddr; a dial there is then a hit. Tests use that closed port
	// for an endpoint that refuses, so it is a hit only then.
	fallback atomic.Bool
}

func (g *Guard) note(s string) {
	g.mu.Lock()
	g.hits = append(g.hits, s)
	g.mu.Unlock()
}

// judged counts judge's calls, so a test can see the dialer's Control run.
var judged atomic.Int64

// judge refuses, and notes, a dial to a real model service on this machine,
// or to the fallback while it is in force.
func (g *Guard) judge(addr string) error {
	judged.Add(1)
	if addr == fallbackAddr && g.fallback.Load() {
		g.note("the default model endpoint, at " + addr + " (no stub could listen)")
		return fmt.Errorf("testenv: %s is the default model endpoint; tests use a stub", addr)
	}
	if Real(addr) {
		g.note("a real model service at " + addr)
		return fmt.Errorf("testenv: %s is a real local model service; tests use a stub", addr)
	}
	return nil
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
	// The managed paths lead to a missing folder inside a 0700 folder of
	// this user's own, which no other user can change or swap (checkPrivate),
	// so none can make the missing folder and plant a managed file. A helper
	// reuses the folder its parent names, resolved and checked, and makes
	// nothing to leave behind when it exits from inside a test; anything else
	// makes one, which Close removes. A folder of its own that fails the
	// check stops the run.
	helper := os.Getenv(Marker) != ""
	parent, perr := checkPrivate(os.Getenv(privateEnv))
	if !helper || perr != nil {
		// A helper a sandbox runs keeps HOME but not this package's variables;
		// its HOME is then the home inside its parent's private folder.
		if p, ok := parentOfHome(os.Getenv("HOME")); ok {
			parent, perr, helper = p, nil, true
		}
	}
	if helper && perr == nil {
		managed.MoveTo(filepath.Join(parent, "none"))
	} else {
		private, err := os.MkdirTemp("", privatePrefix)
		if err != nil {
			panic("testenv: " + err.Error())
		}
		g.private = private
		if ownFolderChecked {
			if private, err = checkPrivate(private); err != nil {
				_ = os.RemoveAll(g.private)
				panic("testenv: the temporary folder is not private: " + err.Error())
			}
		}
		managed.MoveTo(filepath.Join(private, "none"))
		_ = os.Setenv(privateEnv, private)
		g.resolved = private
	}

	// A helper a test runs in a sandbox with no network cannot listen; its
	// defaults then lead to a closed port, and a dial there is noted like a
	// call to the stub.
	stub := "http://" + fallbackAddr
	if ln, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
		g.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g.note("the default model endpoint: " + r.Method + " " + r.URL.Path)
			http.Error(w, `{"error":{"message":"a test reached the default model endpoint; give it a stub"}}`, http.StatusServiceUnavailable)
		}))
		_ = g.srv.Listener.Close()
		g.srv.Listener = ln
		g.srv.Start()
		stub = g.srv.URL
	} else {
		g.fallback.Store(true)
	}
	defaults.LocalModelURL = stub + "/v1"
	defaults.OllamaURL = stub + "/v1"

	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		// The address is judged as written and again once resolved, in the
		// dialer's Control, so a name that resolves to this machine (an
		// /etc/hosts alias, a public name with a loopback record) is caught.
		// The timeouts are http.DefaultTransport's own.
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second,
			Control: func(network, address string, _ syscall.RawConn) error { return g.judge(address) }}
		t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if err := g.judge(addr); err != nil {
				return nil, err
			}
			return d.DialContext(ctx, network, addr)
		}
	}

	if helper {
		g.Helper = true
		return g // a helper keeps what its parent test set
	}
	keepGoCaches()
	// The home is inside the private folder, so Close removes both, and a
	// helper that exits from inside a test makes no home of its own.
	home := filepath.Join(g.resolved, homeName)
	if err := os.Mkdir(home, 0o700); err != nil {
		panic("testenv: " + err.Error())
	}
	g.home = home
	// GIT_CONFIG_NOSYSTEM: this machine's /etc/gitconfig (its hooks, its
	// helpers) is not the test's; a git the tests run reads only their own.
	for k, v := range map[string]string{"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_STATE_HOME": filepath.Join(home, ".local", "state"), Marker: "1", "GIT_CONFIG_NOSYSTEM": "1"} {
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
	return Local(host)
}

// Local reports whether host names this machine: localhost, a loopback or
// unspecified address, in any of the spellings a resolver or the C library
// takes for one (a trailing dot, the short IPv4 forms such as 127.1).
func Local(host string) bool {
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	// An empty host, as in ":11434", is this machine to Go's dialer.
	if host == "" || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ip = shortIPv4(host)
	}
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// shortIPv4 reads the inet_aton forms Go's parser refuses: one to four
// parts, each decimal, 0x hex or 0 octal, the last filling the remaining
// bytes (127.1 is 127.0.0.1). nil if s is not one. It is deliberately
// broader than libc: strconv also takes 0o, 0b and _ forms, which only
// makes the guard refuse more.
func shortIPv4(s string) net.IP {
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return nil
	}
	var v []uint64
	for _, p := range parts {
		n, err := strconv.ParseUint(p, 0, 32)
		if err != nil {
			return nil
		}
		v = append(v, n)
	}
	var out uint64
	for i, n := range v[:len(v)-1] {
		if n > 255 {
			return nil
		}
		out |= n << (24 - 8*uint(i))
	}
	last, room := v[len(v)-1], uint(32-8*(len(v)-1))
	if room < 64 && last >= 1<<room {
		return nil
	}
	out |= last
	return net.IPv4(byte(out>>24), byte(out>>16), byte(out>>8), byte(out)) // #nosec G115 -- each byte of an IPv4 address, masked by the shift
}

// Close stops the stub and removes the home, and returns code, or 1 when
// anything reached a model endpoint no test may use.
func (g *Guard) Close(code int) int {
	if g.srv != nil {
		g.srv.Close()
	}
	if g.private != "" {
		_ = os.RemoveAll(g.private)
	}
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
