package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// stepsModel delegates once and then gives the subagent childCalls in turn,
// one per request, ending each conversation with a closing line.
func stepsModel(t *testing.T, childCalls ...string) model.Adapter {
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
		child, results := false, 0
		for _, m := range req.Messages {
			child = child || (m.Role == "user" && m.Content == childPrompt)
			if m.Role == "tool" {
				results++
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		call := func(name, args string) {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c`+strconv.FormatInt(n.Add(1), 10)+
				`","type":"function","function":{"name":"`+name+`","arguments":`+strconv.Quote(args)+`}}]}}]}`)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		}
		switch {
		case !child && results == 0:
			call(delegate[0], delegate[1])
		case child && results < len(childCalls):
			call("bash", `{"command":`+strconv.Quote(childCalls[results])+`}`)
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

func stepsServer(t *testing.T, adjust func(*Options), childCalls ...string) (*Server, string) {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	cfg.Permissions.Ask = []string{"bash(touch *)"}
	o := Options{Workspace: ws, Config: cfg, Adapter: stepsModel(t, childCalls...),
		Registry: tools.NewRegistry(tools.Read{}, tools.Bash{})}
	if adjust != nil {
		adjust(&o)
	}
	return New(o), ws
}

// asksOf returns the subagent.ask events recorded in the session so far.
func asksOf(s *Server, id string) []agent.SubagentAsk {
	evs, _ := s.store.Events(id)
	var out []agent.SubagentAsk
	for _, ev := range evs {
		if ev.Type == agent.EvSubagentAsk {
			var a agent.SubagentAsk
			_ = json.Unmarshal(ev.Payload, &a)
			out = append(out, a)
		}
	}
	return out
}

func waitAsks(t *testing.T, s *Server, id string, n int) []agent.SubagentAsk {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if a := asksOf(s, id); len(a) >= n {
			return a
		}
	}
	t.Fatalf("no subagent.ask #%d", n)
	return nil
}

// An answer is taken only when it names the subagent's pending request, on
// the parent session, from someone who may see it. A wrong id, the parent's
// own request, a replay of an answered one, another person, and the child's
// session id are all refused, and nothing the child asked runs.
func TestSubagentAskTakesOnlyItsOwnAnswer(t *testing.T) {
	s, ws := stepsServer(t, nil, "touch a.txt", "touch b.txt")
	id := sessionOf(t, callAs(t, s, "alice", "default", "POST", "/v1/sessions", `{"prompt":"go"}`))
	first := waitAsks(t, s, id, 1)[0]
	evs, _ := s.store.Events(id)
	parentReq := ""
	for _, ev := range evs {
		if ev.Type == agent.EvActionRequested {
			parentReq = ev.ID
		}
	}
	yes := func(rid string) string { return `{"approved":true,"request_id":"` + rid + `"}` }
	for name, w := range map[string]*httptest.ResponseRecorder{
		"a wrong id":             callAs(t, s, "alice", "default", "POST", "/v1/sessions/"+id+"/approve", yes("ev-nope")),
		"the parent's own id":    callAs(t, s, "alice", "default", "POST", "/v1/sessions/"+id+"/approve", yes(parentReq)),
		"another person":         callAs(t, s, "bob", "default", "POST", "/v1/sessions/"+id+"/approve", yes(first.RequestID)),
		"the child's session id": callAs(t, s, "alice", "default", "POST", "/v1/sessions/"+first.Session+"/approve", yes(first.RequestID)),
	} {
		if w.Code == http.StatusNoContent || w.Code == http.StatusOK {
			t.Fatalf("%s was taken as the answer: %d %s", name, w.Code, w.Body)
		}
	}
	if w := callAs(t, s, "alice", "default", "POST", "/v1/sessions/"+id+"/approve", `{"approved":false,"request_id":"`+first.RequestID+`"}`); w.Code != http.StatusNoContent {
		t.Fatalf("the right answer was refused: %d %s", w.Code, w.Body)
	}
	second := waitAsks(t, s, id, 2)[1]
	if w := callAs(t, s, "alice", "default", "POST", "/v1/sessions/"+id+"/approve", yes(first.RequestID)); w.Code == http.StatusNoContent {
		t.Fatal("a replay of an answered request approved the next one")
	}
	if w := callAs(t, s, "alice", "default", "POST", "/v1/sessions/"+id+"/approve", `{"approved":false,"request_id":"`+second.RequestID+`"}`); w.Code != http.StatusNoContent {
		t.Fatalf("the second answer was refused: %d %s", w.Code, w.Body)
	}
	eventsUntil(t, s, id, agent.EvSessionEnded)
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(ws, f)); err == nil {
			t.Fatalf("%s was made though every answer was no", f)
		}
	}
}

// A stored secret in a subagent's call is redacted in the parent's copy.
func TestSubagentAskIsRedacted(t *testing.T) {
	const secret = "tok-9d8e7f6a5b4c"
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("DEPLOY_TOKEN", secret); err != nil {
		t.Fatal(err)
	}
	s, _ := stepsServer(t, func(o *Options) { o.Redact = vault.Redactor() }, "touch "+secret)
	id := sessionOf(t, callAs(t, s, "alice", "default", "POST", "/v1/sessions", `{"prompt":"go"}`))
	ask := waitAsks(t, s, id, 1)[0]
	raw, _ := json.Marshal(ask)
	if strings.Contains(string(raw), secret) || !strings.Contains(string(raw), "[secret:DEPLOY_TOKEN]") {
		t.Fatalf("the parent's subagent.ask carries the secret: %s", raw)
	}
	if kids, _ := s.store.Events(ask.Session); strings.Contains(fmt.Sprint(kids), secret) {
		t.Fatal("the subagent's own record carries the secret")
	}
	callAs(t, s, "alice", "default", "POST", "/v1/sessions/"+id+"/approve", `{"approved":false,"request_id":"`+ask.RequestID+`"}`)
	eventsUntil(t, s, id, agent.EvSessionEnded)
}

// A deny rule binds a subagent in bypass mode.
func TestSubagentDenyHoldsInBypass(t *testing.T) {
	s, ws := stepsServer(t, func(o *Options) {
		o.Config.Permissions.Mode = "bypass"
		o.Config.Permissions.Deny = []string{"bash(touch *)"}
	}, "touch denied.txt")
	id := sessionOf(t, callAs(t, s, "alice", "default", "POST", "/v1/sessions", `{"prompt":"go"}`))
	evs := eventsUntil(t, s, id, agent.EvSessionEnded)
	got := payloadOf[agent.SubagentAction](t, evs, agent.EvSubagentAction)
	if got.Decision != "denied" || got.Step != "deny" {
		t.Fatalf("the deny rule did not bind the subagent: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "denied.txt")); err == nil {
		t.Fatal("a denied command ran in a subagent")
	}
}

// Interrupting the parent while a subagent waits settles the ask as denied,
// and a late answer is refused.
func TestInterruptSettlesASubagentsAsk(t *testing.T) {
	s, ws := stepsServer(t, nil, "touch late.txt")
	id := sessionOf(t, callAs(t, s, "alice", "default", "POST", "/v1/sessions", `{"prompt":"go"}`))
	ask := waitAsks(t, s, id, 1)[0]
	if w := callAs(t, s, "alice", "default", "POST", "/v1/sessions/"+id+"/interrupt", ""); w.Code >= 300 {
		t.Fatalf("interrupt %d %s", w.Code, w.Body)
	}
	evs := eventsUntil(t, s, id, agent.EvSessionEnded)
	got := payloadOf[agent.SubagentAction](t, evs, agent.EvSubagentAction)
	if got.Decision != "denied" || got.RequestID != ask.RequestID {
		t.Fatalf("the interrupted ask was not settled as denied: %+v", got)
	}
	if w := callAs(t, s, "alice", "default", "POST", "/v1/sessions/"+id+"/approve", `{"approved":true,"request_id":"`+ask.RequestID+`"}`); w.Code == http.StatusNoContent {
		t.Fatal("a late answer was taken")
	}
	if _, err := os.Stat(filepath.Join(ws, "late.txt")); err == nil {
		t.Fatal("the interrupted subagent's call ran")
	}
}
