package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// scriptedACPAgent stands in for a session: on Run it asks approval for one
// write, emits the events a real turn would, and answers.
type scriptedACPAgent struct {
	opts abhed.Options
	ran  []string
}

func (a *scriptedACPAgent) Run(ctx context.Context, prompt string) (string, error) {
	a.ran = append(a.ran, prompt)
	emit := func(t agent.EventType, payload any) {
		raw, _ := json.Marshal(payload)
		a.opts.OnEvent(abhed.Event{Type: t, Payload: raw})
	}
	emit(agent.EvAgentReasoning, map[string]string{"text": "I should write the file."})
	args := json.RawMessage(`{"path":"/ws/a.txt","content":"hi"}`)
	ok, err := a.opts.Approve(ctx, "write", args, abhed.Decision{Decision: policy.Ask, Step: "default", Scope: "write(/ws/a.txt)", Reason: "changing a file needs approval in default mode"})
	if err != nil {
		return "", err
	}
	emit(agent.EvActionRequested, agent.ActionRequested{CallID: "c1", Tool: "write", Args: args, RequiresApproval: true})
	if !ok {
		emit(agent.EvActionDenied, map[string]string{"call_id": "c1", "reason": "rejected"})
		emit(agent.EvAgentDelta, map[string]string{"text": "Not written."})
		return "Not written.", nil
	}
	emit(agent.EvActionApproved, map[string]string{"call_id": "c1", "by": "reviewer"})
	emit(agent.EvObservation, agent.Observation{CallID: "c1", Tool: "write", Content: "Created a.txt"})
	emit(agent.EvTodoUpdated, agent.TodoList{Items: []agent.Todo{{ID: "1", Text: "write it", Status: "done"}}})
	emit(agent.EvModelCall, agent.ModelCall{TokensIn: 1200, ContextWindow: 32768})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	emit(agent.EvAgentDelta, map[string]string{"text": "Done."})
	return "Done.", nil
}
func (a *scriptedACPAgent) Steer(string)                {}
func (a *scriptedACPAgent) Flush(context.Context) error { return nil } // OnEvent is called inline
func (a *scriptedACPAgent) Close()                      {}

// acpClient drives the adapter over pipes, the way an editor would.
type acpClient struct {
	t      *testing.T
	in     io.Writer
	lines  chan rpcMessage
	answer func(method string, params json.RawMessage) any
}

func newACPClient(t *testing.T, answer func(string, json.RawMessage) any) *acpClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &acpConn{out: outW, version: "test", base: "/ws", sessions: map[string]*acpSession{}, pending: map[int64]chan rpcMessage{}}
	cl := &acpClient{t: t, in: inW, lines: make(chan rpcMessage, 64), answer: answer}
	go func() { _ = c.serve(inR) }()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			var m rpcMessage
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			// A request from the agent is answered from the script, like a dialog.
			if m.Method != "" && m.ID != nil && cl.answer != nil {
				res, _ := json.Marshal(cl.answer(m.Method, m.Params))
				cl.write(rpcMessage{JSONRPC: "2.0", ID: m.ID, Result: res})
			}
			cl.lines <- m
		}
	}()
	return cl
}

func (cl *acpClient) write(m rpcMessage) {
	b, _ := json.Marshal(m)
	_, _ = cl.in.Write(append(b, '\n'))
}

func (cl *acpClient) request(id int, method string, params any) rpcMessage {
	cl.t.Helper()
	raw, _ := json.Marshal(params)
	cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(itoa(id)), Method: method, Params: raw})
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-cl.lines:
			if m.Method == "" && string(m.ID) == itoa(id) {
				return m
			}
		case <-deadline:
			cl.t.Fatalf("no response to %s", method)
		}
	}
}

func (cl *acpClient) collect(id int, method string, params any) (rpcMessage, []map[string]any) {
	cl.t.Helper()
	raw, _ := json.Marshal(params)
	cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(itoa(id)), Method: method, Params: raw})
	var updates []map[string]any
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-cl.lines:
			if m.Method == "session/update" {
				var p struct {
					Update map[string]any `json:"update"`
				}
				_ = json.Unmarshal(m.Params, &p)
				updates = append(updates, p.Update)
			}
			if m.Method == "" && string(m.ID) == itoa(id) {
				return m, updates
			}
		case <-deadline:
			cl.t.Fatalf("no response to %s", method)
		}
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestACPTurnIsDrivenFromTheWire(t *testing.T) {
	var made *scriptedACPAgent
	newACPAgent = func(_ context.Context, opts abhed.Options) (acpAgent, error) {
		made = &scriptedACPAgent{opts: opts}
		return made, nil
	}
	defer func() {
		newACPAgent = func(ctx context.Context, o abhed.Options) (acpAgent, error) { return abhed.New(ctx, o) }
	}()

	var asked []map[string]any
	cl := newACPClient(t, func(method string, params json.RawMessage) any {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		asked = append(asked, p)
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "always"}}
	})

	init := cl.request(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	var initRes struct {
		ProtocolVersion int `json:"protocolVersion"`
		AgentInfo       struct {
			Name string `json:"name"`
		} `json:"agentInfo"`
	}
	_ = json.Unmarshal(init.Result, &initRes)
	if initRes.ProtocolVersion != 1 || initRes.AgentInfo.Name != "abhed" {
		t.Fatalf("initialize: %s", init.Result)
	}

	created := cl.request(2, "session/new", map[string]any{"cwd": "/ws", "mcpServers": []any{}})
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(created.Result, &sess)
	if sess.SessionID == "" || made == nil || made.opts.Workspace != "/ws" {
		t.Fatalf("session/new: %s", created.Result)
	}
	// bash runs in the configured tier, as it would from the terminal.
	if !made.opts.Sandbox {
		t.Fatal("session/new did not ask for the configured sandbox")
	}

	res, updates := cl.collect(3, "session/prompt", map[string]any{"sessionId": sess.SessionID,
		"prompt": []any{map[string]any{"type": "text", "text": "write a file"},
			map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///ws/n.md", "text": "notes"}}}})
	var stop struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(res.Result, &stop)
	if stop.StopReason != "end_turn" {
		t.Fatalf("stopReason: %s", res.Result)
	}
	if len(made.ran) != 1 || !strings.Contains(made.ran[0], "write a file") || !strings.Contains(made.ran[0], "notes") {
		t.Fatalf("prompt text: %q", made.ran)
	}
	kinds := map[string]int{}
	for _, u := range updates {
		kinds[u["sessionUpdate"].(string)]++
	}
	for _, want := range []string{"agent_thought_chunk", "tool_call", "tool_call_update", "plan", "usage_update", "agent_message_chunk"} {
		if kinds[want] == 0 {
			t.Errorf("no %s update; got %v", want, kinds)
		}
	}
	if len(asked) != 1 {
		t.Fatalf("permission requests: %d", len(asked))
	}
	opts := asked[0]["options"].([]any)
	if len(opts) != 3 || opts[1].(map[string]any)["kind"] != "allow_always" {
		t.Fatalf("permission options: %v", opts)
	}

	// The editor chose "always": the same scope is not asked again.
	_, _ = cl.collect(4, "session/prompt", map[string]any{"sessionId": sess.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "again"}}})
	if len(asked) != 1 {
		t.Fatalf("asked again after allow_always: %d", len(asked))
	}
}

// A rejection in the editor is a denial, and the turn still ends cleanly.
func TestACPRejectionIsADenial(t *testing.T) {
	newACPAgent = func(_ context.Context, opts abhed.Options) (acpAgent, error) {
		return &scriptedACPAgent{opts: opts}, nil
	}
	defer func() {
		newACPAgent = func(ctx context.Context, o abhed.Options) (acpAgent, error) { return abhed.New(ctx, o) }
	}()
	cl := newACPClient(t, func(string, json.RawMessage) any {
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "reject"}}
	})
	cl.request(1, "initialize", map[string]any{"protocolVersion": 1})
	created := cl.request(2, "session/new", map[string]any{"cwd": "/ws"})
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(created.Result, &sess)
	res, updates := cl.collect(3, "session/prompt", map[string]any{"sessionId": sess.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "x"}}})
	if !strings.Contains(string(res.Result), "end_turn") {
		t.Fatalf("%s", res.Result)
	}
	failed := false
	for _, u := range updates {
		failed = failed || (u["sessionUpdate"] == "tool_call_update" && u["status"] == "failed")
	}
	if !failed {
		t.Fatalf("the denial did not reach the editor: %v", updates)
	}
}

func TestACPUnknownMethodAndSession(t *testing.T) {
	cl := newACPClient(t, nil)
	if m := cl.request(1, "session/bogus", nil); m.Error == nil || m.Error.Code != -32601 {
		t.Fatalf("unknown method: %+v", m)
	}
	if m := cl.request(2, "session/prompt", map[string]any{"sessionId": "nope"}); m.Error == nil {
		t.Fatalf("unknown session accepted: %+v", m)
	}
}
