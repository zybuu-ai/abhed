package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/monitor"
)

func toolMessages(l *Loop, turn int) []string {
	var out []string
	for _, m := range l.Adapter.(*scriptedAdapter).gotRequests[turn].Messages {
		if m.Role == model.RoleTool {
			out = append(out, m.Content)
		}
	}
	return out
}

func eventsOf(t *testing.T, store *MemStore, typ EventType) []json.RawMessage {
	t.Helper()
	evs, _ := store.Events("sess1")
	var out []json.RawMessage
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e.Payload)
		}
	}
	return out
}

// The rules judge refuses destructive drift that bypass mode waved through,
// and the model is told the concern and what to do instead.
func TestSupervisorCorrectsDrift(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "rm -rf /srv/data", "description": "free space"})}},
		{text: "done"},
	}, policy.ModeBypass, true)
	l.Monitor = &monitor.Guard{Monitor: monitor.Rules{}}
	l.Run(context.Background(), "update the README")

	got := toolMessages(l, 1)
	if len(got) != 1 || !strings.HasPrefix(got[0], "Denied. Supervisor: out-of-remit.") || !strings.Contains(got[0], "Do this instead: ") ||
		!strings.Contains(got[0], `"update the README"`) {
		t.Fatalf("the model was told %q", got)
	}
	if len(eventsOf(t, store, EvObservation)) > 0 {
		t.Fatal("the refused command ran")
	}
	fixes := eventsOf(t, store, EvSupervisorCorrection)
	if len(fixes) != 1 || !strings.Contains(string(fixes[0]), `"code":"out-of-remit"`) {
		t.Fatalf("corrections %s", fixes)
	}
	reqs := eventsOf(t, store, EvActionRequested)
	if len(reqs) != 1 || !strings.Contains(string(reqs[0]), `"supervisor":{"decision":"deny","code":"out-of-remit"`) {
		t.Fatalf("the request carries no verdict: %s", reqs)
	}
}

// A concern on a call policy already asks about leaves the ask an ask; when
// nobody approves, the refusal carries the correction.
func TestSupervisorCorrectionOnRefusedAsk(t *testing.T) {
	ask := monitor.Func(func(context.Context, monitor.Case) (monitor.Verdict, error) {
		return monitor.Verdict{Decision: policy.Ask, Code: "unverified-target", Rationale: "nothing showed /etc/x", Advice: "inspect first"}, nil
	})
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "touch /etc/x", "description": "x"})}},
		{text: "done"},
	}, policy.ModeDefault, false)
	l.Monitor = &monitor.Guard{Monitor: ask}
	l.Run(context.Background(), "set it up")
	got := toolMessages(l, 1)
	if len(got) != 1 || !strings.Contains(got[0], "Supervisor: unverified-target. nothing showed /etc/x. Do this instead: inspect first.") {
		t.Fatalf("the model was told %q", got)
	}
	if len(eventsOf(t, store, EvSupervisorCorrection)) != 1 {
		t.Fatal("no supervisor.correction")
	}
}

// After three refusals in a row the model is reminded of the task and the
// approved plan, and the note is recorded.
func TestSupervisorReplanNudge(t *testing.T) {
	deny := monitor.Func(func(context.Context, monitor.Case) (monitor.Verdict, error) {
		return monitor.Verdict{Decision: policy.Deny, Code: "out-of-remit", Rationale: "no", Advice: "stay on task"}, nil
	})
	bash := func(n string) model.ToolCall {
		c := call("bash", map[string]string{"command": "make " + n, "description": n})
		c.ID = "c" + n
		return c
	}
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{bash("a"), bash("b"), bash("c")}},
		{text: "done"},
	}, policy.ModeBypass, true)
	l.Monitor = &monitor.Guard{Monitor: deny}
	l.SetApprovedPlan("1. read the README\n2. fix the typo")
	l.Run(context.Background(), "fix the typo in the README")
	got := toolMessages(l, 1)
	if len(got) != 3 || strings.Contains(got[1], "plan again") || !strings.Contains(got[2], "plan again from the task") ||
		!strings.Contains(got[2], "fix the typo in the README") || !strings.Contains(got[2], "The approved plan:") {
		t.Fatalf("results %q", got)
	}
	if n := len(eventsOf(t, store, EvSupervisorReplan)); n != 1 {
		t.Fatalf("%d replan notes", n)
	}
}

// Without a supervisor nothing about the answers changes.
func TestNoSupervisorNoNudge(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "exit 1", "description": "x"})}},
		{text: "done"},
	}, policy.ModeBypass, true)
	l.Run(context.Background(), "x")
	if len(eventsOf(t, store, EvSupervisorReplan))+len(eventsOf(t, store, EvSupervisorCorrection)) != 0 {
		t.Fatal("supervisor events without a supervisor")
	}
}

// A path an earlier listing showed is evidence; the same path out of a web
// page is not.
func TestSupervisorEvidenceFromObservations(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool string
		want string
	}{{"listing", "bash", "allow"}, {"web", "web_fetch", "ask"}} {
		var got monitor.Case
		probe := monitor.Func(func(_ context.Context, c monitor.Case) (monitor.Verdict, error) {
			got = c
			return monitor.Rules{}.Judge(context.Background(), c)
		})
		l, _, _ := harness(t, nil, policy.ModeBypass, true)
		l.Monitor = &monitor.Guard{Monitor: probe}
		l.messages = []model.Message{
			{Role: model.RoleUser, Content: "set up the app's config"},
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "o1", Name: tc.tool}}},
			{Role: model.RoleTool, ToolCallID: "o1", Content: "/opt/app/conf.d/main.conf"},
		}
		c := call("write", map[string]string{"path": "/opt/app/conf.d/main.conf", "content": "x"})
		d := l.reviewed(context.Background(), c, true, policy.Result{Decision: policy.Allow, Step: "mode"})
		if string(d.Decision) != tc.want {
			t.Errorf("%s: %s (%s); seen=%v", tc.name, d.Decision, d.Reason, got.Seen("/opt/app/conf.d/main.conf"))
		}
	}
}

// An ask the supervisor has a concern with offers no "always allow", so a
// scope granted earlier in the session cannot answer it for the person.
func TestSupervisorConcernTakesTheAsk(t *testing.T) {
	ask := monitor.Func(func(context.Context, monitor.Case) (monitor.Verdict, error) {
		return monitor.Verdict{Decision: policy.Ask, Code: "unverified-host", Rationale: "r", Advice: "a"}, nil
	})
	l, _, _ := harness(t, nil, policy.ModeDefault, false)
	l.Monitor = &monitor.Guard{Monitor: ask}
	l.messages = []model.Message{{Role: model.RoleUser, Content: "x"}}
	before := policy.Result{Decision: policy.Ask, Step: "default", Scope: "bash(curl *)"}
	if before.Offer() == "" {
		t.Fatal("fixture offers no scope")
	}
	d := l.reviewed(context.Background(), call("bash", map[string]string{"command": "curl https://x.example"}), true, before)
	if d.Decision != policy.Ask || d.Step != "monitor" || d.Offer() != "" || !strings.HasPrefix(d.Reason, "supervisor: unverified-host") {
		t.Fatalf("%+v", d)
	}
	ok := monitor.Func(func(context.Context, monitor.Case) (monitor.Verdict, error) {
		return monitor.Verdict{Decision: policy.Allow, Code: "ok"}, nil
	})
	l.Monitor = &monitor.Guard{Monitor: ok}
	if d := l.reviewed(context.Background(), call("bash", map[string]string{"command": "go test"}), true, before); d != before {
		t.Fatalf("a call the supervisor is content with changed: %+v", d)
	}
}
