package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/skills"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
)

const childPrompt = "do the subtask"

// delegate is the parent's call that spawns one subagent.
var delegate = [2]string{"task", `{"prompt":"` + childPrompt + `","description":"subtask"}`}

// scriptedModel is an endpoint for a session making one call: the parent's
// first request is answered with parent, the subagent's with child (a tool
// call, or a summary when empty), and every request after a tool result
// with a closing line.
func scriptedModel(t *testing.T, parent, child [2]string) model.Adapter {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		last := req.Messages[len(req.Messages)-1]
		w.Header().Set("Content-Type", "text/event-stream")
		call := func(name, args string) {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c`+strconv.FormatInt(n.Add(1), 10)+
				`","type":"function","function":{"name":"`+name+`","arguments":`+strconv.Quote(args)+`}}]}}]}`)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		}
		switch {
		case last.Role == "user" && last.Content == childPrompt && child[0] != "":
			call(child[0], child[1])
		case last.Role == "user" && last.Content != childPrompt && parent[0] != "":
			call(parent[0], parent[1])
		default:
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	a, err := config.ProviderConfig{Type: "openai-compatible", BaseURL: srv.URL, Model: "m", ContextWindow: 8192}.Adapter()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// delegatingServer serves sessions that delegate, in a workspace with an ask
// rule for touch, over st when given.
func delegatingServer(t *testing.T, child [2]string, st EventStore, adjust ...func(*config.Config)) (*Server, string) {
	t.Helper()
	return scriptedServer(t, delegate, child, st, nil, adjust...)
}

// scriptedServer serves sessions making one call each; opt, when set, adjusts
// the server's options before it is built.
func scriptedServer(t *testing.T, parent, child [2]string, st EventStore, opt func(*Options), adjust ...func(*config.Config)) (*Server, string) {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Permissions.Ask = []string{"bash(touch *)"}
	for _, f := range adjust {
		f(&cfg)
	}
	o := Options{Workspace: ws, Config: cfg, Adapter: scriptedModel(t, parent, child),
		Registry: tools.NewRegistry(tools.Read{}, tools.Glob{}, tools.Bash{}), Store: st}
	if opt != nil {
		opt(&o)
	}
	return New(o), ws
}

// eventsUntil waits for an event of type want in the session's record.
func eventsUntil(t *testing.T, s *Server, id string, want agent.EventType) []agent.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		evs, _ := s.store.Events(id)
		for _, ev := range evs {
			if ev.Type == want {
				return evs
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	evs, _ := s.store.Events(id)
	t.Fatalf("no %s in %s: %v", want, id, typesOf(evs))
	return nil
}

func typesOf(evs []agent.Event) []agent.EventType {
	out := make([]agent.EventType, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Type)
	}
	return out
}

func payloadOf[T any](t *testing.T, evs []agent.Event, typ agent.EventType) T {
	t.Helper()
	var v T
	for _, ev := range evs {
		if ev.Type == typ {
			if err := json.Unmarshal(ev.Payload, &v); err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatalf("no %s in %v", typ, typesOf(evs))
	return v
}

// A console session can delegate, and the delegation is in its record: the
// spawn and the return in the parent's, the child's own turns in a record
// linked to it.
func TestServerSessionSpawnsASubagent(t *testing.T) {
	s, _ := delegatingServer(t, [2]string{}, nil)
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"delegate it"}`))
	evs := eventsUntil(t, s, id, agent.EvSessionEnded)

	spawned := payloadOf[map[string]any](t, evs, agent.EvSubagentSpawned)
	returned := payloadOf[map[string]any](t, evs, agent.EvSubagentReturn)
	child, _ := spawned["session"].(string)
	if child == "" || returned["session"] != child || returned["reason"] != string(agent.TermCompleted) {
		t.Fatalf("spawn %v, return %v", spawned, returned)
	}
	kids, _ := s.store.Events(child)
	if len(kids) == 0 || kids[0].ParentID != id {
		t.Fatalf("the child's record is not linked to its parent: %v", kids)
	}
	var obs agent.Observation
	for _, ev := range evs {
		if ev.Type == agent.EvObservation {
			_ = json.Unmarshal(ev.Payload, &obs)
		}
	}
	if obs.Tool != "task" || obs.IsError {
		t.Fatalf("the task call did not run: %+v", obs)
	}
}

// A subagent's ask-rule call in a console session is put to the person, in
// the parent's record where the console reads, and runs only on their answer.
func TestServerSubagentAskReachesThePerson(t *testing.T) {
	s, ws := delegatingServer(t, [2]string{"bash", `{"command":"touch made.txt"}`}, nil)
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"delegate it"}`))
	evs := eventsUntil(t, s, id, agent.EvSubagentAsk)
	ask := payloadOf[agent.SubagentAsk](t, evs, agent.EvSubagentAsk)
	if ask.Tool != "bash" || ask.RequestID == "" || ask.Subagent != "subtask" {
		t.Fatalf("ask %+v", ask)
	}
	if _, err := os.Stat(filepath.Join(ws, "made.txt")); err == nil {
		t.Fatal("the subagent's call ran before anyone answered")
	}
	w := call(t, s, "POST", "/v1/sessions/"+id+"/approve", `{"approved":true,"request_id":"`+ask.RequestID+`"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("answer %d %s", w.Code, w.Body)
	}
	evs = eventsUntil(t, s, id, agent.EvSessionEnded)
	if _, err := os.Stat(filepath.Join(ws, "made.txt")); err != nil {
		t.Fatalf("the approved call did not run: %v", err)
	}
	got := payloadOf[agent.SubagentAction](t, evs, agent.EvSubagentAction)
	if got.Decision != "allowed" || got.By != agent.ByReviewer || got.RequestID != ask.RequestID {
		t.Fatalf("the parent's record does not show the answer: %+v", got)
	}
}

// With nobody to ask, a subagent's ask-rule call is refused, never approved
// on its behalf.
func TestUnattendedSubagentAskIsRefused(t *testing.T) {
	s, ws := delegatingServer(t, [2]string{"bash", `{"command":"touch made.txt"}`}, nil)
	id, err := s.StartSession(context.Background(), StartSpec{Prompt: "delegate it", Unattended: true})
	if err != nil {
		t.Fatal(err)
	}
	evs := eventsUntil(t, s, id, agent.EvSessionEnded)
	if _, err := os.Stat(filepath.Join(ws, "made.txt")); err == nil {
		t.Fatal("an unattended subagent's ask-rule call ran")
	}
	got := payloadOf[agent.SubagentAction](t, evs, agent.EvSubagentAction)
	if got.Decision != "denied" || got.By != agent.ByHeadless || got.Step != "ask" {
		t.Fatalf("the refusal is not in the parent's record: %+v", got)
	}
}

// Two people's sessions each delegate: each child's row is its parent
// owner's, the other person cannot read it, and neither list shows it.
func TestSubagentsOfTwoUsersAreKeptApart(t *testing.T) {
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}}
	s, _ := delegatingServer(t, [2]string{}, st, func(c *config.Config) { c.Auth.Mode = "proxy" })

	children := map[string]string{}
	parents := map[string]string{}
	for _, user := range []string{"alice", "bob"} {
		id := sessionOf(t, callAs(t, s, user, "default", "POST", "/v1/sessions", `{"prompt":"delegate it"}`))
		evs := eventsUntil(t, s, id, agent.EvSessionEnded)
		sp := payloadOf[map[string]any](t, evs, agent.EvSubagentSpawned)
		children[user], parents[user] = sp["session"].(string), id
	}
	for user, child := range children {
		st.mu.Lock()
		row, ok := st.rows[child]
		st.mu.Unlock()
		if !ok || row.User != user || row.ParentID != parents[user] || row.Model != "subagent" {
			t.Fatalf("%s's subagent row: %+v (found %v)", user, row, ok)
		}
	}
	if w := callAs(t, s, "bob", "default", "GET", "/v1/sessions/"+children["alice"]+"/events", ""); w.Code != http.StatusNotFound {
		t.Fatalf("bob read alice's subagent: %d", w.Code)
	}
	if w := callAs(t, s, "alice", "default", "GET", "/v1/sessions/"+children["alice"]+"/events", ""); w.Code != http.StatusOK {
		t.Fatalf("alice cannot read her own subagent: %d", w.Code)
	}
	// Read, never resumed on its own: it goes on only through its parent.
	if w := callAs(t, s, "alice", "default", "POST", "/v1/sessions/"+children["alice"]+"/messages", `{"prompt":"go on"}`); w.Code < 400 {
		t.Fatalf("alice resumed her subagent's session on its own: %d %s", w.Code, w.Body)
	}
	var listed []sessionSummary
	_ = json.Unmarshal(callAs(t, s, "alice", "default", "GET", "/v1/sessions", "").Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0].ID != parents["alice"] {
		t.Fatalf("alice's list: %+v", listed)
	}
	// Each parent's record names only its own child.
	for user, id := range parents {
		evs, _ := s.store.Events(id)
		for _, ev := range evs {
			if ev.Type == agent.EvSubagentSpawned {
				var p map[string]any
				_ = json.Unmarshal(ev.Payload, &p)
				if p["session"] != children[user] {
					t.Fatalf("%s's record names another's subagent: %v", user, p)
				}
			}
		}
	}
}

// A skill's declared pipeline runs in a console session, its steps put
// through the session's loop, where before it fell back to its instructions.
func TestServerSkillRunsItsPipeline(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gather")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: gather\ndescription: use when gathering\n---\nWrite the answer.\n"
	pipe := `{"stages":[{"name":"collect","steps":[{"kind":"tool","tool":"glob","args":{"pattern":"*"},"output":"files","required":true}]}]}`
	for name, body := range map[string]string{"SKILL.md": md, "pipeline.json": pipe} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg, errs := skills.Load([]string{root})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	s, _ := scriptedServer(t, [2]string{"skill", `{"name":"gather"}`}, [2]string{}, nil, func(o *Options) { o.SkillRegistry = reg })
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"gather it"}`))
	evs := eventsUntil(t, s, id, agent.EvSessionEnded)
	stage := payloadOf[map[string]any](t, evs, agent.EvPlanUpdated)
	if stage["skill"] != "gather" {
		t.Fatalf("stage %v", stage)
	}
	var viaStep bool
	for _, ev := range evs {
		var a agent.ActionRequested
		if ev.Type == agent.EvActionRequested && json.Unmarshal(ev.Payload, &a) == nil && a.Via == "skill gather pipeline" {
			viaStep = true
		}
	}
	if !viaStep {
		t.Fatalf("the pipeline's step was not put through the session's loop: %v", typesOf(evs))
	}
}

// Deleting a session deletes its subagents' records, which hold its work, by
// the same path: marked in a durable store, forgotten in memory.
func TestDeletingASessionDeletesItsSubagents(t *testing.T) {
	s, _ := delegatingServer(t, [2]string{}, nil)
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"delegate it"}`))
	evs := eventsUntil(t, s, id, agent.EvSessionEnded)
	child := payloadOf[map[string]any](t, evs, agent.EvSubagentSpawned)["session"].(string)
	if kids, _ := s.store.Events(child); len(kids) == 0 {
		t.Fatal("precondition: the subagent has a record")
	}
	if w := call(t, s, "DELETE", "/v1/sessions/"+id, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", w.Code, w.Body)
	}
	if kids, _ := s.store.Events(child); len(kids) != 0 {
		t.Fatalf("the deleted session's subagent record outlived it: %d events", len(kids))
	}
}

// Subagents are left out of a list by the parent they name, not by a model
// name: a session on a model called "subagent" is listed like any other.
func TestListKeepsASessionOnAModelNamedSubagent(t *testing.T) {
	st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}}
	s, _ := delegatingServer(t, [2]string{}, st)
	for id, parent := range map[string]string{"s-top": "", "s-kid": "s-top"} {
		if err := st.CreateSession(context.Background(), store.SessionRecord{ID: id, Tenant: "default",
			User: "anonymous", Model: "subagent", ParentID: parent, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	var listed []sessionSummary
	_ = json.Unmarshal(call(t, s, "GET", "/v1/sessions", "").Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0].ID != "s-top" {
		t.Fatalf("listed %+v, want the top-level session only", listed)
	}
}

// The resume backstop: a row that names no parent, as a 1.2.x CLI subagent's
// row does, is refused when its record is a subagent's, whether its events
// name the parent or, from before they did, it begins with the child's spawn.
func TestResumeRefusesASubagentRecordByItsEvents(t *testing.T) {
	for name, first := range map[string]agent.Event{
		"events name a parent":  {Type: agent.EvUserMessage, ParentID: "s-top"},
		"begins with its spawn": {Type: agent.EvSubagentSpawned},
	} {
		t.Run(name, func(t *testing.T) {
			st := &durableMem{MemStore: agent.NewMemStore(), rows: map[string]store.SessionRecord{}, ended: map[string]bool{}}
			s, _ := delegatingServer(t, [2]string{}, st, func(c *config.Config) { c.Auth.Mode = "proxy" })
			const id = "s-legacy-child"
			if err := st.CreateSession(context.Background(), store.SessionRecord{ID: id, Tenant: "default",
				User: "alice", Model: "subagent", Mode: "default", StartedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			first.ID, first.SessionID, first.Seq, first.Payload = "k1", id, 1, json.RawMessage(`{"description":"subtask"}`)
			msg, _ := json.Marshal(agent.Message{Text: "child work"})
			for _, ev := range []agent.Event{first,
				{ID: "k2", SessionID: id, ParentID: first.ParentID, Seq: 2, Type: agent.EvUserMessage, Payload: msg},
				{ID: "k3", SessionID: id, ParentID: first.ParentID, Seq: 3, Type: agent.EvSessionEnded, Payload: json.RawMessage(`{"reason":"completed"}`)},
			} {
				if err := st.Append(ev); err != nil {
					t.Fatal(err)
				}
			}
			w := callAs(t, s, "alice", "default", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"go on"}`)
			if w.Code != http.StatusNotFound {
				t.Fatalf("a subagent's record was resumed on its own: %d %s", w.Code, w.Body)
			}
		})
	}
}
