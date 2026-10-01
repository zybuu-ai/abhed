package abhed_test

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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// planningModel writes a plan, delegates, and ends: the first request is
// answered with a todo call, the next with a task call, the subagent with a
// summary, and the parent's last with a closing line.
func planningModel(t *testing.T) string {
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
		tools := 0
		for _, m := range req.Messages {
			if m.Role == "tool" {
				tools++
			}
		}
		last := req.Messages[len(req.Messages)-1]
		w.Header().Set("Content-Type", "text/event-stream")
		call := func(name, args string) {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c`+strconv.FormatInt(n.Add(1), 10)+
				`","type":"function","function":{"name":"`+name+`","arguments":`+strconv.Quote(args)+`}}]}}]}`)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		}
		switch {
		case last.Role == "user" && last.Content == "look around":
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"found it\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		case tools == 0:
			call("todo", `{"items":[{"id":"1","text":"look around","status":"in_progress"}]}`)
		case tools == 1:
			call("task", `{"prompt":"look around","description":"look around"}`)
		default:
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func has(evs []abhed.Event, typ agent.EventType) bool {
	for _, ev := range evs {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

// An embedded agent's todo list is recorded, and with ConfiguredTools it can
// delegate as the command line does; without, it has no subagents.
func TestEmbeddedAgentPlansAndDelegates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, full := range []bool{true, false} {
		a, err := abhed.New(context.Background(), abhed.Options{
			Workspace: t.TempDir(),
			Provider: &abhed.Provider{Type: "openai-compatible", BaseURL: planningModel(t),
				Model: "m", ContextWindow: 8192},
			ConfiguredTools: full,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Run(context.Background(), "plan and delegate"); err != nil {
			t.Fatal(err)
		}
		evs := a.Events()
		a.Close()
		if !has(evs, agent.EvTodoUpdated) {
			t.Fatalf("configured=%v: the todo list was not recorded", full)
		}
		if has(evs, agent.EvSubagentSpawned) != full || has(evs, agent.EvSubagentReturn) != full {
			t.Fatalf("configured=%v: subagent events %v", full, evs)
		}
	}
}

// The workspace's ABHED.md reaches an embedded agent's prompt only with
// ConfiguredTools: an embedder running on repositories it does not own opts in.
func TestEmbeddedAgentReadsMemoryFilesOnlyWhenConfigured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const mark = "MEMORY-MARK-42"
	for _, full := range []bool{false, true} {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "ABHED.md"), []byte(mark+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var system atomic.Value
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			system.Store(string(body))
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}))
		a, err := abhed.New(context.Background(), abhed.Options{Workspace: ws, ConfiguredTools: full,
			Provider: &abhed.Provider{Type: "openai-compatible", BaseURL: srv.URL, Model: "m", ContextWindow: 8192}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Run(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
		a.Close()
		srv.Close()
		sent, _ := system.Load().(string)
		if strings.Contains(sent, mark) != full {
			t.Fatalf("configured=%v: ABHED.md in the prompt = %v", full, !full)
		}
	}
}
