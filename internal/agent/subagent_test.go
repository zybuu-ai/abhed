package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

func subFactory(t *testing.T, turns []scriptedTurn, budget *Budget) *SubagentFactory {
	t.Helper()
	dir := tempDir(t)
	sess, err := tools.NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &SubagentFactory{
		Adapter:   &scriptedAdapter{turns: turns},
		Tools:     tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Edit{}, tools.Glob{}, tools.Grep{}),
		Policy:    policy.New(policy.ModeDefault),
		Approver:  AutoApprove{Yes: true},
		Session:   sess,
		Store:     NewMemStore(),
		Budget:    budget,
		Config:    DefaultConfig(),
		Workspace: dir,
	}
}

// The core value of delegation: the parent gets a summary, not a transcript.
func TestSubagentReturnsOnlySummary(t *testing.T) {
	f := subFactory(t, []scriptedTurn{
		{calls: []model.ToolCall{call("glob", map[string]string{"pattern": "*.go"})}},
		{calls: []model.ToolCall{call("glob", map[string]string{"pattern": "*.md"})}},
		{text: "Found 3 Go files and 2 markdown files under pkg/."},
	}, NewBudget(1_000_000, 10, false))

	summary, err := f.Spawn(context.Background(), SubagentRequest{
		Prompt: "find the files", Description: "find files", AgentType: "explore",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "Found 3 Go files") {
		t.Fatalf("summary missing: %q", summary)
	}
	// Intermediate tool traffic must not leak into what the parent sees.
	if strings.Contains(summary, "glob") {
		t.Fatalf("subagent transcript leaked to the parent: %q", summary)
	}
}

func TestSubagentBudgetExhaustionIsClean(t *testing.T) {
	b := NewBudget(100, 10, false)
	b.Spend(200) // already over

	f := subFactory(t, []scriptedTurn{{text: "done"}}, b)
	_, err := f.Spawn(context.Background(), SubagentRequest{
		Prompt: "x", Description: "y",
	})
	if err == nil {
		t.Fatal("spawn must fail once the budget is exhausted")
	}
	if !strings.Contains(err.Error(), "budget limit reached") {
		t.Fatalf("error should name the cause: %v", err)
	}
	// And it must tell the model what to do instead of retrying.
	if !strings.Contains(err.Error(), "context you have") {
		t.Fatalf("error should guide recovery: %v", err)
	}
}

func TestSubagentCountLimit(t *testing.T) {
	b := NewBudget(1_000_000, 2, false)
	f := subFactory(t, []scriptedTurn{{text: "ok"}}, b)

	for i := 0; i < 2; i++ {
		if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y"}); err != nil {
			t.Fatalf("spawn %d should succeed: %v", i, err)
		}
	}
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y"}); err == nil {
		t.Fatal("third spawn must be refused")
	}
}

func TestNestedSpawningDisabledByDefault(t *testing.T) {
	f := subFactory(t, []scriptedTurn{{text: "ok"}}, NewBudget(1_000_000, 10, false))
	f.Depth = 1 // we are already inside a subagent

	_, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y"})
	if err == nil {
		t.Fatal("nested spawn must be refused by default")
	}
	if !strings.Contains(err.Error(), "nested") {
		t.Fatalf("error should name the reason: %v", err)
	}
}

func TestSubagentSpendCountsAgainstParentBudget(t *testing.T) {
	b := NewBudget(1_000_000, 10, false)
	f := subFactory(t, []scriptedTurn{
		{calls: []model.ToolCall{call("glob", map[string]string{"pattern": "*"})}},
		{text: "done"},
	}, b)

	before := b.Spent()
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y"}); err != nil {
		t.Fatal(err)
	}
	if b.Spent() <= before {
		t.Fatal("subagent spend must count against the parent budget")
	}
}

// A read-only profile must not be able to write, or the orchestrator loses
// track of what changed.
func TestExploreProfileCannotWrite(t *testing.T) {
	f := subFactory(t, []scriptedTurn{{text: "ok"}}, NewBudget(1_000_000, 10, false))

	sub := f.Tools.Subset(Profiles["explore"].Tools...)
	for _, banned := range []string{"write", "edit", "bash"} {
		if _, found := sub.Get(banned); found {
			t.Errorf("explore profile must not expose %q", banned)
		}
	}
	for _, needed := range []string{"read", "glob", "grep"} {
		if _, found := sub.Get(needed); !found {
			t.Errorf("explore profile needs %q", needed)
		}
	}
}

func TestTaskToolRejectsEmptyPrompt(t *testing.T) {
	tool := Task{
		Spawn:    func(context.Context, SubagentRequest) (string, error) { return "", nil },
		Profiles: Profiles,
	}
	args, _ := json.Marshal(taskArgs{Description: "something"})
	res := tool.Run(context.Background(), nil, args)
	if !res.IsError || !strings.Contains(res.Content, "self-contained") {
		t.Fatalf("empty prompt must be refused with guidance: %s", res.Content)
	}
}

func TestTaskToolRejectsUnknownAgentType(t *testing.T) {
	tool := Task{
		Spawn:    func(context.Context, SubagentRequest) (string, error) { return "ok", nil },
		Profiles: Profiles,
	}
	args, _ := json.Marshal(taskArgs{Prompt: "do it", Description: "x", AgentType: "wizard"})
	res := tool.Run(context.Background(), nil, args)
	if !res.IsError || !strings.Contains(res.Content, "Available") {
		t.Fatalf("unknown type should list valid ones: %s", res.Content)
	}
}

func TestSubagentSummaryIsBounded(t *testing.T) {
	huge := strings.Repeat("x", MaxSummaryChars+5000)
	f := subFactory(t, []scriptedTurn{{text: huge}}, NewBudget(1_000_000, 10, false))

	summary, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary) > MaxSummaryChars+100 {
		t.Fatalf("summary must be bounded, got %d chars", len(summary))
	}
	if !strings.Contains(summary, "truncated") {
		t.Fatal("truncation must be visible to the parent")
	}
}

func TestSubagentEarlyTerminationIsReported(t *testing.T) {
	var turns []scriptedTurn
	for i := 0; i < 10; i++ {
		turns = append(turns, scriptedTurn{calls: []model.ToolCall{
			call("glob", map[string]string{"pattern": "*"}),
		}})
	}
	f := subFactory(t, turns, NewBudget(1_000_000, 10, false))
	f.Config.MaxTurns = 2

	summary, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "loop", Description: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "ended early") {
		t.Fatalf("parent must be told the subagent did not finish: %q", summary)
	}
}

// A subagent in its own worktree keeps the parent's syntax mode: a parent that
// turned the check off must not get a child that refuses.
func TestWorktreeSubagentKeepsTheSyntaxMode(t *testing.T) {
	child := tempDir(t)
	p := filepath.Join(child, "cfg.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := subFactory(t, []scriptedTurn{
		{calls: []model.ToolCall{call("read", map[string]string{"path": p})}},
		{calls: []model.ToolCall{call("write", map[string]string{"path": p, "content": "{"})}},
		{text: "done"},
	}, NewBudget(1_000_000, 10, false))
	f.Session.Syntax = tools.SyntaxOff
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", Workspace: child}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "{" {
		t.Fatalf("the child refused a write its parent allows: %q", got)
	}
}

// askingApprover stands in for the person at the prompt and says no.
type askingApprover struct {
	mu    sync.Mutex
	asked []string
}

func (a *askingApprover) Approve(ctx context.Context, tool string, args json.RawMessage, res policy.Result) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, tool+" "+policy.Subject(tool, args))
	NoteAnswer(ctx, Answer{By: ByReviewer})
	return false, nil
}

// parentWithTask builds a parent loop whose task tool spawns through a factory
// that, as the CLI's once did, would approve every ask on its own.
func parentWithTask(t *testing.T, turns []scriptedTurn, appr Approver) (*Loop, *MemStore, string) {
	t.Helper()
	store := NewMemStore()
	l, dir, _ := taskTree(t, &scriptedAdapter{turns: turns}, appr, store, store, false)
	return l, store, dir
}

// taskTree is parentWithTask with its parts chosen: the adapter, the stores the
// parent and the subagents write to, and whether subagents may nest.
func taskTree(t *testing.T, adapter model.Adapter, appr Approver, parentStore, childStore Store, nested bool) (*Loop, string, *SubagentFactory) {
	t.Helper()
	dir := tempDir(t)
	sess, err := tools.NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	pol := policy.New(policy.ModeDefault)
	if err := pol.AddAsk("bash(touch *)"); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(tools.Read{}, tools.Bash{})
	f := &SubagentFactory{
		Adapter: adapter, Tools: reg, Policy: pol, Approver: AutoApprove{Yes: true},
		Session: sess, Store: childStore, Budget: NewBudget(1_000_000, 10, nested),
		Config: DefaultConfig(), Workspace: dir,
	}
	reg.Add(Task{Spawn: f.Spawn, Profiles: Profiles})
	reg.Add(Tasks{Spawn: f.Spawn, Profiles: Profiles, Workspace: dir})
	return NewLoop(adapter, reg, pol, appr, sess, NewRecorder(parentStore, "parent", ""), DefaultConfig()), dir, f
}

func subagentTurns() []scriptedTurn {
	return []scriptedTurn{
		{calls: []model.ToolCall{call("task", map[string]string{"prompt": "clean up", "description": "clean up"})}},
		{calls: []model.ToolCall{call("bash", map[string]string{"command": "rm -rf keep"})}},
		{calls: []model.ToolCall{{ID: "c2", Name: "bash", Args: json.RawMessage(`{"command":"touch made.txt"}`)}}},
		{text: "could not"},
		{text: "done"},
	}
}

// In -p nobody can be asked, so a subagent's destructive command and its
// ask-rule command are refused as headless, and the parent's record says so.
func TestSubagentHeadlessRefusesItsAsks(t *testing.T) {
	l, store, dir := parentWithTask(t, subagentTurns(), AutoApprove{Yes: false})
	if err := os.Mkdir(filepath.Join(dir, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep")); err != nil {
		t.Fatalf("a subagent's rm -rf ran with nobody asked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "made.txt")); err == nil {
		t.Fatal("a subagent's ask-rule command ran with nobody asked")
	}

	evs, _ := store.Events("parent")
	var child string
	steps := map[string]string{}
	for _, e := range evs {
		switch e.Type {
		case EvSubagentSpawned:
			var p map[string]any
			_ = json.Unmarshal(e.Payload, &p)
			child, _ = p["session"].(string)
		case EvSubagentAction:
			var a SubagentAction
			_ = json.Unmarshal(e.Payload, &a)
			if a.Decision != "denied" || a.By != ByHeadless || a.Session == "" {
				t.Fatalf("subagent action not refused as headless: %+v", a)
			}
			steps[a.Subject] = a.Step
		}
	}
	if child == "" {
		t.Fatalf("the parent's record does not name the subagent's session: %s", types(evs))
	}
	if steps["rm -rf keep"] != "destructive" || steps["touch made.txt"] != "ask" {
		t.Fatalf("the parent's record misses the subagent's refusals: %v", steps)
	}
	childEvs, _ := store.Events(child)
	if len(childEvs) == 0 || childEvs[0].ParentID != "parent" {
		t.Fatalf("the subagent's record is not linked to its parent: %+v", childEvs)
	}
}

// Interactively, a subagent's asks go to the person at the parent's prompt.
func TestSubagentAsksTheParentsApprover(t *testing.T) {
	appr := &askingApprover{}
	l, store, dir := parentWithTask(t, subagentTurns(), appr)
	if err := os.Mkdir(filepath.Join(dir, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(appr.asked, "; "); got != "bash rm -rf keep; bash touch made.txt" {
		t.Fatalf("the parent's approver was not asked for the subagent's calls: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep")); err != nil {
		t.Fatalf("a refused rm -rf ran: %v", err)
	}
	evs, _ := store.Events("parent")
	n := 0
	for _, e := range evs {
		if e.Type == EvSubagentAction {
			var a SubagentAction
			_ = json.Unmarshal(e.Payload, &a)
			if a.By != ByReviewer || e.Actor != ActorUser {
				t.Fatalf("the person's refusal is not in the parent's record: %+v", a)
			}
			n++
		}
	}
	if n != 2 {
		t.Fatalf("want 2 subagent actions in the parent's record, got %d: %s", n, types(evs))
	}
}

func taskCall(id, prompt string) model.ToolCall {
	return model.ToolCall{ID: id, Name: "task", Args: json.RawMessage(`{"prompt":"` + prompt + `","description":"` + prompt + `"}`)}
}

func bashCall(id, command string) model.ToolCall {
	return model.ToolCall{ID: id, Name: "bash", Args: json.RawMessage(`{"command":"` + command + `"}`)}
}

func payloads[T any](evs []Event, typ EventType) []T {
	var out []T
	for _, e := range evs {
		if e.Type == typ {
			var v T
			_ = json.Unmarshal(e.Payload, &v)
			out = append(out, v)
		}
	}
	return out
}

type spawnPayload struct {
	Description string `json:"description"`
	Session     string `json:"session"`
	Depth       int    `json:"depth"`
	Reason      string `json:"reason"`
}

// With nesting off, a subagent cannot spawn one of its own.
func TestNestedSubagentRefusedWhenNestingIsOff(t *testing.T) {
	appr := &askingApprover{}
	store := NewMemStore()
	l, _, f := taskTree(t, &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{taskCall("t1", "outer")}},
		{calls: []model.ToolCall{taskCall("t2", "inner")}},
		{text: "could not delegate"},
		{text: "done"},
	}}, appr, store, store, false)
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if n := f.Budget.spawned.Load(); n != 1 {
		t.Fatalf("a subagent spawned another with nesting off: %d spawned", n)
	}
	evs, _ := store.Events("parent")
	if n := len(payloads[spawnPayload](evs, EvSubagentSpawned)); n != 1 {
		t.Fatalf("want one subagent in the root record, got %d", n)
	}
}

// With nesting on, a grandchild's ask reaches the person without the shared
// queue being taken twice, and its events reach the root record.
func TestNestedSubagentAsksAndReachesTheRootRecord(t *testing.T) {
	appr := &askingApprover{}
	store := NewMemStore()
	l, _, _ := taskTree(t, &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{taskCall("t1", "outer")}},
		{calls: []model.ToolCall{taskCall("t2", "inner")}},
		{calls: []model.ToolCall{bashCall("b1", "rm -rf keep")}},
	}}, appr, store, store, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := l.Run(ctx, "go"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(appr.asked, "; "); got != "bash rm -rf keep" {
		t.Fatalf("the grandchild's ask did not reach the person: %q", got)
	}
	evs, _ := store.Events("parent")
	spawned := payloads[spawnPayload](evs, EvSubagentSpawned)
	returned := payloads[spawnPayload](evs, EvSubagentReturn)
	if len(spawned) != 2 || len(returned) != 2 {
		t.Fatalf("the root record misses the nested subagent: %s", types(evs))
	}
	var inner string
	for _, s := range spawned {
		if s.Depth == 1 {
			inner = s.Session
		}
	}
	acts := payloads[SubagentAction](evs, EvSubagentAction)
	if inner == "" || len(acts) != 1 || acts[0].Session != inner || acts[0].Decision != "denied" {
		t.Fatalf("the grandchild's refusal is not in the root record under its session %q: %+v", inner, acts)
	}
}

// routeAdapter answers by what the request is: the root fans out, a subagent
// runs one command, and anything after a tool result says done.
type routeAdapter struct {
	fanout string
}

func (routeAdapter) Name() string                           { return "route" }
func (routeAdapter) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (routeAdapter) CountTokens(model.Request) (int, error) { return 0, nil }

func (r routeAdapter) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 4)
	last := req.Messages[len(req.Messages)-1]
	var tc *model.ToolCall
	switch {
	case last.Role == model.RoleTool:
	case last.Content == "go":
		tc = &model.ToolCall{ID: "ts", Name: "tasks", Args: json.RawMessage(r.fanout)}
	case last.Content == "nest":
		c := taskCall("t", "deep")
		tc = &c
	default:
		c := bashCall("b", "touch "+last.Content+".txt")
		tc = &c
	}
	if tc == nil {
		ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	} else {
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: tc}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

// countingApprover measures how many asks are inside it at once.
type countingApprover struct {
	in, peak, asked atomic.Int32
}

func (c *countingApprover) Approve(context.Context, string, json.RawMessage, policy.Result) (bool, error) {
	n := c.in.Add(1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	c.in.Add(-1)
	c.asked.Add(1)
	return false, nil
}

// Subagents running together, one of them a level deeper, ask one at a time.
func TestParallelSubagentsAskOneAtATime(t *testing.T) {
	appr := &countingApprover{}
	store := NewMemStore()
	adapter := routeAdapter{fanout: `{"tasks":[{"prompt":"a","description":"a"},{"prompt":"b","description":"b"},{"prompt":"nest","description":"nest"}]}`}
	l, _, _ := taskTree(t, adapter, appr, store, store, true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := l.Run(ctx, "go"); err != nil {
		t.Fatal(err)
	}
	if appr.asked.Load() != 3 || appr.peak.Load() != 1 {
		t.Fatalf("want 3 asks one at a time, got %d asks and %d at once", appr.asked.Load(), appr.peak.Load())
	}
}

// grantingApprover allows and remembers the scope offered.
type grantingApprover struct{ granted string }

func (g *grantingApprover) Approve(ctx context.Context, _ string, _ json.RawMessage, res policy.Result) (bool, error) {
	g.granted = res.Offer()
	NoteAnswer(ctx, Answer{By: ByReviewer, Granted: g.granted})
	return true, nil
}

// An ask the person allowed is copied with the scope they chose; a call the
// policy allowed on its own is not; the return names the session.
func TestSubagentRecordCopiesAllowedAsksOnly(t *testing.T) {
	appr := &grantingApprover{}
	l, store, dir := parentWithTask(t, []scriptedTurn{
		{calls: []model.ToolCall{taskCall("t1", "work")}},
		{calls: []model.ToolCall{call("read", map[string]string{"path": "notes.txt"})}},
		{calls: []model.ToolCall{bashCall("b1", "mkdir out")}},
		{text: "made it"},
		{text: "done"},
	}, appr)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("parent")
	acts := payloads[SubagentAction](evs, EvSubagentAction)
	if len(acts) != 1 || acts[0].Tool != "bash" || acts[0].Decision != "allowed" ||
		acts[0].By != ByReviewer || appr.granted == "" || acts[0].GrantedScope != appr.granted {
		t.Fatalf("want only the allowed ask, with its granted scope %q: %+v", appr.granted, acts)
	}
	spawned := payloads[spawnPayload](evs, EvSubagentSpawned)
	returned := payloads[spawnPayload](evs, EvSubagentReturn)
	if len(spawned) != 1 || len(returned) != 1 || returned[0].Session == "" || returned[0].Session != spawned[0].Session {
		t.Fatalf("the parent's return does not name the subagent's session: %+v %+v", spawned, returned)
	}
}

// failingStore refuses every write but the parent's.
type failingStore struct{ *MemStore }

func (s failingStore) Append(ev Event) error {
	if ev.SessionID != "parent" {
		return errors.New("store unavailable")
	}
	return s.MemStore.Append(ev)
}

// A subagent that fails still has its return, with its session, in the parent's record.
func TestFailedSubagentReturnIsRecorded(t *testing.T) {
	store := NewMemStore()
	l, _, _ := taskTree(t, &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{taskCall("t1", "work")}},
		{calls: []model.ToolCall{call("read", map[string]string{"path": "x"})}},
	}}, AutoApprove{Yes: false}, store, failingStore{store}, false)
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("parent")
	spawned := payloads[spawnPayload](evs, EvSubagentSpawned)
	returned := payloads[spawnPayload](evs, EvSubagentReturn)
	if len(spawned) != 1 || len(returned) != 1 || returned[0].Reason != string(TermError) || returned[0].Session != spawned[0].Session {
		t.Fatalf("the failed subagent's return is not in the parent's record: %+v %+v", spawned, returned)
	}
}

// With no loop to answer to and no approver, a subagent's asks are refused.
func TestSubagentWithoutApproverRefuses(t *testing.T) {
	f := subFactory(t, []scriptedTurn{
		{calls: []model.ToolCall{bashCall("b1", "rm -rf keep")}},
		{text: "done"},
	}, NewBudget(1_000_000, 10, false))
	f.Approver = nil
	f.Tools = tools.NewRegistry(tools.Bash{})
	if err := os.Mkdir(filepath.Join(f.Workspace, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.Workspace, "keep")); err != nil {
		t.Fatalf("a subagent with no approver ran rm -rf: %v", err)
	}
}

// An ask still waiting its turn when the run is interrupted is not asked.
func TestQueuedAskGivesUpOnCancel(t *testing.T) {
	inner := &countingApprover{}
	o := oneAtATime{Approver: inner, asks: make(chan struct{}, 1)}
	o.asks <- struct{}{} // another subagent is being asked
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := o.Approve(ctx, "bash", nil, policy.Result{})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil || inner.asked.Load() != 0 {
			t.Fatalf("a cancelled ask went ahead: err %v, asked %d", err, inner.asked.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a queued ask did not give up when its run was interrupted")
	}
}

// A tasks list sent to task names the tasks tool, so the model can correct the call.
func TestTaskNamesTheTasksToolForATasksList(t *testing.T) {
	raw := json.RawMessage(`{"tasks":[{"prompt":"read the README","description":"read readme"}]}`)
	res := Task{}.Run(context.Background(), nil, raw)
	if !res.IsError || !strings.Contains(res.Content, "tasks tool") {
		t.Fatalf("got %q, want an error naming the tasks tool", res.Content)
	}
	res = Task{}.Run(context.Background(), nil, json.RawMessage(`{"description":"x"}`))
	if !res.IsError || strings.Contains(res.Content, "tasks tool") {
		t.Fatalf("a call with no prompt and no tasks got %q", res.Content)
	}
}

// A child redacts as its parent's session does, whether its factory has no
// redactor or one read before the value was stored.
func TestSubagentRedactsAsItsParentDoes(t *testing.T) {
	for _, stale := range []bool{false, true} {
		subagentRedactsAsParent(t, stale)
	}
}

func subagentRedactsAsParent(t *testing.T, stale bool) {
	t.Helper()
	const raw = "fake-subagent-value-5d1c"
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("FAKE_TOKEN", raw); err != nil {
		t.Fatal(err)
	}
	adapter := &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{call("task", map[string]string{"prompt": "read it", "description": "read"})}},
		{calls: []model.ToolCall{call("read", map[string]string{"path": "creds.txt"})}},
		{text: "the file holds " + raw},
		{text: "done"},
	}}
	store := NewMemStore()
	l, dir, f := taskTree(t, adapter, AutoApprove{Yes: true}, store, store, false)
	if f.Redact != nil {
		t.Fatal("the factory under test must have no redactor of its own")
	}
	l.Recorder.Redact = vault.Redactor()
	if stale {
		// Built before the value was stored, as a long-lived factory is.
		f.Redact = secrets.Open(filepath.Join(t.TempDir(), "empty.json")).Redactor()
	}
	if err := os.WriteFile(filepath.Join(dir, "creds.txt"), []byte("token="+raw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	parent, _ := store.Events("parent")
	child := ""
	for _, e := range parent {
		all.Write(e.Payload)
		if e.Type == EvSubagentSpawned {
			var p map[string]any
			_ = json.Unmarshal(e.Payload, &p)
			child, _ = p["session"].(string)
		}
	}
	if child == "" {
		t.Fatalf("no subagent ran: %s", types(parent))
	}
	childEvs, _ := store.Events(child)
	for _, e := range childEvs {
		all.Write(e.Payload)
	}
	for _, req := range adapter.gotRequests {
		for _, m := range req.Messages {
			all.WriteString(m.Content)
		}
	}
	if strings.Contains(all.String(), raw) {
		t.Fatalf("the stored value reached a subagent's record or model:\n%s", all.String())
	}
	if !strings.Contains(all.String(), "[secret:FAKE_TOKEN]") {
		t.Fatalf("the subagent's output was not redacted by name:\n%s", all.String())
	}
}

// scopedProbe reports what its session keeps under scopedProbeKey.
type scopedProbe struct{ saw *any }

type scopedProbeKey struct{}

func (scopedProbe) Name() string            { return "probe" }
func (scopedProbe) Description() string     { return "probe" }
func (scopedProbe) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (scopedProbe) Mutates() bool           { return false }
func (p scopedProbe) Run(_ context.Context, s *tools.Session, _ json.RawMessage) tools.Result {
	*p.saw = s.Scoped(scopedProbeKey{}, nil)
	return tools.Result{Content: "ok"}
}

// A subagent in its own worktree is still the parent's conversation: a login
// the parent made is the child's too, and not a fresh, empty session's.
func TestWorktreeSubagentInheritsScopedState(t *testing.T) {
	var saw any
	f := subFactory(t, []scriptedTurn{
		{calls: []model.ToolCall{call("probe", map[string]string{})}},
		{text: "done"},
	}, NewBudget(1_000_000, 10, false))
	f.Tools.Add(scopedProbe{saw: &saw})
	f.Session.Scoped(scopedProbeKey{}, func() any { return "parent's login" })
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", Workspace: tempDir(t)}); err != nil {
		t.Fatal(err)
	}
	if saw != "parent's login" {
		t.Fatalf("the worktree subagent saw %v, not its parent's login", saw)
	}
}
