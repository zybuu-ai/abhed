package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// editorConn is an adapter whose editor answers every permission request with
// the option of that kind, counting the requests. An "always" it was not
// offered is sent anyway, bound to the request as the offered ones are.
func editorConn(t *testing.T, kind string) (*acpConn, *atomic.Int32) {
	return answeringConn(t, func(params json.RawMessage) any {
		choice := chosen(params, kind)
		if kind == "allow_always" && choice["outcome"].(map[string]any)["outcome"] == "cancelled" {
			once := chosen(params, "allow_once")["outcome"].(map[string]any)["optionId"].(string)
			choice = map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "always:" + strings.TrimPrefix(once, "once:")}}
		}
		return choice
	})
}

func answeringConn(t *testing.T, answer func(json.RawMessage) any) (*acpConn, *atomic.Int32) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	t.Cleanup(func() { _ = inW.Close(); _ = outR.Close() })
	c := &acpConn{out: outW, version: "test", base: "/ws", sessions: map[string]*acpSession{}, pending: map[int64]chan rpcMessage{}}
	go func() { _ = c.serve(inR) }()
	var asked atomic.Int32
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var m rpcMessage
			if json.Unmarshal(sc.Bytes(), &m) != nil || m.Method != "session/request_permission" {
				continue
			}
			asked.Add(1)
			res, _ := json.Marshal(answer(m.Params))
			b, _ := json.Marshal(rpcMessage{JSONRPC: "2.0", ID: m.ID, Result: res})
			_, _ = inW.Write(append(b, '\n'))
		}
	}()
	return c, &asked
}

// A call a remembered "always allow" lets through is recorded as the session
// scope that did, not as a reviewer who was never asked.
func TestACPRememberedScopeIsRecordedAsTheSessionScope(t *testing.T) {
	c, asked := editorConn(t, "reject_once")
	s := &acpSession{id: "s1", cancel: func() {}, always: map[string]bool{"bash(mkdir *)": true}}
	ctx, answer := agent.ExpectAnswer(context.Background())
	d := abhed.Decision{Decision: policy.Ask, Step: "default", Scope: "bash(mkdir *)"}
	ok, err := c.askEditor(ctx, s, "bash", json.RawMessage(`{"command":"mkdir b"}`), d)
	if err != nil || !ok || asked.Load() != 0 {
		t.Fatalf("remembered scope: ok %v err %v, asked %d", ok, err, asked.Load())
	}
	if answer.By != agent.BySessionScope || answer.Scope != "bash(mkdir *)" {
		t.Fatalf("answer %+v, want by session-scope with the scope", answer)
	}
}

// Choosing "always" is recorded on the approval that granted it.
func TestACPAlwaysRecordsTheGrantedScope(t *testing.T) {
	c, _ := editorConn(t, "allow_always")
	s := &acpSession{id: "s1", cancel: func() {}, always: map[string]bool{}}
	ctx, answer := agent.ExpectAnswer(context.Background())
	d := abhed.Decision{Decision: policy.Ask, Step: "default", Scope: "bash(mkdir *)"}
	if ok, err := c.askEditor(ctx, s, "bash", json.RawMessage(`{"command":"mkdir a"}`), d); err != nil || !ok {
		t.Fatalf("always: ok %v err %v", ok, err)
	}
	if answer.By != agent.ByReviewer || answer.Granted != "bash(mkdir *)" || !s.always["bash(mkdir *)"] {
		t.Fatalf("answer %+v always %v", answer, s.always)
	}
}

// An ask rule asks every time, whatever the editor chose to always allow.
func TestACPAskRuleIgnoresARememberedScope(t *testing.T) {
	c, asked := editorConn(t, "reject_once")
	s := &acpSession{id: "s1", cancel: func() {}, always: map[string]bool{"bash(git tag *)": true}}
	for _, d := range []abhed.Decision{
		{Decision: policy.Ask, Step: "ask", Scope: "bash(git tag *)", Reason: "matched ask rule bash(git tag*)"},
		{Decision: policy.Ask, Step: "destructive", Scope: "bash(git tag *)", Reason: "delete or replace a tag"},
	} {
		before := asked.Load()
		ok, err := c.askEditor(context.Background(), s, "bash", json.RawMessage(`{"command":"git tag -d v1"}`), d)
		if err != nil || ok || asked.Load() != before+1 {
			t.Fatalf("step %s: ok %v err %v, asked %d times", d.Step, ok, err, asked.Load()-before)
		}
	}
}

// An editor that sends "always" where it was withheld is refused by the
// system and widens nothing, for an ask rule and a destructive command alike.
func TestACPAlwaysNotOfferedIsRefused(t *testing.T) {
	for _, d := range []abhed.Decision{
		{Decision: policy.Ask, Step: "ask", Scope: "bash(git tag *)"},
		{Decision: policy.Ask, Step: "destructive", Reason: "delete or replace a tag"},
	} {
		c, _ := editorConn(t, "allow_always")
		s := &acpSession{id: "s1", cancel: func() {}, always: map[string]bool{}}
		ctx, answer := agent.ExpectAnswer(context.Background())
		ok, err := c.askEditor(ctx, s, "bash", json.RawMessage(`{"command":"git tag -d v1"}`), d)
		if err != nil || ok || answer.By != agent.BySystem {
			t.Fatalf("step %s: ok %v err %v answer %+v", d.Step, ok, err, answer)
		}
		if len(s.always) != 0 || answer.Granted != "" {
			t.Fatalf("step %s: always %v answer %+v", d.Step, s.always, answer)
		}
	}
}
