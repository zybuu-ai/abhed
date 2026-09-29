package server

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
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
	"github.com/zybuu-ai/abhed/internal/k8s"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// promptAdapter makes one call chosen by the session's prompt, then answers.
type promptAdapter struct{ calls map[string]model.ToolCall }

func (promptAdapter) Name() string { return "prompt" }
func (promptAdapter) Profile() model.Profile {
	return model.Profile{Name: "prompt", ContextWindow: 32000}
}
func (promptAdapter) CountTokens(model.Request) (int, error) { return 10, nil }
func (a promptAdapter) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 3)
	defer close(ch)
	last := req.Messages[len(req.Messages)-1]
	if c, ok := a.calls[last.Content]; ok && last.Role == model.RoleUser {
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &c}
	} else {
		ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	return ch, nil
}

// noRedact records payloads as they are, so a value kept out of the record
// is kept out by the tools, not by the store happening to hold it.
type noRedact struct{}

func (noRedact) Redact(b []byte) []byte { return b }
func (noRedact) Span() int              { return 0 }

// Two people's sessions on one server share one set of tools. Alice's
// k8s_login must not become the credential Bob's k8s_get uses, and her token
// must not reach either record.
func TestClusterLoginDoesNotCrossSessions(t *testing.T) {
	const aliceToken = "tok-alice-7e21-cluster"
	var mu sync.Mutex
	var seen []string
	cluster := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+aliceToken {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"kind":"Status","message":"Unauthorized"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/version") {
			fmt.Fprint(w, `{"major":"1","minor":"29"}`)
			return
		}
		fmt.Fprint(w, `{"kind":"NodeList","items":[{"metadata":{"name":"alice-only-node"}}]}`)
	}))
	defer cluster.Close()

	// The operator's own kubeconfig, with a token the cluster refuses.
	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte(fmt.Sprintf(`apiVersion: v1
clusters:
- cluster: {server: %s, insecure-skip-tls-verify: true}
  name: c
contexts:
- context: {cluster: c, user: u}
  name: ctx
current-context: ctx
users:
- name: u
  user: {token: operator-token}
`, cluster.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cluster.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := k8s.NewManager(k8s.Config{Kubeconfig: kubeconfig, CAFile: ca,
		Clusters: []k8s.LoginCluster{{Name: "prod", Server: cluster.URL}}})
	secret := func(name string) (string, error) {
		if name == "ALICE_TOKEN" {
			return aliceToken, nil
		}
		return "", fmt.Errorf("no secret named %s", name)
	}
	reg := tools.NewRegistry(k8s.GetTool{M: mgr}, k8s.LoginTool{M: mgr, Secret: secret})

	loginArgs, _ := json.Marshal(map[string]string{"cluster": "prod", "token_secret": "ALICE_TOKEN"})
	adapter := promptAdapter{calls: map[string]model.ToolCall{
		"log in": {ID: "l1", Name: "k8s_login", Args: loginArgs},
		"nodes":  {ID: "g1", Name: "k8s_get", Args: json.RawMessage(`{"resource":"nodes"}`)},
	}}
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	cfg.Permissions.Mode = "bypass"
	cfg.Permissions.Allow = []string{"secret(ALICE_TOKEN)"}
	st := agent.NewMemStore()
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: adapter,
		Registry: reg, Store: st, Redact: noRedact{}})
	h := s.Handler()

	run := func(user, prompt string) string {
		r := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"`+prompt+`"}`))
		r.Header.Set("X-Abhed-User", user)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out struct {
			SessionID string `json:"session_id"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if out.SessionID == "" {
			t.Fatalf("no session for %s: %s", user, w.Body.String())
		}
		for deadline := time.Now().Add(5 * time.Second); ; {
			evs, _ := st.Events(out.SessionID)
			observed := false
			for _, e := range evs {
				observed = observed || e.Type == agent.EvObservation
			}
			if observed || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		return out.SessionID
	}
	observation := func(id string) string {
		evs, _ := st.Events(id)
		for _, e := range evs {
			if e.Type == agent.EvObservation {
				return string(e.Payload)
			}
		}
		t.Fatalf("session %s made no call", id)
		return ""
	}

	alice := run("alice", "log in")
	if got := observation(alice); !strings.Contains(got, "Authenticated to") {
		t.Fatalf("alice could not log in: %s", got)
	}
	mu.Lock()
	before := len(seen)
	mu.Unlock()

	bob := run("bob", "nodes")
	if got := observation(bob); strings.Contains(got, "alice-only-node") {
		t.Fatalf("bob's k8s_get read the cluster with alice's login: %s", got)
	}
	mu.Lock()
	for _, line := range seen[before:] {
		if strings.Contains(line, aliceToken) {
			t.Errorf("bob's call sent alice's token: %s", line)
		}
	}
	mu.Unlock()

	for _, id := range []string{alice, bob} {
		evs, _ := st.Events(id)
		for _, e := range evs {
			if strings.Contains(string(e.Payload), aliceToken) {
				t.Errorf("alice's token reached the record in %s: %s", e.Type, e.Payload)
			}
		}
	}
}

// countClose counts its closes.
type countClose struct{ n *atomic.Int32 }

func (c countClose) Close() error { c.n.Add(1); return nil }

// Deleting a session releases what its tools kept for it: a login's
// connections and a connected host are closed, not left for the process.
func TestDeletingASessionClosesItsScopedState(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	st := agent.NewMemStore()
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Store: st, Redact: noRedact{}})
	h := s.Handler()
	do := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Abhed-User", "alice")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(do("POST", "/v1/sessions", `{"prompt":"hi"}`).Body.Bytes(), &out)
	if out.SessionID == "" {
		t.Fatal("no session")
	}
	s.mu.RLock()
	live := s.running[out.SessionID]
	s.mu.RUnlock()
	if live == nil {
		t.Fatal("the session is not running")
	}
	var closed atomic.Int32
	type key struct{}
	live.Loop.Session.Scoped(key{}, func() any { return countClose{&closed} })

	if w := do("DELETE", "/v1/sessions/"+out.SessionID, ""); w.Code >= 300 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if closed.Load() != 1 {
		t.Fatalf("the session's scoped state was closed %d times on delete, want 1", closed.Load())
	}
}
