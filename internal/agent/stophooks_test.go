package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// stopHooks always ask the run to go on, and count how often they were asked.
type stopHooks struct {
	fakeHooks
	asked int
}

func (s *stopHooks) Stop(_ context.Context, _, answer string) (string, string) {
	s.asked++
	return "tests-first", "run the tests before you finish (after " + answer + ")"
}

// A stop hook sends a completed run back to work with its instruction, and
// no more than MaxStopContinues times however often it asks.
func TestStopHookContinuesABoundedNumberOfTimes(t *testing.T) {
	l, _, _ := harness(t, []scriptedTurn{{text: "first"}, {text: "second"}, {text: "third"}, {text: "fourth"}, {text: "fifth"}},
		policy.ModeDefault, true)
	hooks := &stopHooks{}
	l.Hooks = hooks
	reason, err := l.Run(context.Background(), "do it")
	if err != nil || reason != TermCompleted {
		t.Fatalf("%s %v", reason, err)
	}
	if got := len(l.Adapter.(*scriptedAdapter).gotRequests); got != 1+MaxStopContinues {
		t.Fatalf("the model was called %d times, want %d", got, 1+MaxStopContinues)
	}
	if hooks.asked != MaxStopContinues {
		t.Fatalf("the stop hook was asked %d times, want %d", hooks.asked, MaxStopContinues)
	}
	var told int
	for _, m := range l.Messages() {
		if m.Role == model.RoleUser && strings.HasPrefix(m.Content, "run the tests before you finish") {
			told++
		}
	}
	if told != MaxStopContinues {
		t.Fatalf("the instruction reached the model %d times", told)
	}
	if !strings.Contains(l.Messages()[2].Content, "after first") {
		t.Fatalf("the hook was not given the answer: %q", l.Messages()[2].Content)
	}
	// The next run has its own allowance.
	if _, err := l.Run(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if hooks.asked != 2*MaxStopContinues {
		t.Fatalf("a second run's stop hook was asked %d times in all", hooks.asked)
	}
}

// Hooks that cannot continue a run, as the extensions host's are without a
// stop hook, leave the run to end as before.
func TestNoStopHookEndsTheRun(t *testing.T) {
	l, _, _ := harness(t, []scriptedTurn{{text: "only"}}, policy.ModeDefault, true)
	l.Hooks = &fakeHooks{}
	if _, err := l.Run(context.Background(), "do it"); err != nil {
		t.Fatal(err)
	}
	if got := len(l.Adapter.(*scriptedAdapter).gotRequests); got != 1 {
		t.Fatalf("the model was called %d times", got)
	}
}

// A tool that fails is told to the hooks as tool_failure, with its output.
func TestToolFailureIsObserved(t *testing.T) {
	l, _, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("read", map[string]string{"path": "/no/such/file"})}},
		{text: "done"},
	}, policy.ModeAuto, false)
	hooks := &fakeHooks{}
	l.Hooks = hooks
	if _, err := l.Run(context.Background(), "read it"); err != nil {
		t.Fatal(err)
	}
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if !slices.ContainsFunc(hooks.seen, func(s string) bool { return strings.HasPrefix(s, HookToolFailure+":read:") }) {
		t.Fatalf("hooks heard %v", hooks.seen)
	}
}

// History summarized, automatically or on request, is told to the hooks as
// post_compact with what started it.
func TestPostCompactIsObserved(t *testing.T) {
	l, _, _ := pairLoop(t)
	hooks := &fakeHooks{}
	l.Hooks = hooks
	l.messages = append([]model.Message{{Role: model.RoleUser, Content: "first"},
		{Role: model.RoleAssistant, Content: "reply"}}, summaryThenFourExchanges()[1:]...)
	if err := l.compactNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if !slices.Contains(hooks.seen, HookPostCompact+"::auto") {
		t.Fatalf("hooks heard %v", hooks.seen)
	}
}

// A subagent's start is told to the hooks as subagent_start with its task,
// before its end is told as subagent_end.
func TestSubagentStartIsObserved(t *testing.T) {
	adapter := &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{call("task", map[string]string{"prompt": "look around", "description": "looker"})}},
		{text: "looked"},
		{text: "done"},
	}}
	l, _, _ := taskTree(t, adapter, AutoApprove{Yes: true}, NewMemStore(), NewMemStore(), false)
	hooks := &fakeHooks{}
	l.Hooks = hooks
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	start := slices.IndexFunc(hooks.seen, func(s string) bool {
		return strings.HasPrefix(s, HookSubagentStart+":task:") && strings.Contains(s, "look around")
	})
	end := slices.IndexFunc(hooks.seen, func(s string) bool { return strings.HasPrefix(s, HookSubagentEnd+":task:") })
	if start < 0 || end < start {
		t.Fatalf("hooks heard %v", hooks.seen)
	}
}

// leaky fails, printing a stored secret's value.
type leaky struct{}

func (leaky) Name() string            { return "leaky" }
func (leaky) Description() string     { return "fails loudly" }
func (leaky) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (leaky) Mutates() bool           { return false }
func (leaky) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	return tools.Result{Content: "connect failed with password hunter2-hunter2", IsError: true}
}

// secretLoop is a loop whose record withholds DB_PASS, on scripted turns.
func secretLoop(t *testing.T, reg *tools.Registry, turns []scriptedTurn) (*Loop, *MemStore, *scriptedAdapter) {
	t.Helper()
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("DB_PASS", "hunter2-hunter2"); err != nil {
		t.Fatal(err)
	}
	sess, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	rec := NewRecorder(store, "sess1", "")
	rec.Redact = vault.Redactor()
	ad := &scriptedAdapter{turns: turns}
	return NewLoop(ad, reg, policy.New(policy.ModeBypass), AutoApprove{Yes: true}, sess, rec, DefaultConfig()), store, ad
}

// resultHooks see results, and put a stored value back into one.
type resultHooks struct {
	fakeHooks
	results []string
}

func (r *resultHooks) ToolResult(_ context.Context, _, tool, content string, isErr bool) (string, bool) {
	r.results = append(r.results, content)
	return content + " (hook says: hunter2-hunter2)", isErr
}

// A failed tool's output reaches tool_failure, tool_result and the model
// with a stored value redacted, and a hook cannot put one back.
func TestHooksSeeRedactedResults(t *testing.T) {
	l, _, ad := secretLoop(t, tools.NewRegistry(leaky{}), []scriptedTurn{
		{calls: []model.ToolCall{{ID: "c1", Name: "leaky", Args: json.RawMessage(`{}`)}}},
		{text: "done"},
	})
	hooks := &resultHooks{}
	l.Hooks = hooks
	if _, err := l.Run(context.Background(), "try"); err != nil {
		t.Fatal(err)
	}
	hooks.mu.Lock()
	seen := slices.Clone(hooks.seen)
	hooks.mu.Unlock()
	failure := slices.IndexFunc(seen, func(s string) bool { return strings.HasPrefix(s, HookToolFailure+":leaky:") })
	if failure < 0 || !strings.Contains(seen[failure], "[secret:DB_PASS]") {
		t.Fatalf("tool_failure heard %v", seen)
	}
	for _, s := range append(seen, hooks.results...) {
		if strings.Contains(s, "hunter2-hunter2") {
			t.Fatalf("a hook was given the stored value: %q", s)
		}
	}
	if len(hooks.results) != 1 {
		t.Fatalf("tool_result was asked %d times", len(hooks.results))
	}
	for _, m := range ad.gotRequests[len(ad.gotRequests)-1].Messages {
		if strings.Contains(m.Content, "hunter2-hunter2") {
			t.Fatalf("the model read the stored value: %q", m.Content)
		}
		if m.Role == model.RoleTool && !strings.Contains(m.Content, "hook says") {
			t.Fatalf("the hook's rewrite did not reach the model: %q", m.Content)
		}
	}
}

// secretStop sends the run back once with an instruction holding a stored value.
type secretStop struct {
	fakeHooks
	done bool
}

func (s *secretStop) Stop(context.Context, string, string) (string, string) {
	if s.done {
		return "", ""
	}
	s.done = true
	return "tests-first", "the tests failed: login hunter2-hunter2 refused"
}

// A stop hook's instruction is withheld of secrets, recorded as the exact
// text the model read, and read again by a resumed or forked session.
func TestStopInstructionIsWithheldRecordedAndReplayed(t *testing.T) {
	l, store, ad := secretLoop(t, tools.NewRegistry(), []scriptedTurn{{text: "first"}, {text: "second"}})
	l.Hooks = &secretStop{}
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	want := "the tests failed: login [secret:DB_PASS] refused"
	if got := ad.gotRequests[1].Messages; got[len(got)-1].Content != want {
		t.Fatalf("the model read %q", got[len(got)-1].Content)
	}
	evs, _ := store.Events("sess1")
	var c HookContinued
	for _, ev := range evs {
		if ev.Type == EvHookContinued {
			_ = json.Unmarshal(ev.Payload, &c)
		}
	}
	if c.Text != want || c.Hook != "tests-first" || c.SecretsWithheld != "DB_PASS" {
		t.Fatalf("recorded %+v", c)
	}
	msgs, err := Fork(evs, 0)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range msgs {
		roles = append(roles, string(m.Role)+":"+m.Content)
	}
	if !slices.Equal(roles, []string{"user:go", "assistant:first", "user:" + want, "assistant:second"}) {
		t.Fatalf("rebuilt %q", roles)
	}
}

// haltHooks end the run once a tool has run.
type haltHooks struct {
	fakeHooks
	why string
}

func (h *haltHooks) Observe(_ context.Context, event, _, _, _ string) {
	if event == HookToolFailure {
		h.why = "the freeze hook ended the run"
	}
}

func (h *haltHooks) Halted(string) string {
	why := h.why
	h.why = ""
	return why
}

// A hook that ends the run ends it at the next boundary as policy_denied,
// with its reason, before the model is called again.
func TestHookHaltsTheRun(t *testing.T) {
	l, store, ad := secretLoop(t, tools.NewRegistry(leaky{}), []scriptedTurn{
		{calls: []model.ToolCall{{ID: "c1", Name: "leaky", Args: json.RawMessage(`{}`)}}},
		{text: "never"},
	})
	l.Hooks = &haltHooks{}
	reason, err := l.Run(context.Background(), "try")
	if err != nil || reason != TermPolicyDenied {
		t.Fatalf("%s %v", reason, err)
	}
	if len(ad.gotRequests) != 1 {
		t.Fatalf("the model was called %d times", len(ad.gotRequests))
	}
	evs, _ := store.Events("sess1")
	var end SessionEnded
	for _, ev := range evs {
		if ev.Type == EvSessionEnded {
			_ = json.Unmarshal(ev.Payload, &end)
		}
	}
	if end.Detail != "the freeze hook ended the run" {
		t.Fatalf("ended %+v", end)
	}
}
