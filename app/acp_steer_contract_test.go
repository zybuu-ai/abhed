package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// dropHooks refuses every message, as a user_prompt_submit hook may.
type dropHooks struct{}

func (dropHooks) PromptSubmitted(context.Context, string, string) string { return "a hook refused it" }
func (dropHooks) PermissionRequested(context.Context, *policy.Engine, string, string, json.RawMessage, string) string {
	return ""
}
func (dropHooks) Observe(context.Context, string, string, string, string) {}

// §7.6: a steer is an error only when it was not queued (no such session, no
// prompt running, the session closed, empty text), and then nothing of it is
// in the record; once queued, every outcome (withdrawn, dropped, the turn
// ended, the connection closed) is a successful reply naming why it has no
// message. Studio puts the text back in the input only on an error.
func TestStudioSteerErrorsOnlyWhenNotQueued(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	s := r.cl.conn.session(id)
	parts, e := innerOf(s)
	if e != nil {
		t.Fatal(e)
	}
	userMessages := func() int {
		n := 0
		for _, ev := range r.events(id) {
			if ev.Type == agent.EvUserMessage {
				n++
			}
		}
		return n
	}
	steer := func(sid, text string) rpcMessage {
		return r.cl.call("_abhed/session/steer", map[string]any{"sessionId": sid, "text": text})
	}
	awaitQueued := func() string {
		for i := 0; ; i++ {
			if q := parts.Loop.Queued(); len(q) > 0 {
				return q[0].ID
			}
			if i > 300 {
				t.Fatal("the steer was not queued")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	queuedReply := func(m rpcMessage, why string) {
		t.Helper()
		if m.Error != nil || !strings.Contains(string(m.Result), `"userMessageSeq":null`) || !strings.Contains(string(m.Result), why) {
			t.Fatalf("want a successful reply saying %q: %+v %s", why, m.Error, m.Result)
		}
	}

	// Not queued: errors, nothing recorded.
	for name, m := range map[string]rpcMessage{
		"no such session":   steer("s-none", "faster"),
		"no prompt running": steer(id, "faster"),
	} {
		if m.Error == nil {
			t.Fatalf("%s: taken: %s", name, m.Result)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.beginTurn(ctx, func() {})
	if m := steer(id, ""); m.Error == nil {
		t.Fatalf("empty text taken: %s", m.Result)
	}
	if n := userMessages(); n != 0 {
		t.Fatalf("a refused steer left %d user.message", n)
	}

	// Queued, then withdrawn.
	done := make(chan rpcMessage, 1)
	go func() { done <- steer(id, "withdraw me") }()
	r.cl.ok("_abhed/queue/cancel", map[string]any{"sessionId": id, "id": awaitQueued()}, nil)
	queuedReply(<-done, "withdrawn")

	// Queued, then dropped by a hook at the turn's boundary.
	go func() { done <- steer(id, "drop me") }()
	awaitQueued()
	parts.Loop.Hooks = dropHooks{}
	r.model.script(say("ok"))
	if _, err := parts.Loop.RunQueued(context.Background()); err != nil {
		t.Fatal(err)
	}
	parts.Loop.Hooks = nil
	queuedReply(<-done, "hook refused")

	// Queued, then the turn ended before it was delivered.
	go func() { done <- steer(id, "too late") }()
	awaitQueued()
	s.endTurn()
	cancel()
	queuedReply(<-done, "turn ended")
	for _, q := range parts.Loop.Queued() {
		parts.Loop.Unqueue(q.ID)
	}

	// Queued, then the connection closed.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	s.beginTurn(ctx2, func() {})
	go func() { done <- steer(id, "goodbye") }()
	awaitQueued()
	r.cl.conn.inputClosed()
	queuedReply(<-done, "connection closed")
	s.endTurn()

	if n := userMessages(); n != 0 {
		t.Fatalf("%d user.message recorded for steers that never reached the model", n)
	}

	// The session closed: an error, nothing recorded.
	r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
	if m := steer(id, "after close"); m.Error == nil {
		t.Fatalf("a steer to a closed session was taken: %s", m.Result)
	}
}
