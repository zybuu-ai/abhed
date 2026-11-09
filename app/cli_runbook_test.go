package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// scriptServer answers each model call with reply(n), n counting from 1.
func scriptServer(t *testing.T, reply func(w io.Writer, n int)) string {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		reply(w, int(n.Add(1)))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// -p -runbook: the operator's file approves the steps; the step made as
// listed runs, a change outside it asks and so is refused, and a pinned
// hash that differs refuses the run before anything is recorded.
func TestHeadlessRunbook(t *testing.T) {
	var ws string
	url := scriptServer(t, func(w io.Writer, n int) {
		switch n {
		case 1:
			sseCall(w, n, "write", `{"path":`+strconv.Quote(filepath.Join(ws, "notes.txt"))+`,"content":"x"}`)
		case 2:
			sseCall(w, n, "write", `{"path":`+strconv.Quote(filepath.Join(ws, "other.txt"))+`,"content":"x"}`)
		default:
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
	})
	ws = bgWorkspace(t, url, "")
	if real, err := filepath.EvalSymlinks(ws); err == nil {
		ws = real
	}
	file := filepath.Join(t.TempDir(), "rb.json")
	doc, _ := json.Marshal(map[string]any{"plan": "notes", "runbook": []map[string]any{
		{"tool": "write", "target": filepath.Join(ws, "notes.txt"), "args": map[string]string{"content": "x"}},
	}})
	if err := os.WriteFile(file, doc, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := mainHelper([]string{"-C", ws, "-runbook", file, "-runbook-sha256", strings.Repeat("a", 64), "-p", "go"})
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err == nil || !strings.Contains(out.String(), "not the "+strings.Repeat("a", 64)) {
		t.Fatalf("a runbook with another hash ran: %v\n%s", err, out.String())
	}

	cmd = mainHelper([]string{"-C", ws, "-runbook", file, "-output-format", "json", "-p", "go"})
	out.Reset()
	var errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	_ = cmd.Run()
	if !strings.Contains(errOut.String(), "runbook bound: 1 steps") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
	if b, err := os.ReadFile(filepath.Join(ws, "notes.txt")); err != nil || string(b) != "x" {
		t.Fatalf("the step did not run: %v\n%s\n%s", err, out.String(), errOut.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "other.txt")); err == nil {
		t.Fatal("a change outside the runbook ran headless")
	}
	for _, want := range []string{`"plan.decided"`, `"via":"operator"`, `"plan.step"`, "not in the approved plan"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("no %s in the events:\n%s", want, out.String())
		}
	}
}
