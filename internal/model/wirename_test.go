package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	longName   = "mcp__a_server_with_a_long_name__a_tool_whose_name_is_long_enough_to_pass_sixty_four"
	dottedName = "mcp__docs__search.pages"
)

// Every wire name is one a provider accepts, the same each time, and kept
// apart from another name's.
func TestWireNameIsValidStableAndDistinct(t *testing.T) {
	for _, name := range []string{"read", "mcp__gh__create_issue", longName, dottedName, "mcp__x__a.b.c", strings.Repeat("é", 70)} {
		w := WireName(name)
		if !ValidWireName(w) {
			t.Errorf("WireName(%q) = %q, which a provider refuses", name, w)
		}
		if WireName(name) != w {
			t.Errorf("WireName(%q) changed between calls", name)
		}
		if ValidWireName(name) && w != name {
			t.Errorf("a valid name %q was renamed %q", name, w)
		}
	}
	if WireName("mcp__x__a.b") == WireName("mcp__x__a_b") {
		t.Error("two names share a wire name")
	}
}

// capture serves frames for each request and keeps each request's body.
func capture(t *testing.T, frames func(n int) []string) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames(int(n.Add(1))) {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return bodies
	}
}

func calls(t *testing.T, a Adapter, req Request) []ToolCall {
	t.Helper()
	ch, err := a.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var out []ToolCall
	for c := range ch {
		if c.Type == ChunkError {
			t.Fatal(c.Err)
		}
		if c.Type == ChunkToolCall {
			out = append(out, *c.ToolCall)
		}
	}
	return out
}

// sentNames are the tool names in a request body, wherever the provider's
// format puts them: the tool list and earlier turns' calls.
func sentNames(body map[string]any) []string {
	var out []string
	var walk func(v any, key string)
	walk = func(v any, key string) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if k == "name" && (key == "function" || key == "tools" || key == "functionCall" || key == "functionDeclarations" || key == "content" || key == "functionResponse") {
					if s, ok := e.(string); ok {
						out = append(out, s)
					}
				}
				walk(e, k)
			}
		case []any:
			for _, e := range x {
				walk(e, key)
			}
		}
	}
	walk(body, "")
	return out
}

// A long or dotted MCP name goes to each provider as a valid wire name, in
// the tool list and in history, and a call by it comes back as the real name.
func TestLongAndDottedNamesRoundTripThroughTheWire(t *testing.T) {
	cases := []struct {
		name   string
		a      func(url string) Adapter
		frames func(wire string) []string
	}{
		{"openai", func(u string) Adapter { return NewOpenAICompatible(u, "", "m", Profile{}) }, func(w string) []string {
			return []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"` + w + `","arguments":"{\"q\":\"x\"}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			}
		}},
		{"anthropic", func(u string) Adapter { return NewAnthropic(u, "k", "m", Profile{}) }, func(w string) []string {
			return []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c1","name":"` + w + `","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			}
		}},
		{"gemini", func(u string) Adapter { return NewGemini(u, "k", "m", Profile{}) }, func(w string) []string {
			return []string{`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"` + w + `","args":{"q":"x"}}}]},"finishReason":"STOP"}]}`}
		}},
	}
	for _, c := range cases {
		for _, real := range []string{longName, dottedName} {
			t.Run(c.name+"/"+real[:12], func(t *testing.T) {
				wire := WireName(real)
				srv, bodies := capture(t, func(int) []string { return c.frames(wire) })
				req := Request{
					Messages: []Message{
						{Role: RoleUser, Content: "go"},
						{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c0", Name: real, Args: json.RawMessage(`{"q":"y"}`)}}},
						{Role: RoleTool, ToolCallID: "c0", Content: "earlier"},
					},
					Tools: []ToolDef{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
						{Name: real, Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
				}
				got := calls(t, c.a(srv.URL), req)
				if len(got) != 1 || got[0].Name != real {
					t.Fatalf("the call came back as %+v, want %s", got, real)
				}
				names := sentNames(bodies()[0])
				if len(names) == 0 {
					t.Fatal("no tool names found in the request")
				}
				for _, n := range names {
					if !ValidWireName(n) {
						t.Fatalf("%q was sent to the provider: %v", n, names)
					}
				}
				if !strings.Contains(fmt.Sprint(names), wire) {
					t.Fatalf("the wire name %s is not among %v", wire, names)
				}
			})
		}
	}
}

// A request with more tools than the provider takes is refused before it is
// sent, naming the cause, not left to the provider's 400.
func TestTooManyToolsIsRefusedBeforeSending(t *testing.T) {
	var tools []ToolDef
	for i := range 129 {
		tools = append(tools, ToolDef{Name: fmt.Sprintf("t%d", i), InputSchema: json.RawMessage(`{"type":"object"}`)})
	}
	for _, a := range []func(string) Adapter{
		func(u string) Adapter { return NewOpenAICompatible(u, "", "m", Profile{}) },
		func(u string) Adapter { return NewAnthropic(u, "k", "m", Profile{}) },
		func(u string) Adapter { return NewGemini(u, "k", "m", Profile{}) },
		func(u string) Adapter {
			return NewWatsonX(WatsonXConfig{BaseURL: u, IAMURL: u, APIKey: "k", ModelID: "m"})
		},
	} {
		srv, bodies := capture(t, func(int) []string { return nil })
		ad := a(srv.URL)
		_, err := ad.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "x"}}, Tools: tools})
		if !errors.Is(err, ErrTooManyTools) || !strings.Contains(err.Error(), "offers 129 tools") || !strings.Contains(err.Error(), "at most 128") {
			t.Fatalf("%s: %v", ad.Name(), err)
		}
		if len(bodies()) != 0 {
			t.Fatalf("%s: the request was sent", ad.Name())
		}
	}
	// A profile's own limit is the one kept.
	srv, _ := capture(t, func(int) []string { return []string{`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`} })
	c := NewOpenAICompatible(srv.URL, "", "m", Profile{})
	c.SetMaxTools(200)
	if _, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "x"}}, Tools: tools}); err != nil {
		t.Fatalf("with max_tools 200: %v", err)
	}
}

// max_tools reaches the adapter from the spec; a negative one is refused.
func TestMaxToolsFromSpec(t *testing.T) {
	a, err := New(Spec{Type: "vllm", BaseURL: "http://127.0.0.1:1/v1", Model: "m", MaxTools: 64})
	if err != nil {
		t.Fatal(err)
	}
	if ToolLimit(a.Profile()) != 64 {
		t.Fatalf("limit %d", ToolLimit(a.Profile()))
	}
	if _, err := New(Spec{Type: "vllm", BaseURL: "http://127.0.0.1:1/v1", Model: "m", MaxTools: -1}); err == nil {
		t.Fatal("a negative max_tools was taken")
	}
}

// Two tools that would reach the provider under one name are refused, in the
// tool list and in an earlier turn's call, rather than one standing for both.
func TestAmbiguousWireNamesAreRefused(t *testing.T) {
	dotted := "mcp__x__a.b"
	twin := WireName(dotted)
	schema := json.RawMessage(`{"type":"object"}`)
	_, _, err := toWire(Request{Tools: []ToolDef{{Name: dotted, InputSchema: schema}, {Name: twin, InputSchema: schema}}}, 0, "p")
	if err == nil || !strings.Contains(err.Error(), "would reach the provider under one name") {
		t.Fatalf("tool list: %v", err)
	}
	_, _, err = toWire(Request{
		Tools:    []ToolDef{{Name: twin, InputSchema: schema}},
		Messages: []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: dotted}}}},
	}, 0, "p")
	if err == nil || !strings.Contains(err.Error(), "an earlier call") {
		t.Fatalf("history: %v", err)
	}
}

// A long name keeps its server prefix whole and 64 bits of hash.
func TestWireNameKeepsTheServerPrefix(t *testing.T) {
	name := "mcp__a_server_named_at_length__" + strings.Repeat("tool.", 20)
	w := WireName(name)
	if !strings.HasPrefix(w, "mcp__a_server_named_at_length__") || len(w) > 64 {
		t.Fatalf("%q", w)
	}
	if h := w[strings.LastIndex(w, "_")+1:]; len(h) != 16 {
		t.Fatalf("hash %q", h)
	}
}

// An earlier call by a valid name that is now a renamed tool's wire name is
// refused, not read as a call to that tool.
func TestHistoryCallByAnotherToolsWireNameIsRefused(t *testing.T) {
	dotted := "mcp__x__a.b"
	_, _, err := toWire(Request{
		Tools:    []ToolDef{{Name: dotted, InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: WireName(dotted)}}}},
	}, 0, "p")
	if err == nil || !strings.Contains(err.Error(), "an earlier call") {
		t.Fatalf("%v", err)
	}
}
