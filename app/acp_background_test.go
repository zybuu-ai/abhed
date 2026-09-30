package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// An ask made with no prompt turn open is not sent to the editor: it waits
// for the next turn, goes out inside it, and is refused if none opens in time.
func TestACPIdleAskHeldUntilPrompt(t *testing.T) {
	c, asked := editorConn(t, "allow_once")
	s := &acpSession{id: "s1", always: map[string]bool{}}
	d := abhed.Decision{Decision: policy.Ask, Step: "default", Scope: "bash(ls *)"}
	done := make(chan bool, 1)
	go func() {
		ok, _ := c.askEditor(context.Background(), s, "bash", json.RawMessage(`{"command":"ls"}`), d)
		done <- ok
	}()
	time.Sleep(150 * time.Millisecond)
	if asked.Load() != 0 {
		t.Fatal("the editor was asked with no prompt turn open")
	}
	// A prompt opens, as prompt() does.
	s.mu.Lock()
	s.cancel = func() {}
	close(s.turnOpen)
	s.turnOpen = nil
	s.mu.Unlock()
	select {
	case ok := <-done:
		if !ok || asked.Load() != 1 {
			t.Fatalf("the held ask: ok %v, asked %d", ok, asked.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the held ask never went out")
	}

	// No turn opens in time: refused by the system, never asked.
	old := heldAskWait
	heldAskWait = 50 * time.Millisecond
	defer func() { heldAskWait = old }()
	c2, asked2 := editorConn(t, "allow_once")
	s2 := &acpSession{id: "s2", always: map[string]bool{}}
	ctx, answer := agent.ExpectAnswer(context.Background())
	if ok, err := c2.askEditor(ctx, s2, "bash", json.RawMessage(`{"command":"ls"}`), d); ok || err != nil {
		t.Fatalf("an unasked ask: %v %v", ok, err)
	}
	if asked2.Load() != 0 || answer.By != agent.BySystem || !strings.Contains(answer.Reason, "no prompt turn opened") {
		t.Fatalf("asked %d, answer %+v", asked2.Load(), answer)
	}
}

// A background task's card opens when it starts and completes when its
// result arrives, with or without a turn open.
func TestACPNoticeCompletesCardOutsideTurn(t *testing.T) {
	cl := newACPClient(t, nil)
	c := cl.conn
	s := &acpSession{id: "s1", always: map[string]bool{}}
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	c.forward(s, abhed.Event{Type: agent.EvSubagentSpawned, Payload: raw(map[string]any{"background": true, "task_id": "t1", "description": "scan"})})
	c.forward(s, abhed.Event{Type: agent.EvSubagentNotice, Payload: raw(agent.Notice{TaskID: "t1", Status: "completed", Content: "all clear"})})
	var opened, completed bool
	for deadline := time.After(5 * time.Second); !opened || !completed; {
		select {
		case m := <-cl.lines:
			var p struct {
				Update map[string]any `json:"update"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if p.Update["toolCallId"] == "bg-t1" {
				opened = opened || p.Update["sessionUpdate"] == "tool_call" && p.Update["status"] == "in_progress"
				completed = completed || p.Update["sessionUpdate"] == "tool_call_update" && p.Update["status"] == "completed"
			}
		case <-deadline:
			t.Fatalf("opened %v, completed %v", opened, completed)
		}
	}
}

// cancelCounter is an agent that counts CancelTasks.
type cancelCounter struct {
	scriptedACPAgent
	cancelled atomic.Int32
}

func (a *cancelCounter) CancelTasks() int { a.cancelled.Add(1); return 0 }

// session/cancel stops the background tasks, even with no prompt open; and
// a session never runs background tasks above notify.
func TestACPCancelWithoutPromptCancelsChildren(t *testing.T) {
	var made *cancelCounter
	newACPAgent = func(_ context.Context, opts abhed.Options) (acpAgent, error) {
		made = &cancelCounter{scriptedACPAgent: scriptedACPAgent{opts: opts}}
		return made, nil
	}
	defer func() {
		newACPAgent = func(ctx context.Context, o abhed.Options) (acpAgent, error) { return abhed.New(ctx, o) }
	}()
	cl := newACPClient(t, nil)
	m := cl.request(1, "session/new", map[string]any{"cwd": t.TempDir()})
	var res struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(m.Result, &res)
	if made.opts.Background != "notify" {
		t.Fatalf("an ACP session runs background tasks as %q", made.opts.Background)
	}
	cl.notify("session/cancel", map[string]any{"sessionId": res.SessionID})
	for deadline := time.Now().Add(5 * time.Second); made.cancelled.Load() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("session/cancel with no prompt open did not cancel the background tasks")
		}
	}
}

// slowAgent keeps a prompt turn open a while and asks nothing itself.
type slowAgent struct{ scriptedACPAgent }

func (a *slowAgent) Run(ctx context.Context, _ string) (string, error) {
	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
	}
	return "ok", nil
}

// The next session/prompt is what releases a held ask.
func TestACPPromptReleasesHeldAsk(t *testing.T) {
	newACPAgent = func(_ context.Context, opts abhed.Options) (acpAgent, error) {
		return &slowAgent{scriptedACPAgent{opts: opts}}, nil
	}
	defer func() {
		newACPAgent = func(ctx context.Context, o abhed.Options) (acpAgent, error) { return abhed.New(ctx, o) }
	}()
	var asked atomic.Int32
	cl := newACPClient(t, func(method string, params json.RawMessage) any {
		if method == "session/request_permission" {
			asked.Add(1)
			return chosen(params, "allow_once")
		}
		return nil
	})
	m := cl.request(1, "session/new", map[string]any{"cwd": t.TempDir()})
	var res struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(m.Result, &res)
	s := cl.conn.session(res.SessionID)
	done := make(chan bool, 1)
	go func() {
		ok, _ := cl.conn.askEditor(context.Background(), s, "bash", json.RawMessage(`{"command":"ls"}`),
			abhed.Decision{Decision: policy.Ask, Step: "default", Scope: "bash(ls *)"})
		done <- ok
	}()
	time.Sleep(150 * time.Millisecond)
	if asked.Load() != 0 {
		t.Fatal("asked with no prompt open")
	}
	raw, _ := json.Marshal(map[string]any{"sessionId": res.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "hi"}}})
	cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage("2"), Method: "session/prompt", Params: raw})
	select {
	case ok := <-done:
		if !ok || asked.Load() != 1 {
			t.Fatalf("ok %v asked %d", ok, asked.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt did not release the held ask")
	}
}
