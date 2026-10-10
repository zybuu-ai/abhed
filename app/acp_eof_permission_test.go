package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// A permission request open when stdin closes fails as cancelled even before
// the turn's own context is: the loop must not take it for an approval error.
func TestACPPermissionAskFailsAsCancelledWhenInputCloses(t *testing.T) {
	asked := make(chan struct{}, 1)
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	c, _ := answeringConn(t, func(json.RawMessage) any {
		asked <- struct{}{}
		<-hold
		return nil
	})
	s := &acpSession{id: "s1", cancel: func() {}, always: map[string]bool{}}
	d := abhed.Decision{Decision: policy.Ask, Step: "default", Scope: "bash(touch *)"}
	go func() {
		<-asked
		c.inputClosed()
	}()
	_, err := c.askEditor(context.Background(), s, "bash", json.RawMessage(`{"command":"touch a"}`), d)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ask after stdin closed: %v, want it to read as cancelled", err)
	}
}

// stdin closing while a permission request waits ends the turn as cancelled,
// never as an error: the request and the turn both see the same interrupt.
func TestACPInputClosedDuringPermissionIsCancelled(t *testing.T) {
	r := newStudioRig(t, "")
	r.model.script(callTool("c1", "bash", map[string]any{"command": "touch acp-perm.txt"}), say("done"))
	asked := make(chan struct{}, 1)
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	r.cl.answering(func(method string, _ json.RawMessage) any {
		if method == "session/request_permission" {
			asked <- struct{}{}
			<-hold
		}
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
	})
	id := r.open()
	from := r.cl.mark()
	raw, _ := json.Marshal(map[string]any{"sessionId": id, "prompt": []any{map[string]any{"type": "text", "text": "touch it"}}})
	r.cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(`"p1"`), Method: "session/prompt", Params: raw})
	<-asked
	_ = r.cl.in.(io.Closer).Close()

	reply := r.cl.waitFor(from, "reply to session/prompt", func(m rpcMessage) bool { return m.Method == "" && string(m.ID) == `"p1"` })
	var res struct {
		StopReason string `json:"stopReason"`
	}
	if reply.Error != nil || json.Unmarshal(reply.Result, &res) != nil || res.StopReason != "cancelled" {
		t.Fatalf("prompt reply %s %+v, want stopReason cancelled", reply.Result, reply.Error)
	}
	var ended *agent.SessionEnded
	for _, ev := range r.events(id) {
		switch ev.Type {
		case agent.EvActionDenied:
			var d map[string]string
			_ = json.Unmarshal(ev.Payload, &d)
			if d["reason"] != "interrupted before an answer" {
				t.Fatalf("the unanswered request was denied as %q", d["reason"])
			}
		case agent.EvSessionEnded:
			ended = new(agent.SessionEnded)
			_ = json.Unmarshal(ev.Payload, ended)
		}
	}
	if ended == nil || ended.Reason != agent.TermUserInterrupt {
		t.Fatalf("session.ended %+v, want user_interrupt", ended)
	}
}
