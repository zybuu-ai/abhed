package app

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A prompt's message is announced with its seq as soon as it is recorded,
// and its reply names the same seq.
func TestACPPromptNamesItsMessageSeq(t *testing.T) {
	r := newStudioRig(t, "")
	r.model.script(say("hi"))
	id := r.open()
	from := r.cl.mark()
	m := r.cl.call("session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]any{"type": "text", "text": "hello"}}})
	if m.Error != nil {
		t.Fatal(m.Error)
	}
	var seq int64
	for _, ev := range r.events(id) {
		if ev.Type == agent.EvUserMessage {
			seq = ev.Seq
		}
	}
	want := `"userMessageSeq":` + strconv.FormatInt(seq, 10)
	if seq == 0 || !strings.Contains(string(m.Result), want) {
		t.Fatalf("reply %s, want %s", m.Result, want)
	}
	announced := false
	for _, n := range r.cl.since(from) {
		if n.Method == "_abhed/session/userMessage" {
			announced = strings.Contains(string(n.Params), want) && strings.Contains(string(n.Params), `"requestId":`+string(m.ID))
		}
	}
	if !announced {
		t.Fatal("the message's seq was not announced with the prompt's request id")
	}
}

// A prompt that records no message (a command) says so, with seq null.
func TestACPCommandPromptHasNoMessageSeq(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	m := r.cl.call("session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]any{"type": "text", "text": "/diff"}}})
	if m.Error != nil || !strings.Contains(string(m.Result), `"userMessageSeq":null`) || !strings.Contains(string(m.Result), "userMessageReason") {
		t.Fatalf("reply %+v %s", m.Error, m.Result)
	}
}

// A steer's reply comes when its message is recorded, with that seq.
func TestACPSteerReplyNamesItsMessageSeq(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	r.prompt(id, "start")
	s := r.cl.conn.session(id)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.beginTurn(ctx, cancel)
	replied := make(chan rpcMessage, 1)
	go func() {
		replied <- r.cl.call("_abhed/session/steer", map[string]any{"sessionId": id, "text": "faster"})
	}()
	parts, e := innerOf(s)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; len(parts.Loop.Queued()) == 0; i++ {
		if i > 200 {
			t.Fatal("the steer was not queued")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.model.script(say("ok"))
	if _, err := parts.Loop.RunQueued(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := <-replied
	s.endTurn()
	var seq int64
	for _, ev := range r.events(id) {
		if ev.Type == agent.EvUserMessage {
			var msg agent.Message
			_ = json.Unmarshal(ev.Payload, &msg)
			if msg.Steered {
				seq = ev.Seq
			}
		}
	}
	if m.Error != nil || seq == 0 || !strings.Contains(string(m.Result), `"userMessageSeq":`+strconv.FormatInt(seq, 10)) {
		t.Fatalf("steer reply %+v %s (seq %d)", m.Error, m.Result, seq)
	}
}
