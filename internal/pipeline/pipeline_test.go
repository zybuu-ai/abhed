package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func runner(tool ToolFunc, model ModelFunc) *Runner {
	return &Runner{Tool: tool, Model: model, StepTimeout: 5 * time.Second}
}

// The failure that motivated all of this: a model wrote a correct plan ending
// "I will now call retrieve for both", then called something else and never
// retrieved. A declared step is not a suggestion.
func TestADeclaredStepAlwaysRuns(t *testing.T) {
	var called []string
	var mu sync.Mutex
	tool := func(_ context.Context, name string, _ json.RawMessage) (string, error) {
		mu.Lock()
		called = append(called, name)
		mu.Unlock()
		return `{"documents":["d1"]}`, nil
	}
	// A model that would rather do anything else.
	model := func(context.Context, string, json.RawMessage) (string, error) {
		return "I have decided to skip retrieval.", nil
	}

	p := Pipeline{Stages: []Stage{{
		Name:  "retrieve",
		Steps: []Step{{Kind: "tool", Tool: "retrieve", Output: "docs", Required: true}},
	}}}
	if _, err := runner(tool, model).Run(context.Background(), p, "q"); err != nil {
		t.Fatal(err)
	}
	if len(called) != 1 || called[0] != "retrieve" {
		t.Fatalf("tools called = %v; a declared step must run whatever the model prefers", called)
	}
}

// Retrieval per sub-question, and web search, should overlap rather than
// queue. The harness cannot parallelise a decision the model makes serially,
// so the pipeline decides instead.
func TestStepsInAStageRunConcurrently(t *testing.T) {
	var inFlight, peak int32
	tool := func(ctx context.Context, name string, _ json.RawMessage) (string, error) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return `"ok"`, nil
	}
	model := func(context.Context, string, json.RawMessage) (string, error) {
		return `["a","b","c"]`, nil
	}

	p := Pipeline{Stages: []Stage{
		{Name: "decompose", Steps: []Step{{Kind: "model", Prompt: "split", Output: "subs"}}},
		{Name: "gather", Steps: []Step{
			{Kind: "tool", Tool: "retrieve", ForEach: "subs", Output: "docs"},
			{Kind: "tool", Tool: "web_search", Output: "web"},
		}},
	}}

	start := time.Now()
	if _, err := runner(tool, model).Run(context.Background(), p, "q"); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	// Three retrievals plus a web search: four 60ms calls.
	if got := atomic.LoadInt32(&peak); got < 4 {
		t.Errorf("peak concurrency = %d, want 4 — the fan-out and the search must overlap", got)
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("took %s; four 60ms calls ran in series", elapsed)
	}
}

// A fan-out runs once per element, and the results keep the order of the list
// they came from rather than the order they finished.
func TestFanOutKeepsListOrder(t *testing.T) {
	tool := func(_ context.Context, _ string, args json.RawMessage) (string, error) {
		var a struct{ Q string }
		_ = json.Unmarshal(args, &a)
		// The first is slowest, so finishing order differs from list order.
		if a.Q == "first" {
			time.Sleep(80 * time.Millisecond)
		}
		return fmt.Sprintf("%q", "doc-for-"+a.Q), nil
	}
	model := func(context.Context, string, json.RawMessage) (string, error) {
		return `["first","second","third"]`, nil
	}

	p := Pipeline{Stages: []Stage{
		{Name: "decompose", Steps: []Step{{Kind: "model", Prompt: "split", Output: "subs"}}},
		{Name: "retrieve", Steps: []Step{{
			Kind: "tool", Tool: "retrieve", ForEach: "subs", Output: "docs",
			Args: json.RawMessage(`{"q":"{{item}}"}`),
		}}},
	}}
	res, err := runner(tool, model).Run(context.Background(), p, "q")
	if err != nil {
		t.Fatal(err)
	}
	docs, _ := res.State.Get("docs")
	list, ok := docs.([]any)
	if !ok || len(list) != 3 {
		t.Fatalf("docs = %#v, want three results", docs)
	}
	if list[0] != "doc-for-first" {
		t.Errorf("results are in completion order, not list order: %v", list)
	}
}

// A gate that is not satisfied sends the pipeline back, and a bound stops it
// looping forever on a corpus that will never satisfy it.
func TestGateRepeatsAndIsBounded(t *testing.T) {
	var retrievals int32
	tool := func(context.Context, string, json.RawMessage) (string, error) {
		atomic.AddInt32(&retrievals, 1)
		return `"doc"`, nil
	}
	model := func(_ context.Context, prompt string, schema json.RawMessage) (string, error) {
		if schema != nil { // the gate
			return `{"satisfied":false,"reason":"nothing covers the second part"}`, nil
		}
		return `"reformulated"`, nil
	}

	p := Pipeline{
		MaxIterations: 3,
		Stages: []Stage{
			{Name: "retrieve", Steps: []Step{{Kind: "tool", Tool: "retrieve", Output: "docs"}}},
			{Name: "check", Gate: &Gate{Ask: "is it enough?", Repeat: "retrieve", Output: "sufficiency"}},
		},
	}
	res, err := runner(tool, model).Run(context.Background(), p, "q")
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&retrievals); got != 3 {
		t.Errorf("retrieved %d times, want 3 (the declared bound)", got)
	}
	// The verdict is recorded, so a reader sees why the extra hops happened.
	if v, ok := res.State.Get("sufficiency.reason"); !ok || !strings.Contains(fmt.Sprint(v), "second part") {
		t.Errorf("the gate's reasoning was not recorded: %v", v)
	}
}

// A required step failing stops the pipeline; a best-effort one does not.
func TestRequiredAndBestEffortDifferOnFailure(t *testing.T) {
	failing := func(context.Context, string, json.RawMessage) (string, error) {
		return "", fmt.Errorf("the corpus is unreachable")
	}
	model := func(context.Context, string, json.RawMessage) (string, error) { return `"ok"`, nil }

	required := Pipeline{Stages: []Stage{{
		Name:  "retrieve",
		Steps: []Step{{Kind: "tool", Tool: "retrieve", Required: true}},
	}}}
	if _, err := runner(failing, model).Run(context.Background(), required, "q"); err == nil {
		t.Error("a required step that fails must stop the pipeline")
	}

	optional := Pipeline{Stages: []Stage{{
		Name:  "search",
		Steps: []Step{{Kind: "tool", Tool: "web_search"}},
	}}}
	res, err := runner(failing, model).Run(context.Background(), optional, "q")
	if err != nil {
		t.Fatalf("a best-effort step must not stop the pipeline: %v", err)
	}
	if len(res.Failures) != 1 || !strings.Contains(res.Failures[0], "unreachable") {
		t.Errorf("the failure should be recorded and passed on: %v", res.Failures)
	}
}

// A conditional stage is skipped when its condition does not hold — the
// "only decompose a complex query" case.
func TestConditionalStageIsSkipped(t *testing.T) {
	var decomposed int32
	model := func(_ context.Context, prompt string, _ json.RawMessage) (string, error) {
		if strings.Contains(prompt, "split") {
			atomic.AddInt32(&decomposed, 1)
			return `["a","b"]`, nil
		}
		return `{"complexity":"simple"}`, nil
	}
	tool := func(context.Context, string, json.RawMessage) (string, error) { return `"d"`, nil }

	p := Pipeline{Stages: []Stage{
		{Name: "classify", Steps: []Step{{Kind: "model", Prompt: "classify", Output: "class"}}},
		{Name: "decompose", When: "class.complexity == complex",
			Steps: []Step{{Kind: "model", Prompt: "split", Output: "subs"}}},
	}}
	if _, err := runner(tool, model).Run(context.Background(), p, "q"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&decomposed); got != 0 {
		t.Errorf("decompose ran %d times on a simple query", got)
	}
}

// A model that wraps its JSON in prose or a code fence is normal, and failing
// the step for that would fail on presentation rather than substance.
func TestJSONIsFoundInsideProse(t *testing.T) {
	for _, out := range []string{
		`{"complexity":"complex"}`,
		"Here is the classification:\n```json\n{\"complexity\":\"complex\"}\n```",
		"Sure! {\"complexity\":\"complex\"} — hope that helps.",
	} {
		v := decode(out)
		m, ok := v.(map[string]any)
		if !ok || m["complexity"] != "complex" {
			t.Errorf("could not read JSON from %q: got %#v", out, v)
		}
	}
}

// A stage guarded on a value no earlier stage has produced yet must not run.
// The reformulation stage sits before the search it feeds and is guarded on a
// verdict that only exists after the first pass; running it on the first pass
// would overwrite the decomposition with a rewrite of nothing.
func TestConditionOnAnUnsetValueSkipsTheStage(t *testing.T) {
	var reformulated int32
	model := func(_ context.Context, prompt string, _ json.RawMessage) (string, error) {
		if strings.Contains(prompt, "gap") {
			atomic.AddInt32(&reformulated, 1)
			return `["rewritten"]`, nil
		}
		return `["a","b"]`, nil
	}
	tool := func(context.Context, string, json.RawMessage) (string, error) { return `"d"`, nil }

	p := Pipeline{Stages: []Stage{
		{Name: "decompose", Steps: []Step{{Kind: "model", Prompt: "split", Output: "subs"}}},
		{Name: "reformulate", When: "verdict.satisfied != true",
			Steps: []Step{{Kind: "model", Prompt: "target the gap", Output: "subs"}}},
		{Name: "gather", Steps: []Step{{Kind: "tool", Tool: "retrieve", ForEach: "subs", Output: "docs"}}},
	}}
	if _, err := runner(tool, model).Run(context.Background(), p, "q"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&reformulated); got != 0 {
		t.Fatalf("reformulate ran %d times on the first pass, before any verdict "+
			"existed; it would have replaced the decomposition with a rewrite "+
			"of nothing", got)
	}
}

// After a gate has run, a stage guarded on its verdict must run. The gate
// stores a boolean; comparing it to the string "true" has to work, or the
// reformulation a hop depends on never happens and every hop repeats the same
// search.
func TestConditionMatchesABooleanVerdict(t *testing.T) {
	st := newState("q")
	st.set("sufficiency", map[string]any{"satisfied": false, "reason": "gap"})
	r := &Runner{}
	if !r.condition("sufficiency.satisfied != true", st) {
		t.Error(`satisfied=false must satisfy "!= true"`)
	}
	st.set("sufficiency", map[string]any{"satisfied": true, "reason": "ok"})
	if r.condition("sufficiency.satisfied != true", st) {
		t.Error(`satisfied=true must not satisfy "!= true"`)
	}
}

// With ToolTimes the tool step gets its timeout to apply itself, and no deadline;
// without it the Runner imposes the deadline as before.
func TestToolTimesHandsTheTimeoutToTheTool(t *testing.T) {
	p := Pipeline{Stages: []Stage{{Name: "s", Steps: []Step{{Kind: "tool", Tool: "t", TimeoutMS: 70}}}}}
	for _, own := range []bool{true, false} {
		var got time.Duration
		var deadline bool
		r := runner(func(ctx context.Context, _ string, _ json.RawMessage) (string, error) {
			got, _ = ToolTimeout(ctx)
			_, deadline = ctx.Deadline()
			return "", nil
		}, nil)
		r.ToolTimes = own
		if _, err := r.Run(context.Background(), p, ""); err != nil {
			t.Fatal(err)
		}
		if own && (got != 70*time.Millisecond || deadline) {
			t.Fatalf("ToolTimes: timeout %v, deadline %v", got, deadline)
		}
		if !own && (got != 0 || !deadline) {
			t.Fatalf("default: timeout %v, deadline %v", got, deadline)
		}
	}
}

// The guide's example is copied by readers, so it must load as written.
func TestGuideExamplePipelineValidates(t *testing.T) {
	data, err := os.ReadFile("../../docs/guide/example-pipeline.json")
	if err != nil {
		t.Fatal(err)
	}
	var p Pipeline
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}
