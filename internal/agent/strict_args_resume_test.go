package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A refused call's arguments are not an object; a resumed session must still
// replay every call with object arguments, or the provider rejects each turn.
func TestResumeAfterRefusedArgumentsReplaysObjects(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{
			rawCall("read", `["notes.txt"]`),
			{ID: "c2", Name: "bash", Args: json.RawMessage(`{"command":"echo a","Command":"touch x","description":"d"}`)},
			{ID: "c3", Name: "read", Args: json.RawMessage(`{"file_path":"notes.txt"}`)},
		}},
		{text: "done"},
	}, policy.ModeBypass, true)
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("sess1")
	var raws int
	for _, e := range evs {
		if e.Type == EvActionRequested {
			var p ActionRequested
			_ = json.Unmarshal(e.Payload, &p)
			if p.RawArgs != "" {
				raws++
			}
		}
	}
	if raws != 2 {
		t.Fatalf("want the two refused calls' arguments kept as sent, got %d", raws)
	}
	msgs, err := Fork(evs, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, history := range [][]model.Message{msgs, l.Messages()} {
		n := 0
		for _, m := range history {
			for _, c := range m.ToolCalls {
				n++
				if _, err := tools.DecodeArgs(c.Args); err != nil || c.Args[0] != '{' {
					t.Fatalf("call %s replays arguments %s", c.ID, c.Args)
				}
			}
		}
		if n != 3 {
			t.Fatalf("want 3 calls replayed, got %d", n)
		}
	}

	for name, a := range map[string]func(string) model.Adapter{
		"anthropic": func(u string) model.Adapter { return model.NewAnthropic(u, "k", "m", model.Profile{}) },
		"gemini":    func(u string) model.Adapter { return model.NewGemini(u, "k", "m", model.Profile{}) },
		"openai":    func(u string) model.Adapter { return model.NewOpenAICompatible(u, "k", "m", model.Profile{}) },
	} {
		t.Run(name, func(t *testing.T) {
			var body []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ = io.ReadAll(r.Body)
				http.Error(w, "stop", http.StatusBadRequest)
			}))
			defer srv.Close()
			if s, err := a(srv.URL).Complete(context.Background(), model.Request{Messages: msgs, MaxTokens: 10}); err == nil {
				for range s {
				}
			}
			var v any
			if err := json.Unmarshal(body, &v); err != nil {
				t.Fatalf("no request captured: %v", err)
			}
			if n := checkCallArgs(t, v); n != 3 {
				t.Fatalf("found %d calls in the request, want 3: %s", n, body)
			}
		})
	}
}

// checkCallArgs finds each tool call's arguments in a provider request and
// fails unless they are an object; it returns how many it found.
func checkCallArgs(t *testing.T, v any) int {
	t.Helper()
	n := 0
	switch x := v.(type) {
	case map[string]any:
		if x["type"] == "tool_use" {
			n++
			if _, ok := x["input"].(map[string]any); !ok {
				t.Errorf("tool_use input %v is not an object", x["input"])
			}
		}
		if fc, ok := x["functionCall"].(map[string]any); ok {
			n++
			if _, ok := fc["args"].(map[string]any); !ok {
				t.Errorf("functionCall args %v is not an object", fc["args"])
			}
		}
		if f, ok := x["function"].(map[string]any); ok {
			if s, ok := f["arguments"].(string); ok {
				n++
				var m map[string]any
				if json.Unmarshal([]byte(s), &m) != nil || m == nil {
					t.Errorf("function arguments %q are not an object", s)
				}
			}
		}
		for _, e := range x {
			n += checkCallArgs(t, e)
		}
	case []any:
		for _, e := range x {
			n += checkCallArgs(t, e)
		}
	}
	return n
}

// A record whose arguments are not one object replays them as {}.
func TestForkReplaysNonObjectArgumentsAsEmpty(t *testing.T) {
	store := NewMemStore()
	rec := NewRecorder(store, "s", "")
	for i, args := range []string{`"{\"path\":\"x\"}"`, `["x"]`, `null`, `{"path":"a","Path":"b"}`, `{"path":"a"}`} {
		id := string(rune('a' + i))
		if _, err := rec.Record(EvActionRequested, ActorAgent, Trusted, ActionRequested{CallID: id, Tool: "read", Args: json.RawMessage(args)}); err != nil {
			t.Fatal(err)
		}
		if _, err := rec.Record(EvObservation, ActorTool, Untrusted, Observation{CallID: id, Tool: "read", Content: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	evs, _ := store.Events("s")
	msgs, err := Fork(evs, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			got = append(got, string(c.Args))
		}
	}
	if want := []string{"{}", "{}", "{}", "{}", `{"path":"a"}`}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] || got[4] != want[4] {
		t.Fatalf("replayed %q, want %q", got, want)
	}
}
