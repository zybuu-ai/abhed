package app

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

const pricedMain = `,"pricing":{"m-1":{"input":0,"output":0}}`

// Under a dollar cap, an unpriced completion model is refused like any
// other: nothing is sent, and Studio is told once why completions are off.
func TestStudioCompletionsRefusedUnpricedUnderACap(t *testing.T) {
	fim := newFIMServer(t, "x")
	r := completionRig(t, fim.URL, `,"limits":{"max_budget_usd":1}`+pricedMain)
	noHold(t)
	id := r.open()
	want := "completions are off: the completion model has no price under this dollar cap; set pricing for it (0 for a local model)"
	for i := range 2 {
		var res map[string]any
		r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "x", "suffix": "", "seq": i + 1}, &res)
		_, noticed := res["notice"]
		if res["disabled"] != want || noticed != (i == 0) {
			t.Fatalf("request %d: %v", i, res)
		}
	}
	var edits map[string]any
	r.cl.ok("_abhed/nextEdit", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "language": "go",
		"cursor": map[string]int{"line": 0}, "window": map[string]any{"startLine": 0, "text": "a := 1\n"}}, &edits)
	if !strings.Contains(edits["disabled"].(string), "next-edit suggestions are off") || edits["notice"] == nil {
		t.Fatalf("next edit %v", edits)
	}
	if n := len(fim.requests()); n != 0 {
		t.Fatalf("%d requests went out", n)
	}
	r.cl.call("session/close", map[string]any{"sessionId": id})
	if enabled, _ := r.recorded(id, agent.EvCompletionEnabled); len(enabled) != 0 {
		t.Fatalf("completion.enabled recorded for a refused model: %v", enabled)
	}
}

// A priced completion model's calls are charged to the session's budget,
// and their cost is in completion.summary.
func TestStudioCompletionsCharged(t *testing.T) {
	fim := newFIMServer(t, "return a + b")
	r := completionRig(t, fim.URL, `,"limits":{"max_budget_usd":100},"pricing":{"m-1":{"input":0,"output":0},"qwen2.5-coder":{"input":1000000,"output":1000000}}`)
	noHold(t)
	id := r.open()
	var res completeItems
	r.cl.ok("_abhed/complete", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go", "prefix": "func add(a, b int) int {\n\t", "suffix": "\n}\n"}, &res)
	if len(res.Items) != 1 {
		t.Fatalf("%+v", res)
	}
	r.cl.call("session/close", map[string]any{"sessionId": id})
	sum, _ := r.recorded(id, agent.EvCompletionSummary)
	if len(sum) != 1 || sum[0]["cost_usd"] != 34.0 || sum[0]["unpriced"] != nil {
		t.Fatalf("completion.summary %v", sum)
	}
}

// A commit message's model is gated the same way.
func TestStudioCommitMessageRefusedUnpricedUnderACap(t *testing.T) {
	fim := newFIMServer(t, "Add a")
	r := completionRig(t, fim.URL, `,"limits":{"max_budget_usd":1}`+pricedMain)
	for _, args := range [][]string{{"init", "-q"}, {"add", "a.go"}} {
		if args[0] == "add" {
			r.write("a.go", "package a\n")
		}
		if out, err := exec.Command("git", append([]string{"-C", r.ws}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	id := r.open()
	var res map[string]any
	r.cl.ok("_abhed/commitMessage", map[string]any{"sessionId": id}, &res)
	if !strings.Contains(res["disabled"].(string), "commit messages are off: the commit model has no price") || res["notice"] == nil {
		t.Fatalf("%v", res)
	}
	fim.mu.Lock()
	n := len(fim.paths)
	fim.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d requests went out", n)
	}
}
