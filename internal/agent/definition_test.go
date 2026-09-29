package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// fakeMCP stands in for a tool an MCP server offers.
type fakeMCP struct{ name string }

func (f fakeMCP) Name() string          { return f.name }
func (fakeMCP) Description() string     { return "remote" }
func (fakeMCP) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (fakeMCP) Mutates() bool           { return false }
func (fakeMCP) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	return tools.Result{Content: "ok"}
}

// allEvents keeps every event appended, whichever session it belongs to.
type allEvents struct {
	*MemStore
	mu  sync.Mutex
	evs []Event
}

func (a *allEvents) Append(ev Event) error {
	a.mu.Lock()
	a.evs = append(a.evs, ev)
	a.mu.Unlock()
	return a.MemStore.Append(ev)
}

func (a *allEvents) all() []Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Event(nil), a.evs...)
}

// defFactory is a factory offering one loaded definition beside the built-ins.
func defFactory(t *testing.T, turns []scriptedTurn, def *Definition) (*SubagentFactory, *scriptedAdapter) {
	t.Helper()
	f := subFactory(t, turns, NewBudget(1_000_000, 10, false))
	f.Definitions = WithDefinitions(def)
	return f, f.Adapter.(*scriptedAdapter)
}

func toolNames(req model.Request) []string {
	var out []string
	for _, d := range req.Tools {
		out = append(out, d.Name)
	}
	return out
}

// A definition cannot give a child a tool its parent lacks: a name the
// session does not have refuses the spawn, where Subset would drop it.
func TestDefinitionToolsOnlyNarrow(t *testing.T) {
	f, _ := defFactory(t, []scriptedTurn{{text: "ok"}}, &Definition{Name: "writer", Description: "d", Instruction: "w",
		Tools: []string{"read", "bash"}})
	_, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "writer"})
	if err == nil || !strings.Contains(err.Error(), "names tools this session does not have: bash") {
		t.Fatalf("a definition widened the child's tools: %v", err)
	}
	if n := f.Budget.spawned.Load(); n != 0 {
		t.Fatalf("a refused spawn was counted: %d", n)
	}
}

// A typo in a tool list refuses the spawn and names the tool, rather than a
// role running quietly without it.
func TestDefinitionUnknownToolFailsClosed(t *testing.T) {
	f, _ := defFactory(t, nil, &Definition{Name: "reader", Description: "d", Instruction: "r",
		Tools: []string{"Read", "graep", "mcp__none__*"}})
	_, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "reader"})
	if err == nil || !strings.Contains(err.Error(), "graep, mcp__none__*") {
		t.Fatalf("an unknown tool did not refuse the spawn: %v", err)
	}
}

// A loaded role runs with its instructions and exactly the tools it names,
// case-folded, less its disallowed ones; the record names the definition
// and the tools it ran with.
func TestDefinitionRoleToolsAndRecord(t *testing.T) {
	def := &Definition{Name: "auditor", Description: "d", Instruction: "Audit with care.", Source: SourceOperator,
		SHA256: "abc", Tools: []string{"Read", "Grep", "Glob", "mcp__docs__*", "recall"}, DisallowedTools: []string{"glob"}}
	f, ad := defFactory(t, []scriptedTurn{{text: "found nothing"}}, def)
	f.Tools.Add(fakeMCP{"mcp__docs__search"})
	f.Tools.Add(fakeMCP{"mcp__other__x"})
	store := &allEvents{MemStore: NewMemStore()}
	f.Store = store
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "auditor"}); err != nil {
		t.Fatal(err)
	}
	got := toolNames(ad.gotRequests[0])
	if !reflect.DeepEqual(got, []string{"read", "grep", "mcp__docs__search", "recall"}) {
		t.Fatalf("the child's tools: %v", got)
	}
	if sys := ad.gotRequests[0].System; !strings.Contains(sys, "## Role\nAudit with care.") {
		t.Fatalf("the role instructions are not in the child's prompt")
	}
	var spawned map[string]any
	for _, e := range store.all() {
		if e.Type == EvSubagentSpawned {
			_ = json.Unmarshal(e.Payload, &spawned)
		}
	}
	want := map[string]any{"definition": "auditor", "definition_source": "operator", "definition_sha256": "abc",
		"tools": []any{"grep", "mcp__docs__search", "read"}}
	for k, v := range want {
		if !reflect.DeepEqual(spawned[k], v) {
			t.Fatalf("spawned %s = %v, want %v (%v)", k, spawned[k], v, spawned)
		}
	}
}

// A built-in role keeps its fixed tools; general keeps the parent's.
func TestBuiltinRolesUnchanged(t *testing.T) {
	f, ad := defFactory(t, []scriptedTurn{{text: "a"}, {text: "b"}}, &Definition{Name: "x", Description: "d", Instruction: "i"})
	for _, typ := range []string{"explore", "general"} {
		if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: typ}); err != nil {
			t.Fatal(err)
		}
	}
	if got := toolNames(ad.gotRequests[0]); !reflect.DeepEqual(got, []string{"read", "glob", "grep", "recall"}) {
		t.Fatalf("explore's tools: %v", got)
	}
	if got := toolNames(ad.gotRequests[1]); len(got) != 6 {
		t.Fatalf("general's tools: %v", got)
	}
}

// A spawn naming a type the session does not offer is refused, where it once
// ran as the main role.
func TestSpawnRefusesUnknownType(t *testing.T) {
	f := subFactory(t, nil, NewBudget(1_000_000, 10, false))
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "wizard"}); err == nil {
		t.Fatal("an unknown agent type ran")
	}
}

// A definition's permission mode narrows the child's, and never widens it;
// the parent's engine is untouched either way.
func TestDefinitionPermissionModeOnlyNarrows(t *testing.T) {
	for _, c := range []struct {
		parent, def, want policy.Mode
	}{
		{policy.ModeDefault, "plan", policy.ModePlan},
		{policy.ModeBypass, "default", policy.ModeDefault},
		{policy.ModePlan, "default", policy.ModePlan},
		{policy.ModeAcceptEdits, "", policy.ModeAcceptEdits},
	} {
		pol := policy.New(c.parent)
		if err := pol.AddDeny("bash(rm *)"); err != nil {
			t.Fatal(err)
		}
		got := narrowMode(pol, string(c.def))
		if got.Mode != c.want || pol.Mode != c.parent {
			t.Fatalf("%+v: child %s, parent now %s", c, got.Mode, pol.Mode)
		}
		if d := got.Evaluate("bash", true, json.RawMessage(`{"command":"rm x"}`)); d.Decision != policy.Deny {
			t.Fatalf("%+v: the deny rule did not come along", c)
		}
	}

	// End to end: a plan-mode role cannot write though its parent could.
	for _, c := range []struct {
		mode  string
		wrote bool
	}{{"", true}, {"plan", false}} {
		f, _ := defFactory(t, nil, &Definition{Name: "planner", Description: "d", Instruction: "p", PermissionMode: c.mode})
		target := filepath.Join(f.Workspace, "made.txt")
		f.Adapter = &scriptedAdapter{turns: []scriptedTurn{
			{calls: []model.ToolCall{call("write", map[string]string{"path": target, "content": "x"})}},
			{text: "finished"},
		}}
		if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", AgentType: "planner"}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(target); (err == nil) != c.wrote {
			t.Fatalf("mode %q: wrote %v, want %v", c.mode, err == nil, c.wrote)
		}
	}
}

// A definition's turn cap binds the call; nothing goes above the parent's.
func TestChildTurns(t *testing.T) {
	for _, c := range []struct{ parent, def, call, want int }{
		{100, 0, 0, 30},
		{20, 0, 0, 20},
		{100, 0, 50, 50},
		{100, 0, 200, 100},
		{100, 50, 0, 50},
		{100, 12, 20, 12},
		{10, 50, 0, 10},
		{100, 12, 5, 5},
	} {
		if got := childTurns(c.parent, c.def, c.call); got != c.want {
			t.Errorf("childTurns(%d, %d, %d) = %d, want %d", c.parent, c.def, c.call, got, c.want)
		}
	}
}

// The model is offered the session's types: the enum and the listing hold
// the loaded definitions, and task and tasks refuse a type not offered.
func TestAgentTypeEnumListsDefinitions(t *testing.T) {
	defs := WithDefinitions(&Definition{Name: "auditor", Description: "audits the ledger", Instruction: "i"})
	task, tasks := Task{Agents: defs}, Tasks{Agents: defs}
	var schema struct {
		Properties struct {
			AgentType struct {
				Enum []string `json:"enum"`
			} `json:"agent_type"`
			Tasks struct {
				Items struct {
					Properties struct {
						AgentType struct {
							Enum []string `json:"enum"`
						} `json:"agent_type"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"tasks"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(task.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	want := []string{"general", "explore", "test", "review", "auditor"}
	if !reflect.DeepEqual(schema.Properties.AgentType.Enum, want) {
		t.Fatalf("task's enum: %v", schema.Properties.AgentType.Enum)
	}
	if err := json.Unmarshal(tasks.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(schema.Properties.Tasks.Items.Properties.AgentType.Enum, want) {
		t.Fatalf("tasks' enum: %v", schema.Properties.Tasks.Items.Properties.AgentType.Enum)
	}
	if !strings.Contains(task.Description(), "- auditor — audits the ledger") {
		t.Fatalf("the description does not list the definition: %s", task.Description())
	}
	if string(task.Schema()) != string(Task{Agents: defs}.Schema()) {
		t.Fatal("the schema is not stable")
	}
}

// tasks refuses an agent type the session does not offer, before anything
// runs; it once ran such a task as the main role.
func TestTasksRejectsUnknownAgentType(t *testing.T) {
	spawned := 0
	tk := Tasks{Spawn: func(context.Context, SubagentRequest) (string, error) { spawned++; return "ok", nil }}
	res := tk.Run(context.Background(), nil, json.RawMessage(`{"tasks":[{"prompt":"a","description":"a"},{"prompt":"b","description":"b","agent_type":"wizard"}]}`))
	if !res.IsError || !strings.Contains(res.Content, `tasks[1].agent_type "wizard"`) || spawned != 0 {
		t.Fatalf("an unknown type in tasks: %+v, %d spawned", res, spawned)
	}
}

// A role that works in its own worktree asks as worktree isolation does, and
// runs in a checkout of its own through task as through tasks.
func TestWorktreeDefinition(t *testing.T) {
	ws := gitRepo(t)
	defs := WithDefinitions(&Definition{Name: "fixer", Description: "d", Instruction: "i", Isolation: "worktree"})
	var mu sync.Mutex
	var dirs []string
	spawn := func(_ context.Context, req SubagentRequest) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		dirs = append(dirs, req.Workspace)
		return "done", nil
	}
	task := Task{Spawn: spawn, Agents: defs, Workspace: ws}
	tasks := Tasks{Spawn: spawn, Agents: defs, Workspace: ws}
	fixer := json.RawMessage(`{"prompt":"p","description":"d","agent_type":"fixer"}`)
	if !task.MutatesCall(fixer) || task.MutatesCall(json.RawMessage(`{"prompt":"p","description":"d"}`)) {
		t.Fatal("task does not say a worktree role mutates")
	}
	if !tasks.MutatesCall(json.RawMessage(`{"tasks":[{"prompt":"p","description":"d","agent_type":"fixer"}]}`)) {
		t.Fatal("tasks does not say a worktree role mutates")
	}
	if res := task.Run(context.Background(), nil, fixer); res.IsError || !strings.Contains(res.Content, "no changes; removed") {
		t.Fatalf("task: %+v", res)
	}
	res := tasks.Run(context.Background(), nil, json.RawMessage(`{"tasks":[{"prompt":"p","description":"d","agent_type":"fixer"},{"prompt":"q","description":"e"}]}`))
	if res.IsError {
		t.Fatalf("tasks: %+v", res)
	}
	if len(dirs) != 3 || dirs[0] == "" || dirs[0] == ws {
		t.Fatalf("the worktree role did not run in a worktree: %v", dirs)
	}
	isolated := 0
	for _, d := range dirs[1:] {
		if d != "" {
			isolated++
		}
	}
	if isolated != 1 {
		t.Fatalf("tasks isolated %d children, want only the worktree role: %v", isolated, dirs)
	}
}
