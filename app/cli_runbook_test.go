package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Through the CLI: a plan with a runbook lists its steps, a step with no
// undo is confirmed a second time, the step made as listed runs without an
// ask, and a change that is not in the runbook asks, saying so.
func TestCLIRunbookPlan(t *testing.T) {
	var ws atomic.Value
	path := func(name string) string { return strconv.Quote(filepath.Join(ws.Load().(string), name)) }
	c := startCLIWith(t, func(w io.Writer, n int, _ string) {
		switch n {
		case 1:
			rb, _ := json.Marshal([]map[string]any{
				{"tool": "write", "target": filepath.Join(ws.Load().(string), "notes.txt"), "args": map[string]string{"content": "x"}, "note": "the notes"},
				{"tool": "bash", "command": "rm -rf build"},
			})
			sseCall(w, n, "exit_plan", `{"plan":"1. create notes.txt\n2. clear build","runbook":`+string(rb)+`}`)
		case 2:
			sseCall(w, n, "write", `{"path":`+path("notes.txt")+`,"content":"x"}`)
		case 3:
			sseCall(w, n, "write", `{"path":`+path("other.txt")+`,"content":"x"}`)
		default:
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
	})
	ws.Store(c.ws)
	c.command("/mode plan", "mode: plan")
	fmt.Fprintln(c.stdin, "plan a notes file")
	c.waitFor(func(out string) bool {
		return strings.Contains(out, "Proceed with this plan?") && strings.Contains(out, "answer 1-3")
	}, "the plan card")
	out := c.out.String()
	for _, want := range []string{"Runbook", "2 steps", "notes.txt", "rm -rf build", "NO UNDO"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the card does not show %q:\n%s", want, out)
		}
	}
	c.command("2", "cannot be undone")
	c.command("2", "mode: plan → default")
	c.waitFor(func(out string) bool {
		i := strings.LastIndex(out, "needs approval")
		return i >= 0 && strings.Contains(out[i:], "answer 1-")
	}, "the change outside the plan to be asked about")
	if !strings.Contains(c.out.String(), "not in the approved plan") {
		t.Fatalf("the ask does not say why:\n%s", c.out.String())
	}
	c.command(c.declineNumber(), " in / ")
	if b, err := os.ReadFile(filepath.Join(c.ws, "notes.txt")); err != nil || string(b) != "x" {
		t.Fatalf("the runbook step did not run: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(c.ws, "other.txt")); err == nil {
		t.Fatal("a change outside the plan was made unasked")
	}
	evs := c.export()
	decided := eventsOf[agent.PlanDecided](t, evs, agent.EvPlanDecided)
	if len(decided) != 1 || decided[0].Runbook == nil || len(decided[0].PlanHash) != 64 {
		t.Fatalf("decided %+v", decided)
	}
	steps := eventsOf[agent.PlanStep](t, evs, agent.EvPlanStep)
	if len(steps) != 1 || steps[0].Step != 1 || steps[0].PlanHash != decided[0].PlanHash || steps[0].By != agent.ByPlan {
		t.Fatalf("plan.step %+v", steps)
	}
	var reqs []string
	for _, e := range evs {
		if e.Type == agent.EvUserMessage && strings.Contains(string(e.Payload), "runbook") {
			reqs = append(reqs, string(e.Payload))
		}
	}
	if len(reqs) != 1 || !slices.ContainsFunc(reqs, func(s string) bool { return strings.Contains(s, decided[0].PlanHash[:12]) }) {
		t.Fatalf("the model was not given the steps: %v", reqs)
	}
}

// Declining the second question about steps with no undo keeps planning.
func TestRunbookDestructiveDeclinedKeepsPlanning(t *testing.T) {
	var ws atomic.Value
	c := startCLIWith(t, func(w io.Writer, n int, _ string) {
		if n == 1 {
			sseCall(w, n, "exit_plan", `{"plan":"clear build","runbook":[{"tool":"bash","command":"rm -rf build"}]}`)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	ws.Store(c.ws)
	c.command("/mode plan", "mode: plan")
	fmt.Fprintln(c.stdin, "plan it")
	c.waitFor(func(out string) bool { return strings.Contains(out, "answer 1-3") }, "the plan card")
	c.command("2", "cannot be undone")
	c.command("1", "still in plan mode")
	if got := planDecisions(t, c.export()); !slices.Equal(got, []string{"keep-planning>"}) {
		t.Fatalf("decisions %v", got)
	}
}
