package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// askedCall is one call the two-call agent asks about.
type askedCall struct {
	id   string
	args json.RawMessage
	d    abhed.Decision
}

// twoCallAgent asks about each call as the loop does: action.requested first,
// then the approver, with the call and request ids in its context.
type twoCallAgent struct {
	opts  abhed.Options
	calls []askedCall
}

func (a *twoCallAgent) Run(ctx context.Context, _ string) (string, error) {
	emit := func(t agent.EventType, id string, payload any) {
		raw, _ := json.Marshal(payload)
		a.opts.OnEvent(abhed.Event{ID: id, Type: t, Payload: raw})
	}
	for _, c := range a.calls {
		emit(agent.EvActionRequested, "ev-"+c.id, agent.ActionRequested{CallID: c.id, Tool: "bash", Args: c.args, RequiresApproval: true})
		ok, err := a.opts.Approve(agent.WithCallID(agent.WithRequestID(ctx, "ev-"+c.id), c.id), "bash", c.args, c.d)
		if err != nil {
			return "", err
		}
		if !ok {
			emit(agent.EvActionDenied, "", map[string]string{"call_id": c.id, "reason": "rejected"})
			continue
		}
		emit(agent.EvActionApproved, "", map[string]string{"call_id": c.id, "by": "reviewer"})
	}
	return "", nil
}
func (a *twoCallAgent) Steer(string)                {}
func (a *twoCallAgent) Flush(context.Context) error { return nil }
func (a *twoCallAgent) Close()                      {}

// Two calls whose arguments have the same length: an ordinary ask and a destructive one.
var sameLengthCalls = []askedCall{
	{"c1", json.RawMessage(`{"command":"ls a"}`), abhed.Decision{Decision: policy.Ask, Step: "default", Scope: "bash(ls *)", Reason: "ls needs approval"}},
	{"c2", json.RawMessage(`{"command":"rm b"}`), abhed.Decision{Decision: policy.Ask, Step: "destructive", Reason: "delete files — always requires confirmation"}},
}

// runTwoCalls drives one prompt through the two-call agent, answering each
// permission request with answer, and returns the requests and updates.
func runTwoCalls(t *testing.T, answer func(json.RawMessage) any) ([]json.RawMessage, []map[string]any) {
	t.Helper()
	newACPAgent = func(_ context.Context, opts abhed.Options) (acpAgent, error) {
		return &twoCallAgent{opts: opts, calls: sameLengthCalls}, nil
	}
	t.Cleanup(func() {
		newACPAgent = func(ctx context.Context, o abhed.Options) (acpAgent, error) { return abhed.New(ctx, o) }
	})
	var asked []json.RawMessage
	cl := newACPClient(t, func(method string, params json.RawMessage) any {
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
	return asked, updates
}

type permissionRequest struct {
	ToolCall struct {
		ToolCallID string `json:"toolCallId"`
		Meta       struct {
			Abhed struct {
				Step        string `json:"step"`
				Reason      string `json:"reason"`
				Destructive *bool  `json:"destructive"`
				RequestID   string `json:"requestId"`
				Scope       string `json:"scope"`
			} `json:"abhed"`
		} `json:"_meta"`
	} `json:"toolCall"`
}

// The permission request names the call its tool_call names, so an editor can
// put the approval on that card, and two same-length calls do not collide.
func TestACPPermissionRequestNamesItsToolCall(t *testing.T) {
	asked, updates := runTwoCalls(t, func(p json.RawMessage) any { return chosen(p, "allow_once") })
	var shown []string
	for _, u := range updates {
		if u["sessionUpdate"] == "tool_call" {
			shown = append(shown, u["toolCallId"].(string))
		}
	}
	if len(asked) != 2 || len(shown) != 2 {
		t.Fatalf("asked %d, tool calls %v", len(asked), shown)
	}
	var reqs [2]permissionRequest
	for i := range reqs {
		_ = json.Unmarshal(asked[i], &reqs[i])
		if got := reqs[i].ToolCall.ToolCallID; got != shown[i] || got != sameLengthCalls[i].id {
			t.Errorf("request %d names %q, tool_call is %q", i, got, shown[i])
		}
	}
	if reqs[0].ToolCall.ToolCallID == reqs[1].ToolCall.ToolCallID {
		t.Fatalf("same-length calls share the id %q", reqs[0].ToolCall.ToolCallID)
	}

	// The policy step, reason and class ride in _meta, so an editor can confirm harder.
	for i, want := range []struct {
		step, reason, scope string
		destructive         bool
	}{{"default", "ls needs approval", "bash(ls *)", false}, {"destructive", "delete files — always requires confirmation", "", true}} {
		m := reqs[i].ToolCall.Meta.Abhed
		if m.Step != want.step || m.Reason != want.reason || m.Scope != want.scope || m.Destructive == nil || *m.Destructive != want.destructive {
			t.Errorf("request %d _meta.abhed = %+v, want %+v", i, m, want)
		}
		if m.RequestID != "ev-"+sameLengthCalls[i].id {
			t.Errorf("request %d requestId %q", i, m.RequestID)
		}
	}
}

// An answer that names an option offered for another call is refused.
func TestACPAnswerForAnotherCallIsRefused(t *testing.T) {
	var first map[string]any
	_, updates := runTwoCalls(t, func(p json.RawMessage) any {
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

// A refused answer is recorded as refused by the system, not as a reviewer's no.
func TestACPMismatchedAnswerIsRecorded(t *testing.T) {
	c, _ := editorConn(t, "allow_once")
	s := &acpSession{id: "s1", always: map[string]bool{}}
	d := abhed.Decision{Decision: policy.Ask, Step: "default", Scope: "bash(ls *)"}
	ctx := agent.WithCallID(agent.WithRequestID(context.Background(), "ev-a"), "c1")
	if ok, err := c.askEditor(ctx, s, "bash", json.RawMessage(`{"command":"ls"}`), d); err != nil || !ok {
		t.Fatalf("own answer: ok %v err %v", ok, err)
	}
	for _, id := range []string{"once", "once:ev-b", "always:ev-b", "once:c1"} {
		ok, answer := askWith(t, s, d, id)
		if ok || answer.By != agent.BySystem {
			t.Fatalf("option %q: ok %v answer %+v", id, ok, answer)
		}
	}
	if len(s.always) != 0 {
		t.Fatalf("a refused answer widened the session: %v", s.always)
	}
}

// askWith asks once through an editor that answers with optionID.
func askWith(t *testing.T, s *acpSession, d abhed.Decision, optionID string) (bool, *agent.Answer) {
	t.Helper()
	c, _ := fixedEditorConn(t, optionID)
	ctx, answer := agent.ExpectAnswer(agent.WithCallID(agent.WithRequestID(context.Background(), "ev-a"), "c1"))
	ok, err := c.askEditor(ctx, s, "bash", json.RawMessage(`{"command":"ls"}`), d)
	if err != nil {
		t.Fatal(err)
	}
	return ok, answer
}
