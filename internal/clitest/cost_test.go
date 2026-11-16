//go:build unix

package clitest

import (
	"strings"
	"testing"
)

// costConfig is the stub under a model name with a configured price.
const costConfig = `{"sandbox":{"min_tier":"none"},"pricing":{"stub-model":{"input":2,"output":10}},` +
	`"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"{{MODEL_URL}}","model":"stub-model","context_window":32768}}}}`

// -p -output-format json reports the run's cost from the configured price.
func TestHeadlessReportsCost(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{Args: []string{"-p", "hi", "-output-format", "json"}, UserConfig: costConfig,
		Script: "text \"hello\"\nusage in=6000 out=100"})
	usage, _ := resultOf(t, h.Stdout())["usage"].(map[string]any)
	if got, want := usage["cost_usd"], (6000*2.0+100*10.0)/1e6; got != want {
		t.Fatalf("cost_usd %v, want %v; stdout:\n%s", got, want, h.Stdout())
	}
	if usage["unpriced"] == true {
		t.Fatalf("a priced model was reported unpriced:\n%s", h.Stdout())
	}
}

// A model with no price is reported unpriced, with no cost guessed.
func TestHeadlessUnpricedModel(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{Args: []string{"-p", "hi", "-output-format", "json"}, Script: "text \"hello\"\nusage in=6000 out=100"})
	usage, _ := resultOf(t, h.Stdout())["usage"].(map[string]any)
	if usage["cost_usd"] != 0.0 || usage["unpriced"] != true {
		t.Fatalf("usage %v", usage)
	}
}

// -max-budget-usd with a model that has no price fails closed: exit 3, no
// request to the model, and the reason said.
func TestHeadlessDollarBudgetRefusesUnpriced(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{Args: []string{"-p", "hi", "-output-format", "json", "-max-budget-usd", "1"}, Script: `text "hello"`})
	res := resultOf(t, h.Stdout())
	if res["subtype"] != "max_budget" || res["exit_code"] != 3.0 {
		t.Fatalf("result %v", res)
	}
	if msg, _ := res["error"].(string); !strings.Contains(msg, "has no price") {
		t.Fatalf("error %q", msg)
	}
	if n := len(h.Requests()); n != 0 {
		t.Fatalf("%d requests reached the model", n)
	}
}

// Past the dollar cap the run stops at the next turn.
func TestHeadlessDollarBudgetStops(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{Args: []string{"-p", "hi", "-output-format", "json", "-max-budget-usd", "0.001"}, UserConfig: costConfig,
		Script: "tool bash {\"command\":\"true\"}\nusage in=10000 out=100\n\ntext \"done\""})
	res := resultOf(t, h.Stdout())
	if res["subtype"] != "max_budget" {
		t.Fatalf("result %v", res)
	}
	if n := len(h.Requests()); n != 1 {
		t.Fatalf("%d requests; the cap should stop the run after the first", n)
	}
}

// A negative or non-numeric cap is refused before the run.
func TestHeadlessDollarBudgetFlagChecked(t *testing.T) {
	t.Parallel()
	h := piped(t, Opts{Args: []string{"-p", "hi", "-max-budget-usd", "-2"}, Script: `text "hello"`})
	if !strings.Contains(string(h.Output()), "-max-budget-usd must be a positive") {
		t.Fatalf("output:\n%s", h.Output())
	}
}
