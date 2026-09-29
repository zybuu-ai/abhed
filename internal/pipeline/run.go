package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ToolFunc runs a tool. The pipeline does not know what tools exist; the caller
// supplies this, which keeps this package free of the tool registry and the
// policy engine — a step still goes through both, because it goes through the
// caller.
type ToolFunc func(ctx context.Context, name string, args json.RawMessage) (string, error)

// ModelFunc asks the model. schema, when present, requests JSON of that shape.
type ModelFunc func(ctx context.Context, prompt string, schema json.RawMessage) (string, error)

// EventFunc records what the pipeline did, so a reader can see the
// decomposition, the gate verdicts and the reason for each extra hop rather
// than inferring them from tool calls.
type EventFunc func(stage, detail string, data map[string]any)

// Runner executes a pipeline.
type Runner struct {
	Tool  ToolFunc
	Model ModelFunc
	Event EventFunc
	// StepTimeout applies to a step that declares none.
	StepTimeout time.Duration
	// ToolTimes set: a tool step's ctx has no deadline, and Tool applies
	// ToolTimeout(ctx) itself, so a wait for a person's approval is not counted.
	ToolTimes bool
}

type timeoutKey struct{}

// ToolTimeout is the time a tool step may run for, when the Runner leaves it to Tool.
func ToolTimeout(ctx context.Context) (time.Duration, bool) {
	d, ok := ctx.Value(timeoutKey{}).(time.Duration)
	return d, ok
}

// State is what the pipeline has produced so far. It is passed to the model at
// the end, and is what {{references}} resolve against.
type State struct {
	mu     sync.Mutex
	values map[string]any
}

func newState(input string) *State {
	return &State{values: map[string]any{"input": input}}
}

func (s *State) set(key string, v any) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = v
}

// Get returns a value by dotted path.
func (s *State) Get(path string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := any(s.values)
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// All returns a copy of everything produced, for rendering into a prompt.
func (s *State) All() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]any, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out
}

// Result is what a completed pipeline hands back.
type Result struct {
	State    *State
	Failures []string
}

const defaultMaxIterations = 3

// Run executes the pipeline.
//
// Two things are guaranteed here that prose cannot guarantee: a declared step
// runs, and steps in one stage run concurrently. That is the whole point — the
// model is no longer the thing that has to remember.
func (r *Runner) Run(ctx context.Context, p Pipeline, input string) (*Result, error) {
	state := newState(input)
	res := &Result{State: state}

	maxIter := p.MaxIterations
	if maxIter <= 0 {
		maxIter = defaultMaxIterations
	}
	iterations := map[string]int{}

	for i := 0; i < len(p.Stages); i++ {
		stage := p.Stages[i]

		if stage.When != "" && !r.condition(stage.When, state) {
			r.emit(stage.Name, "skipped: "+stage.When, nil)
			continue
		}

		// Record that a stage ran, not only that one was skipped. Without this
		// a reader cannot tell a stage that executed from one that produced no
		// output, which is exactly the question asked of a hop: did the
		// reformulation happen, or did the same search run again?
		if len(stage.Steps) > 0 {
			r.emit(stage.Name, "running", nil)
		}
		if err := r.runSteps(ctx, stage, state, res); err != nil {
			return res, err
		}

		if stage.Gate == nil {
			continue
		}
		verdict, reason, err := r.runGate(ctx, stage, state)
		if err != nil {
			// A gate that cannot be judged must not loop forever; treat an
			// unreadable verdict as satisfied and record why.
			r.emit(stage.Name, "gate failed, continuing: "+err.Error(), nil)
			continue
		}
		if stage.Gate.Output != "" {
			state.set(stage.Gate.Output, map[string]any{
				"satisfied": verdict, "reason": reason,
			})
		}
		r.emit(stage.Name, gateDetail(verdict, reason), map[string]any{
			"satisfied": verdict, "reason": reason,
		})

		if verdict || stage.Gate.Repeat == "" {
			continue
		}
		iterations[stage.Name]++
		if iterations[stage.Name] >= maxIter {
			r.emit(stage.Name, fmt.Sprintf(
				"not satisfied after %d attempts; continuing with what was gathered",
				iterations[stage.Name]), nil)
			continue
		}
		// Jump back. The loop's own i++ moves past the target, so subtract one.
		if at := indexOf(p.Stages, stage.Gate.Repeat); at >= 0 {
			i = at - 1
		}
	}
	return res, nil
}

// runSteps runs a stage's steps, concurrently, expanding any fan-out.
func (r *Runner) runSteps(ctx context.Context, stage Stage, state *State, res *Result) error {
	type job struct {
		step  Step
		item  any
		index int
	}
	var jobs []job
	for _, step := range stage.Steps {
		if step.ForEach == "" {
			jobs = append(jobs, job{step: step, index: -1})
			continue
		}
		items := r.list(step.ForEach, state)
		if len(items) == 0 {
			r.emit(stage.Name, "nothing to fan out over: "+step.ForEach, nil)
			continue
		}
		for n, item := range items {
			jobs = append(jobs, job{step: step, item: item, index: n})
		}
	}
	if len(jobs) == 0 {
		return nil
	}

	type outcome struct {
		out   string
		err   error
		step  Step
		index int
	}
	results := make([]outcome, len(jobs))

	var wg sync.WaitGroup
	for n, j := range jobs {
		wg.Add(1)
		go func(n int, j job) {
			defer wg.Done()
			out, err := r.runStep(ctx, j.step, state, j.item)
			results[n] = outcome{out: out, err: err, step: j.step, index: j.index}
		}(n, j)
	}
	wg.Wait()

	// Collect in job order, so a fan-out's results keep the order of the list
	// they came from rather than the order they finished.
	grouped := map[string][]any{}
	for _, o := range results {
		if o.err != nil {
			msg := fmt.Sprintf("%s: %v", stepLabel(o.step), o.err)
			res.Failures = append(res.Failures, msg)
			r.emit(stage.Name, msg, nil)
			if o.step.Required {
				return fmt.Errorf("required step failed — %s", msg)
			}
			continue
		}
		if o.step.Output == "" {
			continue
		}
		if o.index < 0 {
			state.set(o.step.Output, decode(o.out))
			continue
		}
		grouped[o.step.Output] = append(grouped[o.step.Output], decode(o.out))
	}
	for key, vals := range grouped {
		state.set(key, vals)
	}
	return nil
}

func (r *Runner) runStep(ctx context.Context, s Step, state *State, item any) (string, error) {
	timeout := s.TimeoutOr(r.stepTimeout())
	if s.Kind == "tool" && r.ToolTimes {
		ctx = context.WithValue(ctx, timeoutKey{}, timeout)
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	vals := state.All()
	if item != nil {
		vals["item"] = item
	}

	switch s.Kind {
	case "tool":
		args, err := renderJSON(string(s.Args), vals)
		if err != nil {
			return "", err
		}
		return r.Tool(ctx, s.Tool, args)
	case "model":
		return r.Model(ctx, render(s.Prompt, vals), s.Schema)
	}
	return "", fmt.Errorf("unknown step kind %q", s.Kind)
}

func (r *Runner) runGate(ctx context.Context, stage Stage, state *State) (bool, string, error) {
	schema := json.RawMessage(`{"type":"object","properties":{
	  "satisfied":{"type":"boolean","description":"whether the condition is met"},
	  "reason":{"type":"string","description":"one sentence naming what is missing, or why it is met"}
	},"required":["satisfied","reason"]}`)

	out, err := r.Model(ctx, render(stage.Gate.Ask, state.All()), schema)
	if err != nil {
		return false, "", err
	}
	var v struct {
		Satisfied bool   `json:"satisfied"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(extractJSON(out)), &v); err != nil {
		return false, "", fmt.Errorf("verdict was not readable: %w", err)
	}
	return v.Satisfied, v.Reason, nil
}

// condition evaluates a simple "a.b == value" or "a.b != value" test.
//
// Deliberately not an expression language. A skill author declaring a pipeline
// should not have to learn one, and a condition complicated enough to need it
// is a sign the work belongs in a step rather than in a branch.
var condRe = regexp.MustCompile(`^\s*([\w.]+)\s*(==|!=)\s*(.+?)\s*$`)

func (r *Runner) condition(expr string, state *State) bool {
	m := condRe.FindStringSubmatch(expr)
	if m == nil {
		return true // an unreadable condition does not silently skip a stage
	}
	got, present := state.Get(m[1])
	// A condition on a value nothing has produced yet is not met, whichever
	// operator it uses. Reading "a != b" as true when a does not exist runs a
	// stage before its input exists — which is how a reformulation guarded on a
	// verdict ran on the first pass and replaced the decomposition with a
	// rewrite of nothing.
	if !present {
		return false
	}
	want := strings.Trim(strings.TrimSpace(m[3]), `"'`)
	equal := fmt.Sprint(got) == want
	if m[2] == "!=" {
		return !equal
	}
	return equal
}

func (r *Runner) list(ref string, state *State) []any {
	v, ok := state.Get(ref)
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []any:
		return t
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out
	}
	return []any{v}
}

func (r *Runner) emit(stage, detail string, data map[string]any) {
	if r.Event != nil {
		r.Event(stage, detail, data)
	}
}

func (r *Runner) stepTimeout() time.Duration {
	if r.StepTimeout > 0 {
		return r.StepTimeout
	}
	return 2 * time.Minute
}

func indexOf(stages []Stage, name string) int {
	for i, s := range stages {
		if s.Name == name {
			return i
		}
	}
	return -1
}

func stepLabel(s Step) string {
	if s.Kind == "tool" {
		return "tool " + s.Tool
	}
	return "model step"
}

func gateDetail(satisfied bool, reason string) string {
	if satisfied {
		return "satisfied: " + reason
	}
	return "not satisfied: " + reason
}

// decode parses a step's output as JSON when it is JSON, so later steps can
// reference its fields. Prose is kept as a string.
func decode(out string) any {
	trimmed := strings.TrimSpace(extractJSON(out))
	if trimmed == "" {
		return out
	}
	var v any
	if json.Unmarshal([]byte(trimmed), &v) == nil {
		return v
	}
	return out
}

// extractJSON finds a JSON object or array in text that may carry prose around
// it. Models add a sentence before their JSON more often than not, and failing
// the step for that would fail on presentation rather than substance.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	for _, pair := range [][2]byte{{'{', '}'}, {'[', ']'}} {
		start := strings.IndexByte(s, pair[0])
		end := strings.LastIndexByte(s, pair[1])
		if start >= 0 && end > start {
			return s[start : end+1]
		}
	}
	return s
}

// render substitutes {{path}} references from state.
var refRe = regexp.MustCompile(`\{\{\s*([\w.]+)\s*\}\}`)

func render(tmpl string, vals map[string]any) string {
	return refRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		path := strings.Trim(strings.Trim(m, "{}"), " ")
		cur := any(vals)
		for _, part := range strings.Split(path, ".") {
			mm, ok := cur.(map[string]any)
			if !ok {
				return ""
			}
			cur, ok = mm[part]
			if !ok {
				return ""
			}
		}
		switch t := cur.(type) {
		case string:
			return t
		default:
			b, err := json.Marshal(cur)
			if err != nil {
				return fmt.Sprint(cur)
			}
			return string(b)
		}
	})
}

// renderJSON substitutes {{path}} references into a tool step's JSON argument
// template, escaping the substituted values so untrusted content cannot inject
// JSON structure.
//
// A tool step's args are a JSON template — e.g. {"query":"{{input}}"} — and
// input is often a prior step's output or a RAG result, which is not the
// pipeline author's text. render() returns a string verbatim, so a value
// containing a quote (or `","evil":"…`) would close the JSON string and add or
// change fields in the call the tool actually receives. renderJSON escapes each
// string substitution as a JSON string body, so it stays one string value, and
// validates the rendered result so anything that still does not parse is
// refused rather than handed to a tool.
func renderJSON(tmpl string, vals map[string]any) (json.RawMessage, error) {
	if strings.TrimSpace(tmpl) == "" {
		return json.RawMessage("{}"), nil
	}
	out := refRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		path := strings.Trim(strings.Trim(m, "{}"), " ")
		cur := any(vals)
		for _, part := range strings.Split(path, ".") {
			mm, ok := cur.(map[string]any)
			if !ok {
				return ""
			}
			cur, ok = mm[part]
			if !ok {
				return ""
			}
		}
		switch t := cur.(type) {
		case string:
			// The template supplies the quotes around a "{{ref}}"; what has to
			// be neutralised is any quote, backslash or control byte INSIDE the
			// value. Marshal yields a quoted JSON string with the interior
			// escaped; drop the surrounding quotes and keep the escaped body.
			b, err := json.Marshal(t)
			if err != nil {
				return ""
			}
			return string(b[1 : len(b)-1])
		default:
			b, err := json.Marshal(cur)
			if err != nil {
				return ""
			}
			return string(b)
		}
	})
	if !json.Valid([]byte(out)) {
		return nil, fmt.Errorf("pipeline: step arguments did not render to valid JSON")
	}
	return json.RawMessage(out), nil
}
