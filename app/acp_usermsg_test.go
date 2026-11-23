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

// Contract: across prompts and steers in one session, every reply's
// userMessageSeq is the seq of that message's own user.message in the
// record, and names the session.
func TestACPMessageSeqContract(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	s := r.cl.conn.session(id)
	parts, e := innerOf(s)
	if e != nil {
		t.Fatal(e)
	}
	type sent struct {
		text string
		seq  int64
	}
	var got []sent
	seqOf := func(m rpcMessage) int64 {
		var res struct {
			Meta map[string]map[string]any `json:"_meta"`
		}
		_ = json.Unmarshal(m.Result, &res)
		b := res.Meta[acpMetaKey]
		if b["sessionId"] != id {
			t.Fatalf("the reply's meta names session %v, want %s: %s", b["sessionId"], id, m.Result)
		}
		f, ok := b["userMessageSeq"].(float64)
		if !ok {
			t.Fatalf("no seq in %s", m.Result)
		}
		return int64(f)
	}
	for i := range 3 {
		text := "prompt " + strconv.Itoa(i)
		r.model.script(say("ok"))
		m := r.cl.call("session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]any{"type": "text", "text": text}}})
		if m.Error != nil {
			t.Fatal(m.Error)
		}
		got = append(got, sent{text, seqOf(m)})

		steer := "steer " + strconv.Itoa(i)
		ctx, cancel := context.WithCancel(context.Background())
		s.beginTurn(ctx, cancel)
		replied := make(chan rpcMessage, 1)
		go func() { replied <- r.cl.call("_abhed/session/steer", map[string]any{"sessionId": id, "text": steer}) }()
		for j := 0; len(parts.Loop.Queued()) == 0; j++ {
			if j > 200 {
				t.Fatal("the steer was not queued")
			}
			time.Sleep(10 * time.Millisecond)
		}
		r.model.script(say("ok"))
		if _, err := parts.Loop.RunQueued(context.Background()); err != nil {
			t.Fatal(err)
		}
		m = <-replied
		s.endTurn()
		cancel()
		if m.Error != nil {
			t.Fatal(m.Error)
		}
		got = append(got, sent{steer, seqOf(m)})
	}
	bySeq := map[int64]string{}
	for _, ev := range r.events(id) {
		if ev.Type == agent.EvUserMessage {
			var msg agent.Message
			_ = json.Unmarshal(ev.Payload, &msg)
			bySeq[ev.Seq] = msg.Text
		}
	}
	for _, g := range got {
		if bySeq[g.seq] != g.text {
			t.Fatalf("reply seq %d is %q in the record, want %q", g.seq, bySeq[g.seq], g.text)
		}
	}
}

// The steer reply never waits on nothing: with an MCP restart busy and no
// prompt running it is refused at once, and a steer waiting when its session
// closes is answered.
func TestACPSteerReplyNeverHangs(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	s := r.cl.conn.session(id)
	s.mu.Lock()
	s.mcpRestart = true
	s.mu.Unlock()
	done := make(chan rpcMessage, 1)
	go func() { done <- r.cl.call("_abhed/session/steer", map[string]any{"sessionId": id, "text": "faster"}) }()
	select {
	case m := <-done:
		if m.Error == nil {
			t.Fatalf("a steer with no prompt running was taken: %s", m.Result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the steer's reply hung with no turn open")
	}
	s.mu.Lock()
	s.mcpRestart = false
	s.mu.Unlock()

	// Waiting in an open turn, then the session closes.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.beginTurn(ctx, func() {})
	go func() { done <- r.cl.call("_abhed/session/steer", map[string]any{"sessionId": id, "text": "slower"}) }()
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
	r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
	select {
	case m := <-done:
		if m.Error != nil || !strings.Contains(string(m.Result), `"userMessageSeq":null`) || !strings.Contains(string(m.Result), "session closed") {
			t.Fatalf("reply %+v %s", m.Error, m.Result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the steer's reply hung after its session closed")
	}
}

// On the default configuration with nothing to reach, k8s_login costs the
// model nothing until a login is pasted; from then on it is offered.
func TestK8sLoginIsOfferedOnceALoginIsPasted(t *testing.T) {
	r := newStudioRig(t, "")
	r.model.script(say("hi"), say("I can log you in"))
	id := r.open()
	r.prompt(id, "hello")
	r.prompt(id, "oc login --token=sha256~Zq1cP3xYv8mN0aB4dE6fG7hI9jK2lM5nO8pQrStUvWx --server=https://api.lab.example:6443")
	r.model.mu.Lock()
	bodies := append([]string(nil), r.model.bodies...)
	r.model.mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("%d model calls", len(bodies))
	}
	if strings.Contains(bodies[0], `"name":"k8s_login"`) {
		t.Fatal("k8s_login was offered before anything was pasted")
	}
	// The last call may be a suggestion's, which offers no tools: any call
	// after the paste is the turn's.
	offered := false
	for _, b := range bodies[1:] {
		offered = offered || strings.Contains(b, `"name":"k8s_login"`)
	}
	if !offered {
		t.Fatal("k8s_login was not offered once a login was pasted")
	}
}
