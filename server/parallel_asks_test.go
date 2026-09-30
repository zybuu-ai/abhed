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
	"github.com/zybuu-ai/abhed/internal/tools"
)

// parallelModel answers the parent with one tasks call spawning a subagent per
// name, each subagent with `touch NAME.txt`, and everything after a tool
// result with a closing line.
func parallelModel(t *testing.T, names ...string) *httptest.Server {
	t.Helper()
	var n atomic.Int64
	tasks := make([]map[string]string, 0, len(names))
	for _, name := range names {
		tasks = append(tasks, map[string]string{"prompt": "child " + name, "description": name})
	}
	spawn, _ := json.Marshal(map[string]any{"tasks": tasks})
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
		case last.Role == "user" && strings.HasPrefix(last.Content, "child "):
			call("bash", `{"command":"touch `+strings.TrimPrefix(last.Content, "child ")+`.txt"}`)
		case last.Role == "user":
			call("tasks", string(spawn))
		default:
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// openAsks is the subagent.ask events in the parent's record that no
// subagent.action has settled yet.
func openAsks(s *Server, id string) []agent.SubagentAsk {
	evs, _ := s.store.Events(id)
	open := map[string]agent.SubagentAsk{}
	var order []string
	for _, ev := range evs {
		switch ev.Type {
		case agent.EvSubagentAsk:
			var a agent.SubagentAsk
			if json.Unmarshal(ev.Payload, &a) == nil {
				open[a.RequestID] = a
				order = append(order, a.RequestID)
			}
		case agent.EvSubagentAction:
			var a agent.SubagentAction
			if json.Unmarshal(ev.Payload, &a) == nil {
				delete(open, a.RequestID)
			}
		}
	}
	var out []agent.SubagentAsk
	for _, rid := range order {
		if a, ok := open[rid]; ok {
			out = append(out, a)
		}
	}
	return out
}

// Parallel subagents run together, but a person answers one prompt at a
// time: the parent's record offers only the ask the run is waiting on, each
// is answerable when offered, and every answer is the one recorded.
func TestParallelSubagentAsksAreOfferedOneAtATime(t *testing.T) {
	names := []string{"alpha", "beta", "gamma"}
	srv := parallelModel(t, names...)
	adapter, err := config.ProviderConfig{Type: "openai-compatible", BaseURL: srv.URL, Model: "m", ContextWindow: 8192}.Adapter()
	if err != nil {
		t.Fatal(err)
	}
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Permissions.Ask = []string{"bash(touch *)"}
	s := New(Options{Workspace: ws, Config: cfg, Adapter: adapter,
		Registry: tools.NewRegistry(tools.Read{}, tools.Glob{}, tools.Bash{})})
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"fan out"}`))

	answers := map[string]bool{}
	for i := range names {
		var open []agent.SubagentAsk
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if open = openAsks(s, id); len(open) > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		// The siblings' calls are all waiting by now; give a second offer time to appear.
		time.Sleep(150 * time.Millisecond)
		if open = openAsks(s, id); len(open) != 1 {
			t.Fatalf("answer %d: %d asks offered at once, want 1: %+v", i+1, len(open), open)
		}
		ok := i != 1 // deny the second, so a lost answer shows
		w := call(t, s, "POST", "/v1/sessions/"+id+"/approve", fmt.Sprintf(`{"approved":%t,"request_id":%q}`, ok, open[0].RequestID))
		if w.Code != http.StatusNoContent {
			t.Fatalf("answer %d to %s: %d %s", i+1, open[0].Subagent, w.Code, w.Body)
		}
		answers[open[0].Subagent] = ok
	}
	evs := eventsUntil(t, s, id, agent.EvSessionEnded)
	if len(answers) != len(names) {
		t.Fatalf("not every subagent was asked: %v", answers)
	}
	for name, ok := range answers {
		if _, err := os.Stat(filepath.Join(ws, name+".txt")); (err == nil) != ok {
			t.Errorf("%s: answered %t, but the file exists=%t", name, ok, err == nil)
		}
	}
	settled := 0
	for _, ev := range evs {
		if ev.Type == agent.EvSubagentAction {
			var a agent.SubagentAction
			_ = json.Unmarshal(ev.Payload, &a)
			if a.By != agent.ByReviewer {
				t.Errorf("a subagent's call was settled by %q, not the person: %+v", a.By, a)
			}
			settled++
		}
	}
	if settled != len(names) {
		t.Fatalf("%d subagent calls settled in the parent's record, want %d", settled, len(names))
	}
	// Each child's own record holds its request, queued ones included.
	for _, ev := range evs {
		if ev.Type != agent.EvSubagentSpawned {
			continue
		}
		var sp struct {
			Session string `json:"session"`
		}
		_ = json.Unmarshal(ev.Payload, &sp)
		kids, _ := s.store.Events(sp.Session)
		requested := false
		for _, k := range kids {
			requested = requested || k.Type == agent.EvActionRequested
		}
		if !requested {
			t.Errorf("child %s has no action.requested: %v", sp.Session, typesOf(kids))
		}
	}
}
