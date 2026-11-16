package app

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
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
	if len(sum) != 1 || sum[0]["cost_usd"] != sum[0]["tokens_in"].(float64)+sum[0]["tokens_out"].(float64) || sum[0]["unpriced"] != nil {
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

const pricedCoder = `,"pricing":{"m-1":{"input":0,"output":0},"qwen2.5-coder":{"input":1000000,"output":1000000}}`

// A served next edit is charged, though the chat server reports no usage:
// its tokens are estimated and its cost is in next_edit.summary.
func TestStudioNextEditChargedWithoutUsage(t *testing.T) {
	window := "func area(w, h int) int {\n\tsize := w * h\n\treturn size\n}\n"
	fim := newFIMServer(t, "func area(w, h int) int {\n\tarea := w * h\n\treturn area\n}\n")
	r := completionRig(t, fim.URL, `,"limits":{"max_budget_usd":1000000}`+pricedCoder)
	noHold(t)
	id := r.open()
	uri := "file://" + r.ws + "/area.go"
	var res nextEdits
	r.cl.ok("_abhed/nextEdit", map[string]any{"sessionId": id, "uri": uri, "language": "go",
		"cursor": map[string]int{"line": 11, "character": 2}, "window": map[string]any{"startLine": 10, "text": window}}, &res)
	if len(res.Edits) != 1 {
		t.Fatalf("%+v", res)
	}
	r.cl.call("session/close", map[string]any{"sessionId": id})
	sum, _ := r.recorded(id, agent.EvNextEditSummary)
	if len(sum) != 1 || sum[0]["served"] != float64(1) {
		t.Fatalf("summary %v", sum)
	}
	if usd, _ := sum[0]["cost_usd"].(float64); usd <= 0 || sum[0]["tokens_in"].(float64) <= 0 || sum[0]["estimated"] != float64(1) {
		t.Fatalf("a served next edit was not charged its estimated tokens: %v", sum[0])
	}
}

// A commit message's call is a model.call with its tokens and cost, and no content.
func TestStudioCommitMessageIsAModelCall(t *testing.T) {
	fim := newFIMServer(t, "Add a")
	r := completionRig(t, fim.URL, pricedCoder)
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
	if res["message"] != "Add a" {
		t.Fatalf("%v", res)
	}
	calls, _ := r.recorded(id, agent.EvModelCall)
	var mc map[string]any
	for _, c := range calls {
		if c["purpose"] == agent.PurposeCommitMessage {
			mc = c
		}
	}
	if mc == nil || mc["model"] != "qwen2.5-coder" || mc["tokens_in"].(float64) <= 0 || mc["cost_usd"] == nil {
		t.Fatalf("commit model.call %v in %v", mc, calls)
	}
	for k := range mc {
		if k == "text" || k == "content" || k == "message" {
			t.Fatalf("content in the model.call: %v", mc)
		}
	}
}

// An editor call that never reached the model is not charged; one cancelled
// under a cap is charged at its output bound, whatever input was reported.
func TestEditorChargeReachAndBound(t *testing.T) {
	l := agent.NewLoop(&fakeAdapter{name: "gpt-4o"}, nil, nil, nil, nil, nil, agent.DefaultConfig())
	l.Budget = agent.NewBudget(0, 0, false)
	l.Budget.MaxUSD = 100
	a := &fakeAdapter{name: "gpt-4o"}
	refused := &model.StatusError{Status: 500}
	if in, out, cost, _, est := editorCharge(l, a, 0, 0, refused, false, 4000, 0, 64); in != 0 || out != 0 || cost != nil || est {
		t.Fatalf("an unreached call was charged: %d %d %v", in, out, cost)
	}
	in, out, cost, _, est := editorCharge(l, a, 500, 0, context.Canceled, true, 4000, 8, 64)
	if in != 500 || out != 64 || cost == nil || !est {
		t.Fatalf("a cancelled call: in %d out %d cost %v estimated %v", in, out, cost, est)
	}
}
