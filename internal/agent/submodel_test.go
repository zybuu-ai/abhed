package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
)

// subModel is a scripted model with a name, as model.call records it.
type subModel struct {
	*scriptedAdapter
	name string
}

func (n subModel) Profile() model.Profile {
	return model.Profile{Name: n.name, ContextWindow: 100000}
}

// modelChoice is a resolver offering the named adapters, recording each ask.
type modelChoice struct {
	adapters map[string]model.Adapter
	asked    []string
}

func (m *modelChoice) resolve(name string) (model.Adapter, error) {
	m.asked = append(m.asked, name)
	if a, ok := m.adapters[name]; ok {
		return a, nil
	}
	return nil, errors.New("it is not a configured provider; available: fast")
}

func modelFactory(t *testing.T, budget *Budget) (*SubagentFactory, *subModel, *subModel, *modelChoice) {
	t.Helper()
	f := subFactory(t, nil, budget)
	parent := &subModel{&scriptedAdapter{}, "parent-m"}
	fast := &subModel{&scriptedAdapter{turns: []scriptedTurn{{text: "fast answer"}}}, "fast-m"}
	f.Adapter = parent
	choice := &modelChoice{adapters: map[string]model.Adapter{"fast": fast}}
	f.Models, f.ModelNames = choice.resolve, []string{"fast", "main"}
	f.Store = &allEvents{MemStore: NewMemStore()}
	return f, parent, fast, choice
}

func spawnedPayload(t *testing.T, f *SubagentFactory, typ EventType) map[string]any {
	t.Helper()
	var out map[string]any
	for _, e := range f.Store.(*allEvents).all() {
		if e.Type == typ {
			_ = json.Unmarshal(e.Payload, &out)
		}
	}
	return out
}

// A child asked to run on a configured model runs on it: its model calls name
// that model, the parent's is never called, and the record names both.
func TestSubagentRunsOnNamedModel(t *testing.T) {
	f, parent, fast, _ := modelFactory(t, NewBudget(1_000_000, 10, false))
	summary, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", Model: "fast"})
	if err != nil || summary != "fast answer" {
		t.Fatalf("spawn: %q %v", summary, err)
	}
	if len(fast.gotRequests) != 1 || len(parent.gotRequests) != 0 {
		t.Fatalf("calls: fast %d, parent %d", len(fast.gotRequests), len(parent.gotRequests))
	}
	var called string
	for _, e := range f.Store.(*allEvents).all() {
		if e.Type == EvModelCall {
			var c ModelCall
			_ = json.Unmarshal(e.Payload, &c)
			called = c.Model
		}
	}
	if called != "fast-m" {
		t.Fatalf("model.call names %q", called)
	}
	for _, typ := range []EventType{EvSubagentSpawned, EvSubagentReturn} {
		p := spawnedPayload(t, f, typ)
		if p["provider"] != "fast" || p["model"] != "fast-m" {
			t.Fatalf("%s: %v", typ, p)
		}
	}
	if got := SubagentProvider(f.Store.(*allEvents).all()); got != "fast" {
		t.Fatalf("SubagentProvider = %q", got)
	}
}

// A model that cannot be had refuses the spawn: nothing runs, no spawn is
// counted, and the parent's model is not used in its place.
func TestUnknownModelFailsWithoutFallback(t *testing.T) {
	f, parent, _, _ := modelFactory(t, NewBudget(1_000_000, 10, false))
	_, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", Model: "gone"})
	if err == nil || !strings.Contains(err.Error(), `model "gone" is not available`) || !strings.Contains(err.Error(), "available: fast") {
		t.Fatalf("an unknown model: %v", err)
	}
	if n := f.Budget.spawned.Load(); n != 0 || len(parent.gotRequests) != 0 {
		t.Fatalf("a refused model counted %d spawns and made %d parent calls", n, len(parent.gotRequests))
	}

	// No choice offered: a named model is refused, never run on the parent's.
	f.Models = nil
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", Model: "fast"}); err == nil ||
		!strings.Contains(err.Error(), "offers no model choice") {
		t.Fatalf("a named model with no choice offered: %v", err)
	}
	if len(parent.gotRequests) != 0 {
		t.Fatal("the parent's model ran in place of the one named")
	}
}

// A model value is a provider name, never an endpoint: a URL or a path is
// refused before any lookup.
func TestModelNameIsNotAURL(t *testing.T) {
	f, _, _, choice := modelFactory(t, NewBudget(1_000_000, 10, false))
	for _, v := range []string{"http://evil.example/v1", "evil.example/v1", "a b", "x\nforged"} {
		if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", Model: v}); err == nil {
			t.Fatalf("model %q ran", v)
		}
	}
	if len(choice.asked) != 0 {
		t.Fatalf("the resolver was asked for %v", choice.asked)
	}
}

// The call's model wins over the definition's; inherit, or nothing, takes the
// definition's, then the parent's.
func TestSubagentModelPrecedence(t *testing.T) {
	for _, c := range []struct{ call, def, want string }{
		{"fast", "slow", "fast"},
		{"", "slow", "slow"},
		{"inherit", "slow", "slow"},
		{"inherit", "", ""},
		{"", "inherit", ""},
	} {
		if got := childModel(c.call, c.def); got != c.want {
			t.Errorf("childModel(%q, %q) = %q, want %q", c.call, c.def, got, c.want)
		}
	}
	f, parent, fast, _ := modelFactory(t, NewBudget(1_000_000, 10, false))
	f.Definitions = WithDefinitions(&Definition{Name: "quick", Description: "d", Instruction: "i", Model: "fast"})
	parent.turns = []scriptedTurn{{text: "parent answer"}}
	if s, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "quick"}); err != nil || s != "fast answer" {
		t.Fatalf("the definition's model: %q %v", s, err)
	}
	if s, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y"}); err != nil || s != "parent answer" {
		t.Fatalf("no model named: %q %v", s, err)
	}
	if len(fast.gotRequests) != 1 || len(parent.gotRequests) != 1 {
		t.Fatalf("calls: fast %d, parent %d", len(fast.gotRequests), len(parent.gotRequests))
	}
}

// Children on different models spend from one token budget.
func TestBudgetSharedAcrossModels(t *testing.T) {
	b := NewBudget(150, 10, false)
	f, parent, fast, _ := modelFactory(t, b)
	parent.turns = []scriptedTurn{{text: "a"}}
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", Model: "fast"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y"}); err != nil {
		t.Fatal(err)
	}
	if b.Spent() != 200 || len(fast.gotRequests) != 1 || len(parent.gotRequests) != 1 {
		t.Fatalf("spent %d across fast %d and parent %d calls", b.Spent(), len(fast.gotRequests), len(parent.gotRequests))
	}
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", Model: "fast"}); err == nil ||
		!strings.Contains(err.Error(), "budget limit reached") {
		t.Fatalf("a third child after both models spent the budget: %v", err)
	}
}

// The model property is offered only when there is a choice to make.
func TestModelEnumOnlyWithAChoice(t *testing.T) {
	for _, c := range []struct {
		models []string
		want   bool
	}{{nil, false}, {[]string{"one"}, false}, {[]string{"a", "b"}, true}} {
		for _, schema := range []json.RawMessage{Task{Models: c.models}.Schema(), Tasks{Models: c.models}.Schema()} {
			if got := strings.Contains(string(schema), `"model"`); got != c.want {
				t.Fatalf("models %v: model offered = %v in %s", c.models, got, schema)
			}
		}
	}
	if s := string(Task{Models: []string{"a", "b"}}.Schema()); !strings.Contains(s, `"enum":["a","b"]`) {
		t.Fatalf("the enum: %s", s)
	}
}

// task and tasks pass the model the call names to the spawn.
func TestTaskToolsPassTheModel(t *testing.T) {
	var got []string
	spawn := func(_ context.Context, req SubagentRequest) (string, error) {
		got = append(got, req.Model)
		return "ok", nil
	}
	Task{Spawn: spawn}.Run(context.Background(), nil, json.RawMessage(`{"prompt":"p","description":"d","model":"fast"}`))
	Tasks{Spawn: spawn, MaxParallel: 1}.Run(context.Background(), nil, json.RawMessage(`{"tasks":[{"prompt":"p","description":"d","model":"slow"}]}`))
	if strings.Join(got, ",") != "fast,slow" {
		t.Fatalf("models passed: %v", got)
	}
}

// A child on its parent's model records the parent's provider, so a later
// resume knows which configured model it ran on.
func TestInheritedModelRecordsParentsProvider(t *testing.T) {
	store := NewMemStore()
	l, _, _ := taskTree(t, &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{taskCall("t1", "look")}},
		{text: "looked"},
		{text: "done"},
	}}, AutoApprove{}, store, store, false)
	l.Provider = "main-p"
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("parent")
	spawned := payloads[map[string]any](evs, EvSubagentSpawned)
	if len(spawned) != 1 || spawned[0]["provider"] != "main-p" {
		t.Fatalf("spawned: %v", spawned)
	}
	child, _ := store.Events(spawned[0]["session"].(string))
	if got := SubagentProvider(child); got != "main-p" {
		t.Fatalf("the child's record names provider %q", got)
	}
}

// A managed role's model binds: a call naming another is refused, and the
// same model or none runs it there.
func TestManagedDefinitionModelBinds(t *testing.T) {
	f, parent, fast, _ := modelFactory(t, NewBudget(1_000_000, 10, false))
	f.Definitions = WithDefinitions(&Definition{Name: "onprem", Description: "d", Instruction: "i", Model: "fast", Source: SourceManaged})
	parent.turns = []scriptedTurn{{text: "parent answer"}}
	fast.turns = []scriptedTurn{{text: "a"}, {text: "b"}, {text: "c"}}
	f.Models = func(name string) (model.Adapter, error) {
		if name == "fast" {
			return fast, nil
		}
		return parent, nil
	}
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "onprem", Model: "main"}); err == nil ||
		!strings.Contains(err.Error(), "set by the organisation") {
		t.Fatalf("a call moved a managed role to another model: %v", err)
	}
	for _, m := range []string{"", "inherit", "fast"} {
		if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "onprem", Model: m}); err != nil {
			t.Fatalf("model %q: %v", m, err)
		}
	}
	if len(parent.gotRequests) != 0 || len(fast.gotRequests) != 3 {
		t.Fatalf("calls: parent %d, fast %d", len(parent.gotRequests), len(fast.gotRequests))
	}
}
