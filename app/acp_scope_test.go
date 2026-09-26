package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// editorConn is an adapter whose editor answers every permission request with
// optionID, counting the requests.
func editorConn(t *testing.T, optionID string) (*acpConn, *atomic.Int32) {
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
			res, _ := json.Marshal(map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}})
			b, _ := json.Marshal(rpcMessage{JSONRPC: "2.0", ID: m.ID, Result: res})
			_, _ = inW.Write(append(b, '\n'))
		}
	}()
	return c, &asked
}

// A call a remembered "always allow" lets through is recorded as the session
// scope that did, not as a reviewer who was never asked.
func TestACPRememberedScopeIsRecordedAsTheSessionScope(t *testing.T) {
	c, asked := editorConn(t, "reject")
	s := &acpSession{id: "s1", always: map[string]bool{"bash(mkdir *)": true}}
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
	c, _ := editorConn(t, "always")
	s := &acpSession{id: "s1", always: map[string]bool{}}
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
	c, asked := editorConn(t, "reject")
	s := &acpSession{id: "s1", always: map[string]bool{"bash(git tag *)": true}}
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

// An editor that sends "always" where it was not offered approves once and
// widens nothing.
func TestACPAlwaysNotOfferedApprovesOnce(t *testing.T) {
	c, _ := editorConn(t, "always")
	s := &acpSession{id: "s1", always: map[string]bool{}}
	ctx, answer := agent.ExpectAnswer(context.Background())
	d := abhed.Decision{Decision: policy.Ask, Step: "ask", Scope: "bash(git tag *)"}
	if ok, err := c.askEditor(ctx, s, "bash", json.RawMessage(`{"command":"git tag v1"}`), d); err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if len(s.always) != 0 || answer.Granted != "" {
		t.Fatalf("always %v answer %+v", s.always, answer)
	}
}
