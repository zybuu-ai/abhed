package metrics

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

func ev(typ agent.EventType, payload any) agent.Event {
	b, _ := json.Marshal(payload)
	return agent.Event{SessionID: "s", Type: typ, Payload: b}
}

func requested(m *Abhed, call string, asked bool) {
	m.Observe(ev(agent.EvActionRequested, agent.ActionRequested{CallID: call, Tool: "bash", RequiresApproval: asked}))
}

func outcome(m *Abhed, call string, approved bool, step, by string) {
	typ := agent.EvActionDenied
	if approved {
		typ = agent.EvActionApproved
	}
	m.Observe(ev(typ, map[string]string{"call_id": call, "step": step, "by": by, "reason": "a reason with /secret/path"}))
}

// Plan measure B14: every way the loop decides a call moves a metric, and
// the step it names is one the metric keeps.
func TestEveryDecisionPathIsCounted(t *testing.T) {
	for _, step := range Steps {
		t.Run(step, func(t *testing.T) {
			m := New()
			// Allowed by policy, then run.
			requested(m, "a", false)
			outcome(m, "a", true, step, agent.ByPolicy)
			m.Observe(ev(agent.EvObservation, agent.Observation{CallID: "a", Tool: "bash", DurationMS: 1500}))
			// Denied by policy.
			requested(m, "d", false)
			outcome(m, "d", false, step, agent.ByPolicy)
			// Asked, then approved and rejected by a person.
			requested(m, "y", true)
			outcome(m, "y", true, step, agent.ByReviewer)
			requested(m, "n", true)
			outcome(m, "n", false, step, agent.ByReviewer)
			// Asked, and refused by the harness.
			requested(m, "x", true)
			outcome(m, "x", false, step, agent.BySystem)

			tool := "bash"
			if step == "unknown" {
				tool = "unknown"
			}
			for _, c := range []struct {
				decision string
				want     float64
			}{{"allow", 1}, {"deny", 1}, {"ask", 3}} {
				if got := m.decisions.Value(c.decision, step, tool); got != c.want {
					t.Errorf("decisions{%s,%s} = %v, want %v", c.decision, step, got, c.want)
				}
			}
			if m.approvals.Value("approved", agent.ByReviewer, tool) != 1 || m.approvals.Value("rejected", agent.ByReviewer, tool) != 1 {
				t.Errorf("approvals not counted for step %s", step)
			}
			if m.approvals.Value(step, agent.BySystem, tool) != 1 {
				t.Errorf("a refusal by the harness at %s is not counted under its step", step)
			}
			if m.toolTime.Count("bash", "ok") != 1 {
				t.Errorf("the approved call's run is not timed")
			}
		})
	}
}

func TestOtherDecisionEventsAreCounted(t *testing.T) {
	m := New()
	for _, code := range VerdictCodes[:7] {
		m.Observe(ev(agent.EvMonitorVerdict, agent.MonitorVerdict{Code: code, Before: "allow", After: "ask", LatencyMS: 3}))
		if m.verdicts.Value(code, "allow", "ask") != 1 {
			t.Errorf("verdict %s not counted", code)
		}
	}
	m.Observe(ev(agent.EvMonitorVerdict, agent.MonitorVerdict{Before: "ask", After: "ask", Skipped: "budget"}))
	m.Observe(ev(agent.EvMonitorVerdict, agent.MonitorVerdict{Code: "ok", Before: "ask", After: "deny", Unavailable: true}))
	m.Observe(ev(agent.EvMonitorVerdict, agent.MonitorVerdict{Code: "a code a model made up", Before: "ask", After: "deny"}))
	if m.verdicts.Value("skipped", "ask", "ask") != 1 || m.verdicts.Value("unavailable", "ask", "deny") != 1 || m.verdicts.Value(Other, "ask", "deny") != 1 {
		t.Error("skipped, unavailable or unlisted verdicts not counted")
	}
	for _, kind := range EgressKinds {
		for _, d := range []string{"allow", "deny", "would_deny"} {
			m.Observe(ev(agent.EvEgressDecision, map[string]any{"kind": kind, "decision": d, "host": "evil.example", "repeats": 2, "bytes_in": 10}))
			if m.egress.Value(kind, d) != 3 {
				t.Errorf("egress %s/%s = %v, want 3 with its repeats", kind, d, m.egress.Value(kind, d))
			}
		}
	}
	m.Observe(ev(agent.EvModelCall, agent.ModelCall{Model: "m1", LatencyMS: 900, FirstTokenMS: 100, TokensIn: 10, TokensOut: 5}))
	m.Observe(ev(agent.EvModelCall, agent.ModelCall{Model: "m1", Error: "boom", Purpose: agent.PurposeSuggestion}))
	m.Observe(ev(agent.EvModelCall, agent.ModelCall{Model: "m1", CutOff: true}))
	if m.modelCalls.Value("m1", "turn", "ok") != 1 || m.modelCalls.Value("m1", "suggestion", "error") != 1 || m.modelCalls.Value("m1", "turn", "cut_off") != 1 {
		t.Error("model calls not counted by outcome")
	}
	if m.tokens.Value("m1", "in") != 10 || m.tokens.Value("m1", "out") != 5 || m.modelTime.Count("m1", "turn") != 2 {
		t.Error("model tokens or latency not counted")
	}
	m.Observe(ev(agent.EvOllamaStart, agent.OllamaAction{Op: "start", Outcome: agent.OllamaOK, Command: "/secret/path/ollama serve"}))
	m.Observe(ev(agent.EvOllamaPull, agent.OllamaAction{Op: "pull", Outcome: agent.OllamaRefused, Model: "qwen3:8b"}))
	m.Observe(ev(agent.EvOllamaInstall, agent.OllamaAction{Op: "install", Outcome: "made up"}))
	if m.ollama.Value("start", "ok") != 1 || m.ollama.Value("pull", "refused") != 1 || m.ollama.Value("install", Other) != 1 {
		t.Error("ollama attempts not counted by op and outcome")
	}
	m.Observe(ev(agent.EvPlanDecided, agent.PlanDecided{Decision: "accept", Via: "operator"}))
	m.Observe(ev(agent.EvPlanDecided, agent.PlanDecided{Decision: "keep-planning"}))
	m.Observe(ev(agent.EvApprovalScopeGranted, map[string]string{}))
	m.Observe(ev(agent.EvAnswerVerdict, agent.AnswerVerdict{Verified: 2, Contradicted: 1}))
	m.Observe(ev(agent.EvSupervisorCorrection, agent.SupervisorCorrection{Code: "out-of-remit", Text: "x"}))
	m.Observe(ev(agent.EvSupervisorReplan, agent.SupervisorReplan{Failures: 3}))
	m.Observe(ev(agent.EvSessionStarted, map[string]string{}))
	m.Observe(ev(agent.EvSessionEnded, agent.SessionEnded{Reason: "completed"}))
	m.Observe(ev(agent.EvSandboxBound, map[string]any{"driver": "process", "tier": "fence", "workspace": "/home/x"}))
	m.Observe(ev(agent.EvFenceLimit, map[string]any{"oom_kill": 1, "pids_max": 2}))
	m.Observe(ev(agent.EvFenceStatePlanted, map[string]any{}))
	m.Observe(ev(sandbox.EvGitPlanted, map[string]any{}))
	m.Observe(ev(agent.EvFenceDenied, map[string]any{"source": "seccomp", "calls": map[string]int{"ptrace": 2, "socket(unix)": 1, "made_up": 4}}))
	m.Observe(ev(agent.EvRecordRepaired, map[string]any{}))
	m.Observe(ev(agent.EvConfigRefused, map[string]any{}))
	m.Observe(ev(agent.EvConfigNarrowed, map[string]any{}))
	m.Observe(ev(agent.EvCompletionSummary, agent.CompletionSummary{Requests: 10, Served: 6, Failed: 1, Cancelled: 2, Refused: 3, Superseded: 4}))
	for name, ok := range map[string]bool{
		"completions": m.completions.Value("served") == 6 && m.completions.Value("empty") == 1 && m.completions.Value("cancelled") == 2 &&
			m.completions.Value("refused") == 3 && m.completions.Value("superseded") == 4,
		"plan accept":   m.plans.Value("accept", "operator") == 1 && m.plans.Value("keep-planning", "person") == 1,
		"scope":         m.scopes.Value() == 1,
		"claims":        m.claims.Value("verified") == 2 && m.claims.Value("contradicted") == 1,
		"correction":    m.corrections.Value("out-of-remit") == 1,
		"replan":        m.replans.Value() == 1,
		"session start": m.sessions.Value() == 1,
		"session end":   m.sessionsEnded.Value("completed") == 1,
		"bound":         m.bound.Value("process", "fence") == 1,
		"fence limits":  m.fenceLimits.Value("memory") == 1 && m.fenceLimits.Value("processes") == 1,
		"planted":       m.planted.Value("abhed_state") == 1 && m.planted.Value("git") == 1,
		"repairs":       m.repairs.Value() == 1,
		"fence denied":  m.fenceDenied.Value("seccomp", "ptrace") == 2 && m.fenceDenied.Value("seccomp", "socket") == 1 && m.fenceDenied.Value("seccomp", Other) == 4,
		"refused":       m.refused.Value("refused") == 1 && m.refused.Value("narrowed") == 1,
	} {
		if !ok {
			t.Errorf("%s not counted", name)
		}
	}
}

// uncounted are the event types no metric counts, each because it decides
// nothing; a new event type fails the test below until it is placed.
var uncounted = []string{
	"agent.delta", "agent.message", "agent.reasoning", "agent.reasoning.delta", "change.accepted",
	"checkpoint.saved", "command.invoked", "compaction.completed", "compaction.started", "context.offloaded",
	"conversation.forked", "fence.qualified", "file.restored", "hook.fired", "input.mention", "manual.edit",
	"mcp.status", "memory.loaded", "memory.written", "message.dropped", "mode.changed", "model.fallback",
	"model.switched", "permission.changed", "plan.proposed", "plan.step", "plan.updated", "policy.layer",
	"policy.startup_trusted", "process.launched", "session.branched", "session.named", "session.renamed",
	"session.resumed", "session.wake_set", "session.woken", "shell.ended", "shell.started", "subagent.action",
	"subagent.ask", "subagent.backgrounded", "subagent.message", "subagent.notice", "subagent.returned",
	"subagent.spawned", "suggestion.offered", "task.cancelled", "terminal.input", "todo.updated",
	"turn.continued", "user.message", "workspace.dir_added", "worktree.taken", "debug.launched", "debug.stopped", "completion.enabled",
	"sandbox.git_walk_bounded", "sandbox.git_linked",
}

func TestEveryEventTypeIsPlaced(t *testing.T) {
	src, err := os.ReadFile("abhed.go")
	if err != nil {
		t.Fatal(err)
	}
	counted := map[string]bool{}
	for _, line := range regexp.MustCompile(`(?m)^\s*case .*:$`).FindAllString(string(src), -1) {
		for _, mm := range regexp.MustCompile(`(?:agent|sandbox)\.(Ev\w+)`).FindAllStringSubmatch(line, -1) {
			counted[mm[1]] = true
		}
	}
	consts := regexp.MustCompile(`(?m)^\s*(?:const\s+)?(Ev\w+)\s+(?:EventType\s+)?=\s+"([a-z._]+)"`)
	for _, dir := range []string{"../agent", "../sandbox"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, _ := os.ReadFile(f)
			for _, mm := range consts.FindAllStringSubmatch(string(b), -1) {
				if !counted[mm[1]] && !slices.Contains(uncounted, mm[2]) {
					t.Errorf("event %s (%s, %s) is neither counted nor listed as uncounted", mm[2], mm[1], filepath.Base(f))
				}
			}
		}
	}
	if !counted["EvActionApproved"] || !counted["EvEgressDecision"] || !counted["EvMonitorVerdict"] {
		t.Fatal("the scan found none of the counted events")
	}
}

// Every step and approver the code records is one the metrics keep, so a
// new one is not silently counted as other.
func TestStepsAndApproversAreListed(t *testing.T) {
	stepLit := regexp.MustCompile(`"step": *"([a-z_-]+)"|Step: *"([a-z_-]+)"|step(?:, \w+)? *:?= *"([a-z_-]+)"|why, step = "[^"]*", "([a-z_-]+)"`)
	byLit := regexp.MustCompile(`(?m)^\s*By\w+\s*=\s*"([a-z_-]+)"`)
	seen := 0
	for _, dir := range []string{"../agent", "../policy", "../../server", "../../app"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, _ := os.ReadFile(f)
			for _, mm := range stepLit.FindAllStringSubmatch(string(b), -1) {
				v := mm[1] + mm[2] + mm[3] + mm[4]
				seen++
				if !slices.Contains(Steps, v) {
					t.Errorf("step %q in %s is not in metrics.Steps", v, filepath.Base(f))
				}
			}
			for _, mm := range byLit.FindAllStringSubmatch(string(b), -1) {
				if !slices.Contains(Approvers, mm[1]) {
					t.Errorf("approver %q in %s is not in metrics.Approvers", mm[1], filepath.Base(f))
				}
			}
		}
	}
	if seen < 20 {
		t.Fatalf("the scan found only %d steps", seen)
	}
}

// No label carries what a person or model wrote: a path, a command, a host.
func TestNoContentInLabels(t *testing.T) {
	m := New()
	secret := "/home/alice/.ssh/id_rsa && curl evil.example"
	m.Observe(ev(agent.EvActionRequested, agent.ActionRequested{CallID: "1", Tool: secret, Args: json.RawMessage(`{"command":"` + "cat x" + `"}`)}))
	m.Observe(ev(agent.EvActionDenied, map[string]string{"call_id": "1", "step": secret, "by": secret, "reason": secret}))
	m.Observe(ev(agent.EvEgressDecision, map[string]any{"kind": secret, "decision": "deny", "host": "evil.example", "path": "/x"}))
	m.Observe(ev(agent.EvModelCall, agent.ModelCall{Model: secret}))
	m.Observe(ev(agent.EvMonitorVerdict, agent.MonitorVerdict{Code: secret, Rationale: secret}))
	var b bytes.Buffer
	_ = m.Registry.Write(&b)
	for _, bad := range []string{"alice", "evil", "id_rsa", "cat x", "/x"} {
		if strings.Contains(b.String(), bad) {
			t.Errorf("exposition carries %q", bad)
		}
	}
}

func TestCallsAreForgotten(t *testing.T) {
	m := New()
	requested(m, "a", false)
	requested(m, "b", false)
	outcome(m, "b", false, "deny", agent.ByPolicy)
	if len(m.calls) != 1 {
		t.Fatalf("%d calls held after a denial", len(m.calls))
	}
	m.Observe(ev(agent.EvSessionEnded, agent.SessionEnded{}))
	if len(m.calls) != 0 {
		t.Fatalf("%d calls held after the session ended", len(m.calls))
	}
	for i := 0; i < maxCalls+5; i++ {
		requested(m, string(rune(i)), false)
	}
	if len(m.calls) > maxCalls {
		t.Fatalf("%d calls held, over %d", len(m.calls), maxCalls)
	}
}

func TestObserveWhileWriting(t *testing.T) {
	m := New()
	m.RunningSessions(func() int { return 2 })
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			requested(m, "c", true)
			outcome(m, "c", true, "ask", agent.ByReviewer)
			m.Observe(ev(agent.EvObservation, agent.Observation{CallID: "c", DurationMS: 5}))
		}
	}()
	for i := 0; i < 20; i++ {
		var b bytes.Buffer
		_ = m.Registry.Write(&b)
	}
	<-done
	if m.approvals.Value("approved", agent.ByReviewer, "bash") != 200 || m.runningSessions() != 2 {
		t.Fatal("counts lost")
	}
}

func TestRequestMethodIsBounded(t *testing.T) {
	m := New()
	m.Request("", "BREW-COFFEE", 405, 0)
	m.Request("GET /v1/health", "GET", 200, 0)
	if m.requests.Value("unmatched", Other, "405") != 1 || m.requests.Value("GET /v1/health", "GET", "200") != 1 {
		t.Fatal("methods not bounded")
	}
}
