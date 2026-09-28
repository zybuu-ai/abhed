package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// namedAdapter is a scripted model that names itself, so a test can tell
// which of two answered.
type namedAdapter struct {
	*scriptedAdapter
	name   string
	window int
}

func (n namedAdapter) Profile() model.Profile {
	return model.Profile{Name: n.name, ContextWindow: n.window}
}

func named(name string, window int) namedAdapter {
	return namedAdapter{scriptedAdapter: &scriptedAdapter{}, name: name, window: window}
}

func switchLoop(t *testing.T, a model.Adapter) (*Loop, *MemStore) {
	t.Helper()
	dir := tempDir(t)
	sess, err := tools.NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	cfg := DefaultConfig()
	cfg.SystemPrompt = BuildSystemPrompt(BuildOptions{
		Profile: "main", Workspace: dir, Model: a.Profile().Name, ContextWindow: a.Profile().ContextWindow,
	})
	l := NewLoop(a, tools.NewRegistry(tools.Read{}), policy.New(policy.ModeDefault),
		AutoApprove{Yes: true}, sess, NewRecorder(store, "sess1", ""), cfg)
	return l, store
}

// A switched session's next turn goes to the new model, which the prompt
// names, and the record says which model each call went to and why.
func TestSwitchModelAnswersTheNextTurnAndIsRecorded(t *testing.T) {
	a, b := named("model-a", 1000), named("model-b", 2000)
	l, store := switchLoop(t, a)
	if _, err := l.Run(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if err := l.SwitchModel("b", b); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	if len(a.gotRequests) != 1 || len(b.gotRequests) != 1 {
		t.Fatalf("model-a answered %d turns and model-b %d; want 1 each", len(a.gotRequests), len(b.gotRequests))
	}
	sys := b.gotRequests[0].System
	if !strings.Contains(sys, "Model: model-b · Context window: 2000 tokens") || strings.Contains(sys, "model-a") {
		t.Fatalf("the prompt sent to model-b does not name it:\n%s", sys)
	}

	events, _ := store.Events("sess1")
	var calls []string
	var switched ModelSwitched
	for _, ev := range events {
		switch ev.Type {
		case EvModelCall:
			var mc ModelCall
			_ = json.Unmarshal(ev.Payload, &mc)
			calls = append(calls, mc.Model)
		case EvModelSwitched:
			_ = json.Unmarshal(ev.Payload, &switched)
		}
	}
	if strings.Join(calls, ",") != "model-a,model-b" {
		t.Fatalf("the record names the calls' models as %q", calls)
	}
	if switched != (ModelSwitched{Provider: "b", Model: "model-b", From: "model-a"}) {
		t.Fatalf("the switch was recorded as %+v", switched)
	}
	if got := ProviderOf(events); got != "b" {
		t.Fatalf("ProviderOf = %q, want b", got)
	}
}

// A switch the record refuses is not made: the model that answers is always
// one the record names.
func TestSwitchModelRefusedByTheRecordIsNotMade(t *testing.T) {
	a, b := named("model-a", 1000), named("model-b", 2000)
	l, _ := switchLoop(t, a)
	l.Recorder.Gate = func() error { return errors.New("claimed elsewhere") }
	if err := l.SwitchModel("b", b); err == nil {
		t.Fatal("a switch the record refused reported success")
	}
	if l.Adapter.Profile().Name != "model-a" || strings.Contains(l.Config.SystemPrompt, "model-b") {
		t.Fatal("a switch the record refused was made anyway")
	}
}

// A subagent runs on the model its parent runs on now, not the one the
// factory was built with at startup.
func TestSubagentRunsOnTheParentsCurrentModel(t *testing.T) {
	startup, now := named("model-a", 1000), named("model-b", 2000)
	f := subFactory(t, nil, NewBudget(1_000_000, 10, false))
	f.Adapter = startup
	parent, _ := switchLoop(t, startup)
	parent.SetAdapter(now)
	if _, err := f.Spawn(parent.asParent(context.Background()), SubagentRequest{
		Prompt: "look", Description: "look around", AgentType: "explore",
	}); err != nil {
		t.Fatal(err)
	}
	if len(startup.gotRequests) != 0 || len(now.gotRequests) != 1 {
		t.Fatalf("the subagent's calls went to model-a %d times and model-b %d", len(startup.gotRequests), len(now.gotRequests))
	}
	if !strings.Contains(now.gotRequests[0].System, "Model: model-b") {
		t.Fatalf("the subagent's prompt does not name its model:\n%s", now.gotRequests[0].System)
	}
}

// ProviderOf reads the latest switch, or else the session's start.
func TestProviderOfReadsTheLatestChoice(t *testing.T) {
	ev := func(typ EventType, payload string) Event { return Event{Type: typ, Payload: json.RawMessage(payload)} }
	for _, c := range []struct {
		events []Event
		want   string
	}{
		{nil, ""},
		{[]Event{ev(EvSessionStarted, `{"model":"m"}`)}, ""},
		{[]Event{ev(EvSessionStarted, `{"provider":"a"}`)}, "a"},
		{[]Event{ev(EvSessionStarted, `{"provider":"a"}`), ev(EvModelSwitched, `{"provider":"b"}`)}, "b"},
		{[]Event{ev(EvModelSwitched, `{"provider":"b"}`), ev(EvModelSwitched, `{"provider":""}`)}, ""},
	} {
		if got := ProviderOf(c.events); got != c.want {
			t.Errorf("ProviderOf(%v) = %q, want %q", c.events, got, c.want)
		}
	}
}
