package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// taskAgent is a session with background tasks and nothing else.
type taskAgent struct {
	mu        sync.Mutex
	tasks     []abhed.TaskInfo
	cancelled []string
}

func (a *taskAgent) Run(context.Context, string) (string, error) { return "", nil }
func (a *taskAgent) Steer(string)                                {}
func (a *taskAgent) Flush(context.Context) error                 { return nil }
func (a *taskAgent) CancelTasks() int                            { return 0 }
func (a *taskAgent) Close()                                      {}
func (a *taskAgent) Background() []abhed.TaskInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]abhed.TaskInfo(nil), a.tasks...)
}
func (a *taskAgent) CancelTask(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, t := range a.tasks {
		if t.ID == id && t.Status == "running" {
			a.tasks[i].Status, a.tasks[i].Reason = "cancelled", "user_interrupt"
			a.cancelled = append(a.cancelled, id)
			return nil
		}
	}
	return errors.New("not running")
}

// tasksRig is a connection with one session whose agent has two running tasks.
func tasksRig(t *testing.T) (*studioClient, *acpSession, *taskAgent) {
	t.Helper()
	cl := newStudioClient(t, "/ws", false)
	a := &taskAgent{tasks: []abhed.TaskInfo{
		{ID: "t1", Description: "scan \x1b[31mred", Status: "running", Started: time.Now()},
		{ID: "t2", Description: "build", Status: "running", Started: time.Now()},
	}}
	s := &acpSession{id: "s1", cwd: "/ws", agent: a, always: map[string]bool{}}
	cl.conn.sessMu.Lock()
	cl.conn.sessions[s.id] = s
	cl.conn.sessMu.Unlock()
	return cl, s, a
}

// heldAsk asks as task would, with no prompt open, and reports the answer.
func heldAsk(c *acpConn, s *acpSession, task, request string) chan bool {
	done := make(chan bool, 1)
	go func() {
		ctx := agent.WithBackgroundTask(agent.WithRequestID(context.Background(), request), task)
		ok, _ := c.askEditor(ctx, s, "bash", json.RawMessage(`{"command":"make"}`),
			abhed.Decision{Decision: policy.Ask, Step: "default", Scope: "bash(make *)"})
		done <- ok
	}()
	return done
}

// §6.3: a held ask goes to the editor only when a person opens a review
// window for its task; it is marked held, its answer is the person's, and
// another task's ask stays held.
func TestStudioReviewReleasesHeldAsks(t *testing.T) {
	cl, s, _ := tasksRig(t)
	var asks []json.RawMessage
	var mu sync.Mutex
	cl.answering(func(method string, params json.RawMessage) any {
		mu.Lock()
		asks = append(asks, params)
		mu.Unlock()
		return chosen(params, "allow_once")
	})
	one, two := heldAsk(cl.conn, s, "t1", "r1"), heldAsk(cl.conn, s, "t2", "r2")
	deadline := time.Now().Add(5 * time.Second)
	for s.waitingAll() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	cl.ok("_abhed/tasks/list", map[string]any{"sessionId": "s1"}, &list)
	if len(list.Tasks) != 2 || list.Tasks[0]["waiting_asks"] != float64(1) {
		t.Fatalf("tasks: %v", list.Tasks)
	}
	mu.Lock()
	if len(asks) != 0 {
		t.Fatal("a held ask went to the editor with no review")
	}
	mu.Unlock()
	var rel struct {
		Released int `json:"released"`
	}
	cl.ok("_abhed/tasks/review", map[string]any{"sessionId": "s1", "taskId": "t1"}, &rel)
	if rel.Released != 1 {
		t.Fatalf("released %d", rel.Released)
	}
	select {
	case ok := <-one:
		if !ok {
			t.Fatal("the reviewed ask was refused")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reviewed ask never went out")
	}
	mu.Lock()
	if len(asks) != 1 || !strings.Contains(string(asks[0]), `"held":true`) || !strings.Contains(string(asks[0]), `"taskId":"t1"`) {
		t.Fatalf("asks: %s", asks)
	}
	mu.Unlock()
	select {
	case <-two:
		t.Fatal("another task's ask left its hold")
	case <-time.After(200 * time.Millisecond):
	}
	// Stop closes the window and refuses what is still held only when asked.
	cl.write(rpcMessage{JSONRPC: "2.0", Method: "session/cancel", Params: json.RawMessage(`{"sessionId":"s1"}`)})
	cl.ok("_abhed/tasks/review", map[string]any{"sessionId": "s1"}, &rel)
	select {
	case <-two:
	case <-time.After(5 * time.Second):
		t.Fatal("the second ask never went out")
	}
	cl.refused(errParams, "_abhed/tasks/review", map[string]any{"sessionId": "s1", "taskId": "nope"})
}

// §2.2 and §6.3: an ask out through a review window is refused, never
// approved, when the window closes before a person answers.
func TestStudioClosedReviewRefusesItsAsk(t *testing.T) {
	cl, s, _ := tasksRig(t)
	got, never := make(chan struct{}, 1), make(chan struct{})
	t.Cleanup(func() { close(never) })
	cl.answering(func(method string, params json.RawMessage) any {
		got <- struct{}{}
		<-never // nobody answers
		return nil
	})
	done := heldAsk(cl.conn, s, "t1", "r1")
	for s.waitingAll() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	cl.ok("_abhed/tasks/review", map[string]any{"sessionId": "s1"}, nil)
	<-got
	cl.write(rpcMessage{JSONRPC: "2.0", Method: "session/cancel", Params: json.RawMessage(`{"sessionId":"s1"}`)})
	select {
	case ok := <-done:
		if ok {
			t.Fatal("an unanswered held ask was approved")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the ask was left open after the window closed")
	}
}

// §6.2: one task is stopped as the person's stop; an unknown id is refused.
func TestStudioCancelOneTask(t *testing.T) {
	cl, _, a := tasksRig(t)
	from := cl.mark()
	var res struct {
		Cancelled bool `json:"cancelled"`
	}
	cl.ok("_abhed/tasks/cancel", map[string]any{"sessionId": "s1", "taskId": "t2"}, &res)
	if !res.Cancelled || len(a.cancelled) != 1 || a.cancelled[0] != "t2" {
		t.Fatalf("cancel: %v %v", res, a.cancelled)
	}
	m, _ := cl.waitFor(from, "_abhed/tasks/changed", func(m rpcMessage) bool { return m.Method == "_abhed/tasks/changed" })
	if !strings.Contains(string(m.Params), `"cancelled"`) {
		t.Fatalf("tasks/changed: %s", m.Params)
	}
	cl.refused(errParams, "_abhed/tasks/cancel", map[string]any{"sessionId": "s1", "taskId": "zz"})
	cl.refused(errParams, "_abhed/tasks/list", map[string]any{"sessionId": "other"})
}

// §6.1: a subagent in the foreground gets a card when it starts and its
// numbers when it returns; its description is shown escaped.
func TestStudioSubagentCards(t *testing.T) {
	cl, s, _ := tasksRig(t)
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	from := cl.mark()
	cl.conn.forward(s, abhed.Event{Type: agent.EvSubagentSpawned, Payload: raw(map[string]any{
		"session": "child1", "description": "look‮evil", "agent_type": "explore", "depth": 1, "model": "m-1"})})
	cl.conn.forward(s, abhed.Event{Type: agent.EvSubagentReturn, Payload: raw(map[string]any{
		"session": "child1", "reason": "completed", "turns": 3, "tokens_in": 10, "tokens_out": 5, "summary_chars": 42})})
	cl.waitFor(from, "the return", func(m rpcMessage) bool { return strings.Contains(string(m.Params), `"summaryChars":42`) })
	ups := updates(cl.since(from))
	if len(ups) != 2 || ups[0]["toolCallId"] != "sub-child1" || ups[0]["kind"] != "think" || ups[1]["status"] != "completed" {
		t.Fatalf("cards: %v", ups)
	}
	if title := ups[0]["title"].(string); strings.ContainsRune(title, '‮') || !strings.Contains(title, "explore") {
		t.Fatalf("title %q", title)
	}
	if sub := meta(ups[0])["subagent"].(map[string]any); sub["agentType"] != "explore" || sub["depth"] != float64(1) {
		t.Fatalf("meta: %v", meta(ups[0]))
	}
}

// §2.2: an answer to a held ask names an option offered for that request;
// one bound to another request is refused, not taken as an allow.
func TestRuleHeldAskBoundToItsRequest(t *testing.T) {
	cl, s, _ := tasksRig(t)
	cl.answering(func(method string, params json.RawMessage) any {
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "once:another-request"}}
	})
	done := heldAsk(cl.conn, s, "t1", "r1")
	for s.waitingAll() < 1 {
		waitABit()
	}
	cl.ok("_abhed/tasks/review", map[string]any{"sessionId": "s1", "taskId": "t1"}, nil)
	if <-done {
		t.Fatal("an option bound to another request approved the held ask")
	}
}

func waitABit() { time.Sleep(10 * time.Millisecond) }
