package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// fakeHooks refuses prompts and calls that name a word, and keeps what it
// was told.
type fakeHooks struct {
	refusePrompt, refuseCall string
	mu                       sync.Mutex
	seen                     []string
}

func (f *fakeHooks) note(s string) {
	f.mu.Lock()
	f.seen = append(f.seen, s)
	f.mu.Unlock()
}

func (f *fakeHooks) PromptSubmitted(_ context.Context, _ string, text string) string {
	f.note("prompt:" + text)
	if f.refusePrompt != "" && strings.Contains(text, f.refusePrompt) {
		return "prompt refused by hook"
	}
	return ""
}

func (f *fakeHooks) PermissionRequested(_ context.Context, _ string, tool string, args json.RawMessage, _ string) string {
	f.note("permission:" + tool)
	if f.refuseCall != "" && strings.Contains(string(args), f.refuseCall) {
		return "call refused by hook"
	}
	return ""
}

func (f *fakeHooks) Observe(_ context.Context, event, _, tool, detail string) {
	f.note(event + ":" + tool + ":" + detail)
}

// A refused prompt is never recorded or sent, and the run does not start.
func TestHookRefusesAPrompt(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{{text: "never"}}, policy.ModeDefault, true)
	hooks := &fakeHooks{refusePrompt: "forbidden"}
	l.Hooks = hooks
	reason, err := l.Run(context.Background(), "the forbidden thing")
	if err != nil || reason != TermPromptRefused {
		t.Fatalf("%s %v", reason, err)
	}
	evs, _ := store.Events("sess1")
	if hasEvent(evs, EvUserMessage) || len(l.Adapter.(*scriptedAdapter).gotRequests) != 0 {
		t.Fatal("a refused prompt was recorded or sent")
	}
	// A steering message a hook refuses is dropped, and said to be.
	l.Steer("more forbidden talk")
	if _, err := l.Run(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	evs, _ = store.Events("sess1")
	if !hasEvent(evs, EvMessageDropped) {
		t.Fatal("a refused steering message was not recorded as dropped")
	}
	for _, m := range l.Messages() {
		if strings.Contains(m.Content, "forbidden") {
			t.Fatal("a refused steering message reached the model")
		}
	}
}

// A permission_request hook's refusal stops a call before the person is
// asked; otherwise the hooks hear that the person is needed, and the run's end.
func TestHookRefusesAPermissionRequest(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "make deploy"})}},
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "make test"})}},
		{text: "done"},
	}, policy.ModeDefault, true)
	hooks := &fakeHooks{refuseCall: "deploy"}
	l.Hooks = hooks
	counter := &askCounter{}
	l.Approver = counter
	if _, err := l.Run(context.Background(), "ship it"); err != nil {
		t.Fatal(err)
	}
	if counter.asked != 1 {
		t.Fatalf("asked %d times; the refused call must not be asked", counter.asked)
	}
	steps := deniedSteps(t, store)
	if len(steps) == 0 || steps[0] != "hook" {
		t.Fatalf("denied at %v", steps)
	}
	hooks.mu.Lock()
	seen := slices.Clone(hooks.seen)
	hooks.mu.Unlock()
	if !slices.ContainsFunc(seen, func(s string) bool { return strings.HasPrefix(s, "notification:bash:approval needed") }) ||
		seen[len(seen)-1] != "turn_end::completed" {
		t.Fatalf("hooks heard %v", seen)
	}
	if slices.ContainsFunc(seen, func(s string) bool { return strings.HasPrefix(s, "notification") && strings.Contains(s, "deploy") }) {
		t.Fatal("a refused call was announced as needing the person")
	}
}

// An approval or denial a rule made names the rule in the record; one the
// mode made names none.
func TestRecordedDecisionsNameTheRule(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{
			call("bash", map[string]string{"command": "go test ./..."}),
			call("bash", map[string]string{"command": "curl http://x"}),
			call("glob", map[string]string{"pattern": "*.go"}),
		}},
		{text: "done"},
	}, policy.ModeAuto, false)
	if err := l.Policy.AddAllow("bash(go test*)"); err != nil {
		t.Fatal(err)
	}
	if err := l.Policy.AddDeny("bash(curl *)"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("sess1")
	var got []string
	for _, e := range evs {
		if e.Type != EvActionApproved && e.Type != EvActionDenied {
			continue
		}
		var p map[string]string
		_ = json.Unmarshal(e.Payload, &p)
		got = append(got, string(e.Type)+" "+p["step"]+" "+p["rule"])
	}
	want := []string{"action.approved allow bash(go test*)", "action.denied deny bash(curl *)", "action.approved mode "}
	if !slices.Equal(got, want) {
		t.Fatalf("recorded %q, want %q", got, want)
	}
}

// A subagent's call that would be put to the person is screened by the
// parent's hooks first, and a refusal there means nobody is asked.
func TestSubagentAskIsVetoedByTheParentsHooks(t *testing.T) {
	parent, _, _ := harness(t, []scriptedTurn{{text: "parent"}}, policy.ModeDefault, true)
	hooks := &fakeHooks{refuseCall: "deploy"}
	parent.Hooks = hooks
	f := subFactory(t, nil, NewBudget(1_000_000, 10, false))
	counter := &askCounter{}
	f.Approver = counter
	args, _ := json.Marshal(map[string]string{"path": f.Workspace + "/deploy.txt", "content": "x"})
	// A subagent runs on the model its parent runs on now.
	parent.Adapter = &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{{ID: "w1", Name: "write", Args: args}}},
		{text: "done"},
	}}
	if _, err := f.Spawn(parent.asParent(context.Background()), SubagentRequest{
		Prompt: "write it", Description: "write", AgentType: "general",
	}); err != nil {
		t.Fatal(err)
	}
	if counter.asked != 0 {
		t.Fatalf("the person was asked %d times about a call the hook refused", counter.asked)
	}
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if !slices.Contains(hooks.seen, "permission:write") {
		t.Fatalf("the parent's hooks never saw the subagent's call: %v", hooks.seen)
	}
	// The subagent's task is not a person's message, and its end is the
	// parent's subagent_end, not a turn_end of its own.
	for _, s := range hooks.seen {
		if strings.HasPrefix(s, "prompt:") || strings.HasPrefix(s, "turn_end") {
			t.Fatalf("the subagent's own run reached the hooks as %q", s)
		}
	}
}
