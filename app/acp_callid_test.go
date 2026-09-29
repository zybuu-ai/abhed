package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// askedCall is one call the scripted agent asks about. recorded, when set,
// is the request as the record holds it, standing in for a redacted copy.
type askedCall struct {
	id       string
	args     json.RawMessage
	d        abhed.Decision
	recorded agent.ActionRequested
}

// callsAgent asks about each call as the loop does, and delivers its events
// only when flushed, as the SDK's forwarder goroutine may.
type callsAgent struct {
	opts  abhed.Options
	calls []askedCall
	mu    sync.Mutex
	queue []abhed.Event
}

func (a *callsAgent) emit(t agent.EventType, id string, payload any) {
	raw, _ := json.Marshal(payload)
	a.mu.Lock()
	a.queue = append(a.queue, abhed.Event{ID: id, Type: t, Payload: raw})
	a.mu.Unlock()
}

func (a *callsAgent) Run(ctx context.Context, _ string) (string, error) {
	for _, c := range a.calls {
		asked := c.recorded
		if asked.Args == nil {
			asked.Args, asked.Reason, asked.Scope = c.args, c.d.Reason, c.d.Offer()
		}
		asked.CallID, asked.Tool, asked.RequiresApproval = c.id, "bash", true
		a.emit(agent.EvActionRequested, "ev-"+c.id, asked)
		raw, _ := json.Marshal(asked)
		actx := agent.WithRequested(agent.WithCallID(agent.WithRequestID(ctx, "ev-"+c.id), c.id),
			abhed.Event{ID: "ev-" + c.id, Type: agent.EvActionRequested, Payload: raw})
		ok, err := a.opts.Approve(actx, "bash", c.args, c.d)
		if err != nil {
			return "", err
		}
		if !ok {
			a.emit(agent.EvActionDenied, "", map[string]string{"call_id": c.id, "reason": "rejected"})
			continue
		}
		a.emit(agent.EvActionApproved, "", map[string]string{"call_id": c.id, "by": "reviewer"})
	}
	return "", nil
}

func (a *callsAgent) Flush(context.Context) error {
	a.mu.Lock()
	q := a.queue
	a.queue = nil
	a.mu.Unlock()
	for _, ev := range q {
		a.opts.OnEvent(ev)
	}
	return nil
}
func (a *callsAgent) Steer(string) {}
func (a *callsAgent) Close()       {}

// Three calls whose arguments have the same length: an ordinary ask, a
// destructive one, and an ask rule, which offers no scope but is not destructive.
var sameLengthCalls = []askedCall{
	{id: "c1", args: json.RawMessage(`{"command":"ls a"}`), d: abhed.Decision{Decision: policy.Ask, Step: "default", Scope: "bash(ls *)", Reason: "ls needs approval"}},
	{id: "c2", args: json.RawMessage(`{"command":"rm b"}`), d: abhed.Decision{Decision: policy.Ask, Step: "destructive", Reason: "delete files — always requires confirmation"}},
	{id: "c3", args: json.RawMessage(`{"command":"gh c"}`), d: abhed.Decision{Decision: policy.Ask, Step: "ask", Reason: "matched ask rule bash(gh *)"}},
}

// runCalls drives one prompt through a scripted agent making calls, answering
// each permission request with answer.
func runCalls(t *testing.T, calls []askedCall, answer func(json.RawMessage) any) (*acpClient, []json.RawMessage, []map[string]any) {
	t.Helper()
	newACPAgent = func(_ context.Context, opts abhed.Options) (acpAgent, error) {
		return &callsAgent{opts: opts, calls: calls}, nil
	}
	t.Cleanup(func() {
		newACPAgent = func(ctx context.Context, o abhed.Options) (acpAgent, error) { return abhed.New(ctx, o) }
	})
	var asked []json.RawMessage
	cl := newACPClient(t, func(_ string, params json.RawMessage) any {
		asked = append(asked, params)
		return answer(params)
	})
	cl.request(1, "initialize", map[string]any{"protocolVersion": 1})
	created := cl.request(2, "session/new", map[string]any{"cwd": "/ws"})
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(created.Result, &sess)
	_, updates := cl.prompt(3, map[string]any{"sessionId": sess.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "x"}}})
	return cl, asked, updates
}

type abhedMeta struct {
	Tool        string `json:"tool"`
	Step        string `json:"step"`
	Reason      string `json:"reason"`
	Destructive *bool  `json:"destructive"`
	RequestID   string `json:"requestId"`
	Scope       string `json:"scope"`
}

type permissionRequest struct {
	ToolCall struct {
		ToolCallID string                     `json:"toolCallId"`
		Title      string                     `json:"title"`
		RawInput   json.RawMessage            `json:"rawInput"`
		Meta       map[string]json.RawMessage `json:"_meta"`
	} `json:"toolCall"`
}

func (p permissionRequest) meta() (abhedMeta, bool) {
	var m abhedMeta
	raw, ok := p.ToolCall.Meta[acpMetaKey]
	_ = json.Unmarshal(raw, &m)
	return m, ok
}

// The permission request names the call its tool_call names, arrives after
// that tool_call, and same-length calls do not collide.
func TestACPPermissionRequestNamesItsToolCall(t *testing.T) {
	cl, asked, updates := runCalls(t, sameLengthCalls, func(p json.RawMessage) any { return chosen(p, "allow_once") })
	var shown []string
	for _, u := range updates {
		if u["sessionUpdate"] == "tool_call" {
			shown = append(shown, u["toolCallId"].(string))
			if _, ok := u["name"]; ok {
				t.Errorf("tool_call carries a root-level name, which the spec does not allow: %v", u)
			}
		}
	}
	if len(asked) != 3 || len(shown) != 3 {
		t.Fatalf("asked %d, tool calls %v", len(asked), shown)
	}
	ids := map[string]bool{}
	for i := range asked {
		var r permissionRequest
		_ = json.Unmarshal(asked[i], &r)
		if got := r.ToolCall.ToolCallID; got != shown[i] || got != sameLengthCalls[i].id {
			t.Errorf("request %d names %q, tool_call is %q", i, got, shown[i])
		}
		ids[r.ToolCall.ToolCallID] = true
	}
	if len(ids) != 3 {
		t.Fatalf("same-length calls share ids: %v", ids)
	}
	want := []string{"call c1", "ask c1", "call c2", "ask c2", "call c3", "ask c3"}
	if got := cl.arrived(); !slices.Equal(got, want) {
		t.Fatalf("editor read %v, want each tool_call before its request %v", got, want)
	}
}

// The policy step, reason and class ride in a namespaced _meta, so an editor
// can confirm a destructive call harder; "no scope" is not "destructive".
func TestACPPermissionRequestCarriesThePolicyDecision(t *testing.T) {
	_, asked, _ := runCalls(t, sameLengthCalls, func(p json.RawMessage) any { return chosen(p, "allow_once") })
	if len(asked) != 3 {
		t.Fatalf("asked %d", len(asked))
	}
	for i, want := range []struct {
		step, reason, scope string
		destructive         bool
	}{
		{"default", "ls needs approval", "bash(ls *)", false},
		{"destructive", "delete files — always requires confirmation", "", true},
		{"ask", "matched ask rule bash(gh *)", "", false},
	} {
		var r permissionRequest
		_ = json.Unmarshal(asked[i], &r)
		m, ok := r.meta()
		if !ok || m.Tool != "bash" || m.Step != want.step || m.Reason != want.reason || m.Scope != want.scope ||
			m.Destructive == nil || *m.Destructive != want.destructive {
			t.Errorf("request %d _meta[%q] = %+v (present %v), want %+v", i, acpMetaKey, m, ok, want)
		}
		if m.RequestID != "ev-"+sameLengthCalls[i].id {
			t.Errorf("request %d requestId %q", i, m.RequestID)
		}
	}
}

// The editor is shown the request as recorded, so what the record redacted
// does not reach the dialog.
func TestACPPermissionRequestShowsTheRecordedCopy(t *testing.T) {
	calls := []askedCall{{
		id: "c1", args: json.RawMessage(`{"command":"curl -H tok-live-123 x"}`),
		d: abhed.Decision{Decision: policy.Ask, Step: "monitor", Reason: "it sends tok-live-123 out"},
		recorded: agent.ActionRequested{Args: json.RawMessage(`{"command":"curl -H [secret:TOK] x"}`),
			Reason: "it sends [secret:TOK] out"},
	}}
	_, asked, _ := runCalls(t, calls, func(p json.RawMessage) any { return chosen(p, "reject_once") })
	if len(asked) != 1 || strings.Contains(string(asked[0]), "tok-live-123") {
		t.Fatalf("the request carries what the record redacted: %s", asked)
	}
	var r permissionRequest
	_ = json.Unmarshal(asked[0], &r)
	m, _ := r.meta()
	if m.Reason != "it sends [secret:TOK] out" || !strings.Contains(string(r.ToolCall.RawInput), "[secret:TOK]") ||
		!strings.Contains(r.ToolCall.Title, "[secret:TOK]") {
		t.Fatalf("reason %q rawInput %s title %q", m.Reason, r.ToolCall.RawInput, r.ToolCall.Title)
	}
}

// An answer that names an option offered for another call is refused.
func TestACPAnswerForAnotherCallIsRefused(t *testing.T) {
	var first any
	_, _, updates := runCalls(t, sameLengthCalls[:2], func(p json.RawMessage) any {
		if first == nil {
			first = chosen(p, "allow_once")
		}
		return first
	})
	status := map[string]string{}
	for _, u := range updates {
		if u["sessionUpdate"] == "tool_call_update" {
			status[u["toolCallId"].(string)] = u["status"].(string)
		}
	}
	if status["c1"] != "in_progress" || status["c2"] != "failed" {
		t.Fatalf("statuses %v: c1 answered for itself, c2 with c1's answer", status)
	}
}

// Every answer that is not one of this call's offered options, including an
// "always" that was withheld, a cancel, or a reply that cannot be read, is a
// denial recorded as the system's, not a reviewer's.
func TestACPAnswersNotOfferedAreRefusedBySystem(t *testing.T) {
	sel := func(id string) any {
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": id}}
	}
	defaultAsk := abhed.Decision{Decision: policy.Ask, Step: "default", Scope: "bash(ls *)"}
	destructive := abhed.Decision{Decision: policy.Ask, Step: "destructive", Reason: "delete files — always requires confirmation"}
	askRule := abhed.Decision{Decision: policy.Ask, Step: "ask", Scope: "bash(git tag *)", Reason: "matched ask rule bash(git tag*)"}
	for _, tc := range []struct {
		name  string
		d     abhed.Decision
		reply any
	}{
		{"unbound once", defaultAsk, sel("once")},
		{"another request", defaultAsk, sel("once:ev-b")},
		{"another request's always", defaultAsk, sel("always:ev-b")},
		{"bound to the call id", defaultAsk, sel("once:c1")},
		{"withheld always, destructive", destructive, sel("always:ev-a")},
		{"withheld always, ask rule", askRule, sel("always:ev-a")},
		{"cancelled with a valid option", defaultAsk, map[string]any{"outcome": map[string]any{"outcome": "cancelled", "optionId": "once:ev-a"}}},
		{"unknown outcome", defaultAsk, map[string]any{"outcome": map[string]any{"outcome": "maybe", "optionId": "once:ev-a"}}},
		{"unreadable", defaultAsk, "yes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &acpSession{id: "s1", always: map[string]bool{}}
			c, _ := answeringConn(t, func(json.RawMessage) any { return tc.reply })
			ctx, answer := agent.ExpectAnswer(agent.WithCallID(agent.WithRequestID(context.Background(), "ev-a"), "c1"))
			ok, err := c.askEditor(ctx, s, "bash", json.RawMessage(`{"command":"ls"}`), tc.d)
			if err != nil || ok || answer.By != agent.BySystem || answer.Reason == "" {
				t.Fatalf("ok %v err %v answer %+v, want a denial by the system", ok, err, answer)
			}
			if len(s.always) != 0 {
				t.Fatalf("a refused answer widened the session: %v", s.always)
			}
		})
	}
}

// Through the SDK's real forwarder, the tool_call reaches the editor before the
// permission request that names it.
func TestACPRealAgentShowsTheToolCallFirst(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var turns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frame := `{"choices":[{"delta":{"content":"done"}}]}`
		if turns.Add(1) == 1 {
			args, _ := json.Marshal(map[string]string{"path": filepath.Join(ws, "a.txt"), "content": "x\n"})
			call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "id": "call_w1", "type": "function",
					"function": map[string]any{"name": "write", "arguments": string(args)}}}}}}})
			frame = string(call)
		}
		fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", frame)
	}))
	defer srv.Close()
	cfg := `{"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + srv.URL + `","model":"m","context_window":8192}}}}`
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var asked []json.RawMessage
	cl := newACPClient(t, func(_ string, params json.RawMessage) any {
		asked = append(asked, params)
		return chosen(params, "reject_once")
	})
	cl.request(1, "initialize", map[string]any{"protocolVersion": 1})
	created := cl.request(2, "session/new", map[string]any{"cwd": ws})
	if created.Error != nil {
		t.Skipf("no session here: %s", created.Error.Message)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(created.Result, &sess)
	_, _ = cl.prompt(3, map[string]any{"sessionId": sess.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "write a.txt"}}})
	if got, want := cl.arrived(), []string{"call call_w1", "ask call_w1"}; !slices.Equal(got, want) {
		t.Fatalf("editor read %v, want %v", got, want)
	}
	var r permissionRequest
	_ = json.Unmarshal(asked[0], &r)
	if m, ok := r.meta(); !ok || m.Step != "default" || m.RequestID == "" || m.Destructive == nil || *m.Destructive {
		t.Fatalf("_meta %+v", m)
	}
}
