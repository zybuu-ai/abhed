package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ollama"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// fakeOllama is the host the server's assist sees: an httptest Ollama API,
// a registry, an ollama program that is only a file, and a Launch that runs
// nothing and brings the fake API up. No test starts a real Ollama.
type fakeOllama struct {
	api, reg *httptest.Server
	// url is where the API listens once it is up: nothing answers there
	// before, so a check finds the connection refused, as with no server.
	url      string
	once     sync.Once
	up       atomic.Bool
	mu       sync.Mutex
	pulled   []string
	launched []string
}

func newFakeOllama(t *testing.T) *fakeOllama {
	t.Helper()
	f := &fakeOllama{}
	f.api = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"0.40.10"}`)
		case "/api/tags":
			f.mu.Lock()
			pulled := append([]string(nil), f.pulled...)
			f.mu.Unlock()
			var ms []map[string]string
			for _, m := range pulled {
				ms = append(ms, map[string]string{"name": m})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": ms})
		case "/api/pull":
			var b struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			fmt.Fprintln(w, `{"status":"downloading","total":100,"completed":40}`)
			fmt.Fprintln(w, `{"status":"success"}`)
			f.mu.Lock()
			f.pulled = append(f.pulled, b.Model)
			f.mu.Unlock()
		}
	}))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.url = "http://" + l.Addr().String()
	_ = l.Close()
	t.Cleanup(func() {
		if f.up.Load() {
			f.api.Close()
		}
	})
	f.reg = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"config":{"size":10},"layers":[{"size":5000}]}`)
	}))
	t.Cleanup(f.reg.Close)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ollama"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	env := ollama.Env{
		GOOS: "linux",
		LookPath: func(name string) (string, error) {
			if name == "ollama" {
				return filepath.Join(bin, "ollama"), nil
			}
			return "", fmt.Errorf("%s: not found", name)
		},
		HTTP: &http.Client{}, Web: &http.Client{}, Registry: f.reg.URL,
		LogDir: filepath.Join(home, "logs"), StateDir: filepath.Join(home, "state"), ReadyWait: 2 * time.Second,
		MemTotal: func() (uint64, error) { return 64 << 30, nil }, DiskFree: func(string) (uint64, error) { return 500 << 30, nil },
		ModelsDir: home, Environ: func() []string { return nil },
		Launch: func(path string, args, env []string, _ *os.File) (int, <-chan error, error) {
			f.mu.Lock()
			f.launched = append(f.launched, strings.Join(append(append([]string{}, env...), append([]string{path}, args...)...), " "))
			f.mu.Unlock()
			f.start(t)
			return 4242, nil, nil
		},
		Listening: func(context.Context, string) bool { return false },
	}
	old := ollamaEnv.Get()
	ollamaEnv.Set(func() ollama.Env { return env })
	t.Cleanup(func() { ollamaEnv.Set(old) })
	return f
}

// start brings the API up at its address.
func (f *fakeOllama) start(t *testing.T) {
	f.once.Do(func() {
		l, err := net.Listen("tcp", strings.TrimPrefix(f.url, "http://"))
		if err != nil {
			t.Errorf("the fake API's port was taken: %v", err)
			return
		}
		f.api.Listener = l
		f.api.Start()
		f.up.Store(true)
	})
}

// ollamaServer serves provider "a" by default and "olm", which the fake
// Ollama serves. auth is "" (nobody signs in) or "proxy", where the
// X-Abhed-User and X-Abhed-Groups headers name the caller.
func ollamaServer(t *testing.T, assist, auth string, m *Metrics) (*Server, *durableMem, *fakeOllama) {
	t.Helper()
	f := newFakeOllama(t)
	a := newModelServer(t, "from a")
	cfg := config.Default()
	cfg.Suggest.Enabled = false
	cfg.Auth.Mode = auth
	cfg.Model.Default = "a"
	cfg.Model.Providers = map[string]config.ProviderConfig{
		"a":   {Type: "openai-compatible", BaseURL: a.url, Model: "model-a", ContextWindow: 8192},
		"olm": {Type: "ollama", BaseURL: f.url + "/v1", Model: "qwen3:8b", ContextWindow: 8192},
	}
	cfg.Model.Ollama.Assist = assist
	def, err := cfg.Model.Providers["a"].Adapter()
	if err != nil {
		t.Fatal(err)
	}
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}, orphaned: map[string]bool{}}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: def, Metrics: m,
		Registry: tools.NewRegistry(tools.Read{}, tools.Glob{}), Store: st})
	return s, st, f
}

// who is a caller: where from, and as whom under proxy auth.
type who struct{ remote, user, groups string }

var (
	loopbackNobody = who{remote: "127.0.0.1:50000"}
	admin          = who{remote: "127.0.0.1:50000", user: "alice", groups: DefaultAdminGroup}
	loopbackBob    = who{remote: "127.0.0.1:50000", user: "bob"}
)

func callWho(t *testing.T, s *Server, c who, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = c.remote
	if c.user != "" {
		r.Header.Set("X-Abhed-User", c.user)
	}
	if c.groups != "" {
		r.Header.Set("X-Abhed-Groups", c.groups)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestOllamaProvidersAreMarked(t *testing.T) {
	s, _, _ := ollamaServer(t, "", "", nil)
	var list []providerInfo
	_ = json.Unmarshal(call(t, s, "GET", "/v1/providers", "").Body.Bytes(), &list)
	marked := map[string]bool{}
	for _, p := range list {
		marked[p.Name] = p.Ollama
	}
	if !marked["olm"] || marked["a"] {
		t.Fatalf("providers %+v", list)
	}
}

// refusedEverywhere checks a caller is shown the steps and nothing runs.
func refusedEverywhere(t *testing.T, s *Server, f *fakeOllama, c who) {
	t.Helper()
	w := callWho(t, s, c, "GET", "/v1/ollama/status?provider=olm", "")
	var rep ollama.Report
	_ = json.Unmarshal(w.Body.Bytes(), &rep)
	if w.Code != http.StatusOK || rep.CanAct || rep.Refusal == nil || rep.Refusal.Kind != ollama.KindNotAdmin ||
		!strings.Contains(strings.Join(rep.Refusal.Steps, "\n"), "abhed ollama check olm") || rep.Problem == nil || len(rep.Problem.Steps) == 0 {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"binary"`) || rep.Binary != "" || rep.Version != "" {
		t.Fatalf("the host's paths shown to a caller who may not act: %s", w.Body.String())
	}
	for _, path := range []string{"/v1/ollama/start", "/v1/ollama/install", "/v1/ollama/pull"} {
		if w := callWho(t, s, c, "POST", path, `{"provider":"olm","always":true}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"kind":"not_admin"`) || strings.Contains(w.Body.String(), `"binary"`) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if w := callWho(t, s, c, "POST", "/v1/ollama/forget", `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("forget: %d", w.Code)
	}
	if len(f.launched) != 0 {
		t.Fatalf("launched %v", f.launched)
	}
	if _, err := os.Stat(filepath.Join(ollamaEnv.Get()().StateDir, "ollama.json")); err == nil {
		t.Fatal("a refused caller wrote the always-start choice")
	}
}

// With nobody signing in, a request from loopback acts for no one: it could
// be the agent's own command, a rebound page or a proxy on the host.
func TestOllamaUnauthenticatedLoopbackIsRefused(t *testing.T) {
	s, _, f := ollamaServer(t, "", "", nil)
	refusedEverywhere(t, s, f, loopbackNobody)
}

// A signed-in caller who is not an administrator is refused, from loopback too.
func TestOllamaSignedInNonAdminIsRefused(t *testing.T) {
	s, _, f := ollamaServer(t, "", "proxy", nil)
	refusedEverywhere(t, s, f, loopbackBob)
}

func TestOllamaAdministratorActs(t *testing.T) {
	s, _, f := ollamaServer(t, "", "proxy", nil)
	w := callWho(t, s, admin, "GET", "/v1/ollama/status?provider=olm", "")
	var rep ollama.Report
	_ = json.Unmarshal(w.Body.Bytes(), &rep)
	if !rep.CanAct || rep.Start == nil || rep.Next != "start" || rep.Binary == "" {
		t.Fatalf("status for an administrator: %s", w.Body.String())
	}
	w = callWho(t, s, admin, "POST", "/v1/ollama/start", `{"provider":"olm"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"version":"0.40.10"`) {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	if len(f.launched) != 1 || !strings.Contains(f.launched[0], "OLLAMA_HOST=127.0.0.1:") || !strings.HasSuffix(f.launched[0], "ollama serve") {
		t.Fatalf("launched %v", f.launched)
	}
}

func TestOllamaAssistOffActsOnNothing(t *testing.T) {
	s, _, f := ollamaServer(t, "off", "proxy", nil)
	for _, path := range []string{"/v1/ollama/start", "/v1/ollama/pull"} {
		if w := callWho(t, s, admin, "POST", path, `{"provider":"olm"}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"kind":"assist_off"`) {
			t.Fatalf("%s with assist off: %d %s", path, w.Code, w.Body.String())
		}
	}
	if len(f.launched) != 0 {
		t.Fatalf("launched %v", f.launched)
	}
	if w := callWho(t, s, admin, "POST", "/v1/ollama/start", `{"provider":"a"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a provider Ollama does not serve: %d", w.Code)
	}
	if w := callWho(t, s, admin, "POST", "/v1/ollama/start", `{"provider":"nope"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown provider: %d", w.Code)
	}
}

// adminSession starts a session as the administrator and waits for its turn.
func adminSession(t *testing.T, s *Server, st *durableMem) string {
	t.Helper()
	id := sessionOf(t, callWho(t, s, admin, "POST", "/v1/sessions", `{"prompt":"one"}`))
	turnsEnded(t, st, id, 1)
	return id
}

// A pull streams its progress, and the attempt is in the session's record.
func TestOllamaPullStreamsAndIsRecorded(t *testing.T) {
	s, st, f := ollamaServer(t, "ask", "proxy", nil)
	id := adminSession(t, s, st)
	f.start(t)
	w := callWho(t, s, admin, "POST", "/v1/ollama/pull", `{"provider":"olm","session_id":"`+id+`"}`)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("pull: %d %s", w.Code, w.Body.String())
	}
	var lines []map[string]json.RawMessage
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	for sc.Scan() {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(sc.Bytes(), &m)
		lines = append(lines, m)
	}
	if len(lines) < 2 || lines[0]["progress"] == nil || lines[len(lines)-1]["status"] == nil || !strings.Contains(string(lines[len(lines)-1]["status"]), `"ready":true`) {
		t.Fatalf("pull lines %s", w.Body.String())
	}
	events, _ := st.Events(id)
	var got []agent.OllamaAction
	for _, ev := range events {
		if ev.Type == agent.EvOllamaPull {
			var a agent.OllamaAction
			_ = json.Unmarshal(ev.Payload, &a)
			got = append(got, a)
			if ev.Actor != agent.ActorUser {
				t.Errorf("recorded as %s", ev.Actor)
			}
		}
	}
	if len(got) != 1 || got[0].Outcome != agent.OllamaOK || got[0].Surface != "server" || got[0].Bytes != 5010 || got[0].Model != "qwen3:8b" {
		t.Fatalf("recorded %+v", got)
	}
}

func ollamaCount(t *testing.T, m *Metrics, op, outcome string) string {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	prefix := `abhed_ollama_actions_total{op="` + op + `",outcome="` + outcome + `"} `
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return "0"
}

// Every attempt the server handles is counted once: one with no session
// here, one recorded in a session as its event is written.
func TestOllamaAttemptsAreCountedOnce(t *testing.T) {
	m := NewMetrics()
	s, st, f := ollamaServer(t, "", "proxy", m)
	if w := callWho(t, s, loopbackBob, "POST", "/v1/ollama/start", `{"provider":"olm"}`); w.Code != http.StatusForbidden {
		t.Fatalf("start by a non-administrator: %d", w.Code)
	}
	if got := ollamaCount(t, m, "start", "refused"); got != "1" {
		t.Fatalf("a refused start with no session counted %s", got)
	}
	if w := callWho(t, s, admin, "POST", "/v1/ollama/start", `{"provider":"olm"}`); w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	if got := ollamaCount(t, m, "start", "ok"); got != "1" {
		t.Fatalf("a start with no session counted %s", got)
	}
	id := adminSession(t, s, st)
	if w := callWho(t, s, admin, "POST", "/v1/ollama/pull", `{"provider":"olm","session_id":"`+id+`"}`); w.Code != http.StatusOK {
		t.Fatalf("pull: %d %s", w.Code, w.Body.String())
	}
	if got := ollamaCount(t, m, "pull", "ok"); got != "1" {
		t.Fatalf("a pull recorded in a session counted %s", got)
	}
	evs, _ := st.Events(id)
	n := 0
	for _, ev := range evs {
		if ev.Type == agent.EvOllamaPull {
			n++
		}
	}
	if n != 1 || len(f.launched) != 1 {
		t.Fatalf("pull events %d, launched %v", n, f.launched)
	}
}
