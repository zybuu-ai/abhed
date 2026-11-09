package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// stopHooks always ask the run to go on, and count how often they were asked.
type stopHooks struct {
	fakeHooks
	asked int
}

func (s *stopHooks) Stop(_ context.Context, _, answer string) string {
	s.asked++
	return "run the tests before you finish (after " + answer + ")"
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
