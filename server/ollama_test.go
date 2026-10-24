package server

import (
	"bufio"
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
		Launch: func(path string, args, env []string, _ *os.File) (int, error) {
			f.mu.Lock()
			f.launched = append(f.launched, strings.Join(append(append([]string{}, env...), append([]string{path}, args...)...), " "))
			f.mu.Unlock()
			f.start(t)
			return 4242, nil
		},
	}
	old := ollamaEnv
	ollamaEnv = func() ollama.Env { return env }
	t.Cleanup(func() { ollamaEnv = old })
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

// ollamaServer is switchServer with a provider "olm" that the fake Ollama serves.
func ollamaServer(t *testing.T, assist string) (*Server, *durableMem, *fakeOllama) {
	t.Helper()
	f := newFakeOllama(t)
	s, st, _, _ := switchServer(t)
	s.opts.Config.Model.Providers["olm"] = config.ProviderConfig{Type: "ollama", BaseURL: f.url + "/v1", Model: "qwen3:8b", ContextWindow: 8192}
	s.opts.Config.Model.Ollama.Assist = assist
	return s, st, f
}

// callFrom is call from a remote address: loopback is the host itself.
func callFrom(t *testing.T, s *Server, remote, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

const (
	fromHost  = "127.0.0.1:50000"
	fromOther = "192.0.2.7:50000"
)

func TestOllamaProvidersAreMarked(t *testing.T) {
	s, _, _ := ollamaServer(t, "")
	var list []providerInfo
	_ = json.Unmarshal(call(t, s, "GET", "/v1/providers", "").Body.Bytes(), &list)
	marked := map[string]bool{}
	for _, p := range list {
		marked[p.Name] = p.Ollama
	}
	if !marked["olm"] || marked["a"] || marked["b"] {
		t.Fatalf("providers %+v", list)
	}
}

// Only someone who may act on the host gets the offer; anyone else gets the
// message and the steps, and nothing runs.
func TestOllamaOnlyTheHostOrAnAdministratorActs(t *testing.T) {
	s, _, f := ollamaServer(t, "")
	var rep ollama.Report
	w := callFrom(t, s, fromOther, "GET", "/v1/ollama/status?provider=olm", "")
	_ = json.Unmarshal(w.Body.Bytes(), &rep)
	if w.Code != http.StatusOK || rep.CanAct || rep.Refusal == nil || rep.Refusal.Kind != ollama.KindNotAdmin || rep.Problem == nil || len(rep.Problem.Steps) == 0 {
		t.Fatalf("status for another machine: %d %s", w.Code, w.Body.String())
	}
	if w := callFrom(t, s, fromOther, "POST", "/v1/ollama/start", `{"provider":"olm"}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"kind":"not_admin"`) {
		t.Fatalf("start from another machine: %d %s", w.Code, w.Body.String())
	}
	if len(f.launched) != 0 {
		t.Fatalf("launched %v", f.launched)
	}
	if w := callFrom(t, s, fromOther, "POST", "/v1/ollama/forget", `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("forget from another machine: %d", w.Code)
	}

	w = callFrom(t, s, fromHost, "GET", "/v1/ollama/status?provider=olm", "")
	rep = ollama.Report{}
	_ = json.Unmarshal(w.Body.Bytes(), &rep)
	if !rep.CanAct || rep.Start == nil || rep.Next != "start" {
		t.Fatalf("status from the host: %s", w.Body.String())
	}
	w = callFrom(t, s, fromHost, "POST", "/v1/ollama/start", `{"provider":"olm"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"version":"0.40.10"`) {
		t.Fatalf("start from the host: %d %s", w.Code, w.Body.String())
	}
	if len(f.launched) != 1 || !strings.Contains(f.launched[0], "OLLAMA_HOST=127.0.0.1:") || !strings.HasSuffix(f.launched[0], "ollama serve") {
		t.Fatalf("launched %v", f.launched)
	}
}

func TestOllamaAssistOffActsOnNothing(t *testing.T) {
	s, _, f := ollamaServer(t, "off")
	for _, path := range []string{"/v1/ollama/start", "/v1/ollama/pull"} {
		if w := callFrom(t, s, fromHost, "POST", path, `{"provider":"olm"}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"kind":"assist_off"`) {
			t.Fatalf("%s with assist off: %d %s", path, w.Code, w.Body.String())
		}
	}
	if len(f.launched) != 0 {
		t.Fatalf("launched %v", f.launched)
	}
	if w := callFrom(t, s, fromHost, "POST", "/v1/ollama/start", `{"provider":"a"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a provider Ollama does not serve: %d", w.Code)
	}
	if w := callFrom(t, s, fromHost, "POST", "/v1/ollama/start", `{"provider":"nope"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown provider: %d", w.Code)
	}
}

// A pull streams its progress, and the attempts are in the session's record.
func TestOllamaPullStreamsAndIsRecorded(t *testing.T) {
	s, st, f := ollamaServer(t, "ask")
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"one"}`))
	turnsEnded(t, st, id, 1)
	f.start(t)
	w := callFrom(t, s, fromHost, "POST", "/v1/ollama/pull", `{"provider":"olm","session_id":"`+id+`"}`)
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
