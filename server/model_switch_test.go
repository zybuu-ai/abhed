package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

// modelServer is a scripted OpenAI-compatible endpoint that counts the
// requests it answers and keeps their system prompts.
type modelServer struct {
	url    string
	calls  atomic.Int32
	mu     sync.Mutex
	system []string
	// hold, when set, is waited on before answering.
	hold chan struct{}
}

func newModelServer(t *testing.T, reply string) *modelServer {
	t.Helper()
	m := &modelServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		m.mu.Lock()
		if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
			m.system = append(m.system, req.Messages[0].Content)
		}
		hold := m.hold
		m.mu.Unlock()
		m.calls.Add(1)
		if hold != nil {
			<-hold
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", reply)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	m.url = srv.URL
	return m
}

// switchServer runs on provider "a" by default with "b" configured beside it,
// each a scripted endpoint, over a store with session rows as Postgres has.
func switchServer(t *testing.T, adjust ...func(*config.Config)) (*Server, *durableMem, *modelServer, *modelServer) {
	t.Helper()
	a, b := newModelServer(t, "from a"), newModelServer(t, "from b")
	cfg := config.Default()
	for _, f := range adjust {
		f(&cfg)
	}
	cfg.Model.Default = "a"
	cfg.Model.Providers = map[string]config.ProviderConfig{
		"a": {Type: "openai-compatible", BaseURL: a.url, Model: "model-a", ContextWindow: 8192},
		"b": {Type: "openai-compatible", BaseURL: b.url, Model: "model-b", ContextWindow: 16384},
	}
	def, err := cfg.Model.Providers["a"].Adapter()
	if err != nil {
		t.Fatal(err)
	}
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: def,
		Registry: tools.NewRegistry(tools.Read{}, tools.Glob{}), Store: st})
	return s, st, a, b
}

func call(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return callAs(t, s, "", "", method, path, body)
}

// callAs is call from a user in a tenant, as a trusted proxy names them.
func callAs(t *testing.T, s *Server, user, tenant, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if user != "" {
		r.Header.Set("X-Abhed-User", user)
		r.Header.Set("X-Abhed-Tenant", tenant)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func sessionOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.SessionID == "" {
		t.Fatalf("no session: %d %s", w.Code, w.Body.String())
	}
	return out.SessionID
}

// turnsEnded waits for the session's record to hold n session.ended events.
func turnsEnded(t *testing.T, st *durableMem, id string, n int) []agent.Event {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		events, _ := st.Events(id)
		ended := 0
		for _, ev := range events {
			if ev.Type == agent.EvSessionEnded {
				ended++
			}
		}
		if ended >= n {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session did not end %d turn(s): %d events", n, len(events))
		}
	}
}

// callModels is the model each model.call in events names.
func callModels(events []agent.Event) []string {
	var out []string
	for _, ev := range events {
		if ev.Type == agent.EvModelCall {
			var mc agent.ModelCall
			_ = json.Unmarshal(ev.Payload, &mc)
			out = append(out, mc.Model)
		}
	}
	return out
}

func forget(s *Server, id string) {
	s.mu.Lock()
	delete(s.running, id) // as a restart, or another node, would see it
	s.mu.Unlock()
}

// A switch on a live session makes the new model answer the next turn, and
// the record, the list and the reply all name it.
func TestSwitchedModelAnswersTheNextTurn(t *testing.T) {
	s, st, a, b := switchServer(t)
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"one"}`))
	turnsEnded(t, st, id, 1)

	w := call(t, s, "POST", "/v1/sessions/"+id+"/model", `{"provider":"b"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"model":"model-b"`) || !strings.Contains(w.Body.String(), `"from":"model-a"`) {
		t.Fatalf("switch: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"two"}`); w.Code != http.StatusAccepted {
		t.Fatalf("message: %d %s", w.Code, w.Body.String())
	}
	events := turnsEnded(t, st, id, 2)
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Fatalf("model-a answered %d calls and model-b %d; want 1 each", a.calls.Load(), b.calls.Load())
	}
	if got := strings.Join(callModels(events), ","); got != "model-a,model-b" {
		t.Fatalf("the record names the calls' models as %q", got)
	}
	if agent.ProviderOf(events) != "b" {
		t.Fatal("the switch is not in the record")
	}
	var list []sessionSummary
	_ = json.Unmarshal(call(t, s, "GET", "/v1/sessions", "").Body.Bytes(), &list)
	if len(list) != 1 || list[0].Model != "model-b" || list[0].Provider != "b" {
		t.Fatalf("the list shows %+v", list)
	}
}

// A session switched to another model keeps it when it is continued from its
// record: after a restart, or on another node.
func TestResumedSessionKeepsItsSwitchedModel(t *testing.T) {
	s, st, a, b := switchServer(t)
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"one"}`))
	turnsEnded(t, st, id, 1)
	if w := call(t, s, "POST", "/v1/sessions/"+id+"/model", `{"provider":"b"}`); w.Code != http.StatusOK {
		t.Fatalf("switch: %d %s", w.Code, w.Body.String())
	}
	forget(s, id)
	if w := call(t, s, "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"two"}`); w.Code != http.StatusAccepted {
		t.Fatalf("message: %d %s", w.Code, w.Body.String())
	}
	turnsEnded(t, st, id, 2)
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Fatalf("after a resume model-a answered %d calls and model-b %d; want 1 each", a.calls.Load(), b.calls.Load())
	}
	if sys := b.system[len(b.system)-1]; !strings.Contains(sys, "Model: model-b") {
		t.Fatalf("the resumed prompt does not name model-b:\n%s", sys)
	}
}

// A session started on a chosen model, workbench or not, is continued on it.
func TestResumedSessionKeepsTheModelItStartedOn(t *testing.T) {
	for _, body := range []string{`{"workbench":true,"provider":"b"}`, `{"prompt":"one","provider":"b"}`} {
		t.Run(body, func(t *testing.T) {
			s, st, a, b := switchServer(t)
			id := sessionOf(t, call(t, s, "POST", "/v1/sessions", body))
			if strings.Contains(body, "prompt") {
				turnsEnded(t, st, id, 1)
			} else {
				s.closeIdle() // a restart ends an idle workbench session, as shutdown does
			}
			before := b.calls.Load()
			forget(s, id)
			if w := call(t, s, "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"next"}`); w.Code != http.StatusAccepted {
				t.Fatalf("message: %d %s", w.Code, w.Body.String())
			}
			for deadline := time.Now().Add(10 * time.Second); a.calls.Load() == 0 && b.calls.Load() == before; time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the continued session made no model call")
				}
			}
			if a.calls.Load() != 0 {
				t.Fatalf("a session started on model-b was continued on model-a")
			}
		})
	}
}

// A finished session this server no longer holds is reopened to switch, not
// refused as unknown; an unknown provider and a turn in flight are refused,
// saying why, and leave the model as it was.
func TestModelSwitchRefusalsSayWhy(t *testing.T) {
	s, st, a, _ := switchServer(t)
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"one"}`))
	turnsEnded(t, st, id, 1)
	forget(s, id)
	if w := call(t, s, "POST", "/v1/sessions/"+id+"/model", `{"provider":"nope"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "no such provider") {
		t.Fatalf("an unknown provider: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, "POST", "/v1/sessions/"+id+"/model", `{"provider":"b"}`); w.Code != http.StatusOK {
		t.Fatalf("a session from before a restart could not be switched: %d %s", w.Code, w.Body.String())
	}

	a.mu.Lock()
	a.hold = make(chan struct{})
	a.mu.Unlock()
	busy := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"slow"}`))
	for deadline := time.Now().Add(10 * time.Second); a.calls.Load() < 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the slow turn never reached the model")
		}
	}
	w := call(t, s, "POST", "/v1/sessions/"+busy+"/model", `{"provider":"b"}`)
	close(a.hold)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "mid-turn") {
		t.Fatalf("a switch mid-turn: %d %s", w.Code, w.Body.String())
	}
	events := turnsEnded(t, st, busy, 1)
	if agent.ProviderOf(events) != "" {
		t.Fatal("a refused switch was recorded")
	}
}

// Only the owner switches a session's model: another user in the tenant and
// a user of the same name in another tenant are told it does not exist,
// whether the session is live here or only in the store, and nothing is written.
func TestModelSwitchIsTheOwnersAlone(t *testing.T) {
	s, st, _, _ := switchServer(t, func(c *config.Config) { c.Auth.Mode = "proxy" })
	id := sessionOf(t, callAs(t, s, "alice", "acme", "POST", "/v1/sessions", `{"prompt":"one"}`))
	turnsEnded(t, st, id, 1)
	strangers := [][2]string{{"bob", "acme"}, {"alice", "other"}}
	for _, where := range []string{"live", "stored"} {
		if where == "stored" {
			forget(s, id)
		}
		for _, who := range strangers {
			w := callAs(t, s, who[0], who[1], "POST", "/v1/sessions/"+id+"/model", `{"provider":"b"}`)
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s in %s switched alice's %s session: %d %s", who[0], who[1], where, w.Code, w.Body.String())
			}
		}
	}
	events, _ := st.Events(id)
	for _, ev := range events {
		if ev.Type == agent.EvModelSwitched {
			t.Fatalf("a stranger's switch was written to alice's record: %s", ev.Payload)
		}
	}
	if w := callAs(t, s, "alice", "acme", "POST", "/v1/sessions/"+id+"/model", `{"provider":"b"}`); w.Code != http.StatusOK {
		t.Fatalf("the owner could not switch: %d %s", w.Code, w.Body.String())
	}
}

// A session whose recorded provider is no longer configured continues on the
// default, and its next turn records the move before the call it explains.
func TestResumeOnARemovedProviderRecordsTheFallback(t *testing.T) {
	s, st, a, b := switchServer(t)
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"one","provider":"b"}`))
	turnsEnded(t, st, id, 1)
	if w := call(t, s, "POST", "/v1/sessions/"+id+"/model", `{"provider":"b"}`); w.Code != http.StatusOK {
		t.Fatalf("switch: %d %s", w.Code, w.Body.String())
	}
	delete(s.opts.Config.Model.Providers, "b") // as a later configuration without it would
	forget(s, id)
	before := b.calls.Load()
	if w := call(t, s, "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"two"}`); w.Code != http.StatusAccepted {
		t.Fatalf("message: %d %s", w.Code, w.Body.String())
	}
	events := turnsEnded(t, st, id, 2)
	if a.calls.Load() != 1 || b.calls.Load() != before {
		t.Fatalf("model-a answered %d calls and model-b %d more", a.calls.Load(), b.calls.Load()-before)
	}
	var fallback, message int
	for i, ev := range events {
		if ev.Type == agent.EvModelSwitched && strings.Contains(string(ev.Payload), `"provider":"a"`) &&
			strings.Contains(string(ev.Payload), `"from":"model-b"`) {
			fallback = i
		}
		if ev.Type == agent.EvUserMessage {
			message = i
		}
	}
	if fallback == 0 || fallback > message {
		t.Fatalf("the fallback to model-a is not recorded before the turn it explains")
	}
	if agent.ProviderOf(events) != "a" {
		t.Fatalf("the record still names %q", agent.ProviderOf(events))
	}
}

// A session started on a chosen provider and never switched, whose provider
// was later removed, continues on the default and records the move.
func TestResumeOfAnUnswitchedSessionOnARemovedProviderRecordsTheFallback(t *testing.T) {
	s, st, a, _ := switchServer(t)
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"one","provider":"b"}`))
	turnsEnded(t, st, id, 1)
	delete(s.opts.Config.Model.Providers, "b") // as a later configuration without it would
	forget(s, id)
	if w := call(t, s, "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"two"}`); w.Code != http.StatusAccepted {
		t.Fatalf("message: %d %s", w.Code, w.Body.String())
	}
	events := turnsEnded(t, st, id, 2)
	if a.calls.Load() != 1 {
		t.Fatalf("model-a answered %d calls; want 1", a.calls.Load())
	}
	var fallback, message int
	for i, ev := range events {
		if ev.Type == agent.EvModelSwitched && strings.Contains(string(ev.Payload), `"provider":"a"`) &&
			strings.Contains(string(ev.Payload), `"from":"model-b"`) {
			fallback = i
		}
		if ev.Type == agent.EvUserMessage {
			message = i
		}
	}
	if fallback == 0 || fallback > message {
		t.Fatal("the silent fallback from model-b to model-a is not recorded before the turn it explains")
	}
}

// A session started on a model two providers now serve cannot say which it
// ran on, so it continues on the default and records the move.
func TestResumeOnAModelSeveralProvidersServeRecordsTheFallback(t *testing.T) {
	s, st, a, _ := switchServer(t)
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"one","provider":"b"}`))
	turnsEnded(t, st, id, 1)
	s.opts.Config.Model.Providers["c"] = s.opts.Config.Model.Providers["b"] // a second provider for model-b
	forget(s, id)
	if w := call(t, s, "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"two"}`); w.Code != http.StatusAccepted {
		t.Fatalf("message: %d %s", w.Code, w.Body.String())
	}
	events := turnsEnded(t, st, id, 2)
	if a.calls.Load() != 1 {
		t.Fatalf("model-a answered %d calls; want 1", a.calls.Load())
	}
	recorded := false
	for _, ev := range events {
		if ev.Type == agent.EvUserMessage && recorded {
			return
		}
		if ev.Type == agent.EvModelSwitched && strings.Contains(string(ev.Payload), `"provider":"a"`) &&
			strings.Contains(string(ev.Payload), `"from":"model-b"`) {
			recorded = true
		}
	}
	t.Fatal("the fallback from model-b to model-a is not recorded before the turn it explains")
}
