package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// planApprover answers every ask with yes, and keeps what it was asked.
type planApprover struct {
	mu      sync.Mutex
	yes     bool
	reasons []string
}

func (a *planApprover) Approve(_ context.Context, _ string, _ json.RawMessage, d policy.Result) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reasons = append(a.reasons, d.Reason)
	return a.yes, nil
}

// fakeSSH stands in for the ssh tool: a mutating call with a host and a command.
type fakeSSH struct{ ran *[]string }

func (fakeSSH) Name() string        { return "ssh" }
func (fakeSSH) Description() string { return "ssh" }
func (fakeSSH) Mutates() bool       { return true }
func (fakeSSH) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"host":{"type":"string"},"command":{"type":"string"},"timeout_seconds":{"type":"integer"}},"required":["host","command"]}`)
}
func (f fakeSSH) Run(_ context.Context, _ *tools.Session, raw json.RawMessage) tools.Result {
	var a struct{ Host, Command string }
	_ = json.Unmarshal(raw, &a)
	*f.ran = append(*f.ran, a.Host+": "+a.Command)
	return tools.Result{Content: "exit 0"}
}

type runbookRig struct {
	l     *Loop
	store *MemStore
	dir   string
	appr  *planApprover
	ran   *[]string
}

// runbookHarness is a loop in plan mode with the ssh stand-in, whose plan
// has been proposed with steps and accepted into mode.
func runbookHarness(t *testing.T, mode policy.Mode, steps []tools.RunbookStep, turns []scriptedTurn) *runbookRig {
	t.Helper()
	dir := tempDir(t)
	l, store := harnessIn(t, dir, turns, policy.ModePlan, true)
	ran := &[]string{}
	l.Tools.Add(fakeSSH{ran: ran})
	appr := &planApprover{yes: true}
	l.Approver = appr
	l.EnablePlanExit()
	if err := l.proposePlan(context.Background(), "rotate the certificate", steps); err != nil {
		t.Fatal(err)
	}
	p, ok := l.TakePlan()
	if !ok || p.Runbook == nil || len(p.Runbook.Steps) != len(steps) {
		t.Fatalf("proposed %+v %v", p, ok)
	}
	if err := l.BindPlan(p.Runbook.Hash); err != nil {
		t.Fatal(err)
	}
	l.Policy.Mode = mode
	return &runbookRig{l: l, store: store, dir: dir, appr: appr, ran: ran}
}

func sshStep(host, cmd string) tools.RunbookStep {
	return tools.RunbookStep{Tool: "ssh", Target: host, Command: cmd}
}

func sshCall(id, host, cmd string) model.ToolCall {
	c := call("ssh", map[string]string{"host": host, "command": cmd})
	c.ID = id
	return c
}

func approvals(t *testing.T, evs []Event) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	for _, e := range evs {
		if e.Type == EvActionApproved || e.Type == EvActionDenied {
			var m map[string]string
			_ = json.Unmarshal(e.Payload, &m)
			m["event"] = string(e.Type)
			out[m["call_id"]] = m
		}
	}
	return out
}

// The next step, made exactly as approved, runs without an ask and is
// recorded as plan.step with the plan's hash; the same call again asks.
func TestRunbookStepRunsUnasked(t *testing.T) {
	r := runbookHarness(t, policy.ModeDefault, []tools.RunbookStep{
		sshStep("web1", "openssl x509 -enddate -noout -in /etc/nginx/tls/site.crt"),
		sshStep("web1", "systemctl reload nginx"),
	}, []scriptedTurn{
		{calls: []model.ToolCall{sshCall("a", "web1", "openssl x509 -enddate -noout -in /etc/nginx/tls/site.crt")}},
		{calls: []model.ToolCall{sshCall("b", "web1", "systemctl reload nginx")}},
		{calls: []model.ToolCall{sshCall("c", "web1", "systemctl reload nginx")}},
		{text: "done"},
	})
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(r.appr.reasons) != 1 || !strings.Contains(r.appr.reasons[0], "not in the approved plan") {
		t.Fatalf("asked %q; only the repeat should ask", r.appr.reasons)
	}
	rb, done, ok := r.l.BoundRunbook()
	if !ok || !done[0] || !done[1] {
		t.Fatalf("bound %v done %v", ok, done)
	}
	evs, _ := r.store.Events("sess1")
	got := approvals(t, evs)
	for i, id := range []string{"a", "b"} {
		a := got[id]
		if a["event"] != string(EvActionApproved) || a["by"] != ByPlan || a["step"] != "plan" ||
			a["plan_hash"] != rb.Hash || a["plan_step"] != []string{"1", "2"}[i] {
			t.Fatalf("%s recorded %v", id, a)
		}
	}
	if got["c"]["by"] == ByPlan || got["c"]["plan_hash"] != "" {
		t.Fatalf("the repeat was recorded as a plan step: %v", got["c"])
	}
	var steps []PlanStep
	for _, e := range evs {
		if e.Type == EvPlanStep {
			var p PlanStep
			_ = json.Unmarshal(e.Payload, &p)
			steps = append(steps, p)
		}
	}
	if len(steps) != 2 || steps[1].Step != 2 || steps[1].PlanHash != rb.Hash || steps[1].Target != "web1" ||
		steps[1].StepHash != rb.Steps[1].Hash || steps[1].By != ByPlan {
		t.Fatalf("plan.step %+v", steps)
	}
	if len(*r.ran) != 3 {
		t.Fatalf("ran %v", *r.ran)
	}
}

// A changed argument, another host, or a step out of order is not the
// approved step: it asks, saying so, and a refusal leaves it unrun.
func TestRunbookChangedCallAsks(t *testing.T) {
	r := runbookHarness(t, policy.ModeDefault, []tools.RunbookStep{
		sshStep("web1", "nginx -t"),
		sshStep("web1", "systemctl reload nginx"),
	}, []scriptedTurn{
		{calls: []model.ToolCall{sshCall("a", "web1", "nginx -t; rm -rf /srv")}},
		{calls: []model.ToolCall{sshCall("b", "web2", "nginx -t")}},
		{calls: []model.ToolCall{sshCall("c", "web1", "systemctl reload nginx")}},
		{text: "done"},
	})
	r.appr.yes = false
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(r.appr.reasons) != 3 {
		t.Fatalf("asked %q", r.appr.reasons)
	}
	for _, why := range r.appr.reasons {
		if !strings.Contains(why, "not in the approved plan") {
			t.Fatalf("asked %q", why)
		}
	}
	if !strings.Contains(r.appr.reasons[2], "out of order") {
		t.Fatalf("out of order asked %q", r.appr.reasons[2])
	}
	if len(*r.ran) != 0 {
		t.Fatalf("ran %v", *r.ran)
	}
	if _, done, _ := r.l.BoundRunbook(); done[0] || done[1] {
		t.Fatalf("a refused call took a step: %v", done)
	}
}

// A call the policy alone would allow still asks while a runbook is bound
// and it is not a step; a read-only call does not.
func TestRunbookTightensAllowedChanges(t *testing.T) {
	r := runbookHarness(t, policy.ModeAcceptEdits, []tools.RunbookStep{sshStep("web1", "uptime")}, nil)
	dir := r.dir
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.l.Adapter = &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{call("write", map[string]string{"path": filepath.Join(dir, "new.txt"), "content": "y"})}},
		{calls: []model.ToolCall{call("read", map[string]string{"path": filepath.Join(dir, "f.txt")})}},
		{text: "done"},
	}}
	r.appr.yes = false
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(r.appr.reasons) != 1 || !strings.Contains(r.appr.reasons[0], "the policy alone would allow it") {
		t.Fatalf("asked %q", r.appr.reasons)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err == nil {
		t.Fatal("a write outside the plan ran unasked")
	}
}

// A deny rule still refuses an approved step, and refuses a runbook that
// names one when it is proposed.
func TestRunbookDenyStillWins(t *testing.T) {
	r := runbookHarness(t, policy.ModeDefault, []tools.RunbookStep{sshStep("web1", "systemctl reload nginx")}, []scriptedTurn{
		{calls: []model.ToolCall{sshCall("a", "web1", "systemctl reload nginx")}},
		{text: "done"},
	})
	if err := r.l.Policy.AddDeny("ssh(web1/systemctl *)"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs, _ := r.store.Events("sess1")
	if a := approvals(t, evs)["a"]; a["event"] != string(EvActionDenied) || a["step"] != "deny" {
		t.Fatalf("recorded %v", a)
	}
	if len(*r.ran) != 0 || len(r.appr.reasons) != 0 {
		t.Fatalf("ran %v asked %v", *r.ran, r.appr.reasons)
	}

	l, _ := harnessIn(t, tempDir(t), nil, policy.ModePlan, true)
	l.Tools.Add(fakeSSH{ran: &[]string{}})
	_ = l.Policy.AddDeny("ssh(*/rm *)")
	err := l.proposePlan(context.Background(), "p", []tools.RunbookStep{sshStep("web1", "uptime"), sshStep("web1", "rm -rf /srv")})
	if err == nil || !strings.Contains(err.Error(), "step 2") {
		t.Fatalf("a denied step was proposed: %v", err)
	}
}

// Steps are canonical and hashed: the same runbook hashes the same, a
// changed argument or plan text does not; a step with no undo is named.
func TestRunbookHashAndDestructive(t *testing.T) {
	l, _ := harnessIn(t, tempDir(t), nil, policy.ModePlan, true)
	l.Tools.Add(fakeSSH{ran: &[]string{}})
	steps := []tools.RunbookStep{
		sshStep("web1", "rm -rf /var/cache/nginx"),
		{Tool: "bash", Command: "go test ./..."},
		{Tool: "ssh", Args: json.RawMessage(`{"command":"uptime","host":"web2"}`)},
	}
	a, err := l.prepareRunbook("p", steps)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := l.prepareRunbook("p", steps)
	if a.Hash != b.Hash || len(a.Hash) != 64 {
		t.Fatalf("hash %s vs %s", a.Hash, b.Hash)
	}
	if c, _ := l.prepareRunbook("p2", steps); c.Hash == a.Hash {
		t.Fatal("the plan text is not in the hash")
	}
	changed := append([]tools.RunbookStep(nil), steps...)
	changed[1].Command = "go test ./... -run X"
	if c, _ := l.prepareRunbook("p", changed); c.Hash == a.Hash || c.Steps[0].Hash != a.Steps[0].Hash {
		t.Fatal("a changed step did not change the hash")
	}
	if a.Steps[0].Destructive == "" || a.Steps[1].Destructive != "" || a.Steps[2].Target != "web2" {
		t.Fatalf("steps %+v", a.Steps)
	}
	for _, bad := range []tools.RunbookStep{
		{Tool: "read", Args: json.RawMessage(`{"path":"x"}`)},
		{Tool: "ssh", Command: "uptime"},
		{Tool: "ssh", Target: "web1", Command: "uptime", Args: json.RawMessage(`{"host":"web2"}`)},
		{Tool: "bash", Target: "web1", Command: "uptime"},
	} {
		if _, err := l.prepareRunbook("p", []tools.RunbookStep{bad}); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

// A plan accepted with another hash binds nothing; a rebuilt or rewound
// conversation lets the runbook go, so each step asks again.
func TestRunbookBindingEnds(t *testing.T) {
	r := runbookHarness(t, policy.ModeDefault, []tools.RunbookStep{sshStep("web1", "uptime")}, nil)
	if _, _, ok := r.l.BoundRunbook(); !ok {
		t.Fatal("not bound")
	}
	r.l.SetHistory(nil, 0)
	if _, _, ok := r.l.BoundRunbook(); ok {
		t.Fatal("a rebuilt conversation kept the runbook")
	}

	l, _ := harnessIn(t, tempDir(t), nil, policy.ModePlan, true)
	l.Tools.Add(fakeSSH{ran: &[]string{}})
	l.EnablePlanExit()
	if err := l.proposePlan(context.Background(), "p", []tools.RunbookStep{sshStep("web1", "uptime")}); err != nil {
		t.Fatal(err)
	}
	if err := l.BindPlan(strings.Repeat("0", 64)); err == nil {
		t.Fatal("bound a plan that was not proposed")
	}
	if _, _, ok := l.BoundRunbook(); ok {
		t.Fatal("bound")
	}
}

// A subagent's change under a bound runbook asks: it is never a step.
func TestRunbookSubagentCallAsks(t *testing.T) {
	r := runbookHarness(t, policy.ModeAcceptEdits, []tools.RunbookStep{sshStep("web1", "uptime")}, nil)
	ctx := r.l.asParent(context.Background())
	sub, _ := harnessIn(t, r.dir, nil, policy.ModeAcceptEdits, true)
	tool, _ := sub.Tools.Get("write")
	c := call("write", map[string]string{"path": filepath.Join(r.dir, "x"), "content": "y"})
	d := sub.Policy.Evaluate("write", true, c.Args)
	if d.Decision != policy.Allow {
		t.Fatalf("precondition: %+v", d)
	}
	got, hit := sub.planBind(ctx, tool, c, d)
	if got.Decision != policy.Ask || hit != nil || !strings.Contains(got.Reason, "subagent") {
		t.Fatalf("got %+v", got)
	}
}
