package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// resumeModel plays a child: its first instruction gets "first answer", and a
// follow-up gets "second answer" when the earlier answer is in what it sees.
// With write set, the first instruction writes that file first.
type resumeModel struct {
	mu    sync.Mutex
	name  string
	write string
	seen  [][]model.Message
}

func (m *resumeModel) Name() string { return m.name }
func (m *resumeModel) Profile() model.Profile {
	return model.Profile{Name: m.name, ContextWindow: 100000}
}
func (m *resumeModel) CountTokens(req model.Request) (int, error) {
	n := len(req.System)
	for _, msg := range req.Messages {
		n += len(msg.Content)
	}
	return n / 4, nil
}
func (m *resumeModel) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	m.mu.Lock()
	m.seen = append(m.seen, append([]model.Message(nil), req.Messages...))
	m.mu.Unlock()
	ch := make(chan model.Chunk, 4)
	last := req.Messages[len(req.Messages)-1]
	all := ""
	for _, msg := range req.Messages {
		all += msg.Content + "\n"
	}
	switch {
	case last.Role == model.RoleUser && last.Content == "work" && m.write != "":
		// Written in the child's own working directory, as its prompt names it.
		wd := ""
		if _, rest, ok := strings.Cut(req.System, "Working directory: "); ok {
			wd, _, _ = strings.Cut(rest, "\n")
		}
		c := model.ToolCall{ID: "w1", Name: "write", Args: json.RawMessage(`{"path":"` + filepath.Join(wd, m.write) + `","content":"x"}`)}
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &c}
	case strings.Contains(all, "first answer") && last.Content == "more":
		ch <- model.Chunk{Type: model.ChunkText, Text: "second answer"}
	default:
		ch <- model.Chunk{Type: model.ChunkText, Text: "first answer"}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

type resumeRig struct {
	f     *SubagentFactory
	l     *Loop
	m     *resumeModel
	store *MemStore
	ws    string
}

func newResumeRig(t *testing.T, ws string) *resumeRig {
	t.Helper()
	if ws == "" {
		ws = tempDir(t)
	}
	sess, err := tools.NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	m := &resumeModel{name: "rm"}
	store := NewMemStore()
	reg := tools.NewRegistry(tools.Read{}, tools.Write{})
	f := &SubagentFactory{Adapter: m, Tools: reg, Policy: policy.New(policy.ModeAcceptEdits), Session: sess, Store: store,
		Budget: NewBudget(1_000_000, 10, false), Config: DefaultConfig(), Workspace: ws, Background: true}
	l := NewLoop(m, reg, policy.New(policy.ModeAcceptEdits), AutoApprove{}, sess, NewRecorder(store, "parent", ""), DefaultConfig())
	NewBackground(l, BackgroundPolicy{Wake: WakeNotify, MaxLive: 4})
	t.Cleanup(func() { l.Background.Close(TermSessionClosed) })
	return &resumeRig{f: f, l: l, m: m, store: store, ws: ws}
}

func (r *resumeRig) ctx() context.Context { return r.l.asParent(context.Background()) }

// spawn runs a first child and returns its task id.
func (r *resumeRig) spawn(t *testing.T, req SubagentRequest) string {
	t.Helper()
	if _, err := r.f.Spawn(r.ctx(), req); err != nil {
		t.Fatal(err)
	}
	var id string
	for _, s := range payloads[map[string]any](r.events(t), EvSubagentSpawned) {
		id = s["session"].(string)
	}
	return id
}

func (r *resumeRig) events(t *testing.T) []Event {
	t.Helper()
	evs, _ := r.store.Events("parent")
	return evs
}

// A resumed child goes on in its own conversation: it sees what it said
// before, and its record goes on in one sequence, linked in the parent's.
func TestResumeContinuesChildConversation(t *testing.T) {
	r := newResumeRig(t, "")
	id := r.spawn(t, SubagentRequest{Prompt: "work", Description: "d"})
	before, _ := r.store.Events(id)
	summary, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: id})
	if err != nil || summary != "second answer" {
		t.Fatalf("resume: %q %v", summary, err)
	}
	after, _ := r.store.Events(id)
	for i := 1; i < len(after); i++ {
		if after[i].Seq != after[i-1].Seq+1 {
			t.Fatalf("the child's record breaks its sequence at %d", i)
		}
	}
	if len(after) <= len(before) {
		t.Fatal("the resume was not recorded in the child's record")
	}
	var resumed map[string]any
	for _, s := range payloads[map[string]any](r.events(t), EvSubagentSpawned) {
		if s["resume"] == true {
			resumed = s
		}
	}
	if resumed == nil || resumed["session"] != id || int64(resumed["through_seq"].(float64)) != before[len(before)-1].Seq {
		t.Fatalf("the parent's record of the resume: %v", resumed)
	}
}

// Only the session that started a child may resume it; anything else, a
// grandchild or a store saying the child is another's, is "no such task".
func TestResumeOnlyByParent(t *testing.T) {
	r := newResumeRig(t, "")
	id := r.spawn(t, SubagentRequest{Prompt: "work", Description: "d"})
	other := NewLoop(r.m, r.l.Tools, r.l.Policy, AutoApprove{}, r.l.Session, NewRecorder(r.store, "other", ""), DefaultConfig())
	for _, c := range []struct {
		name string
		ctx  context.Context
		id   string
	}{
		{"another session", other.asParent(context.Background()), id},
		{"unknown id", r.ctx(), "NOSUCH"},
		{"not an id", r.ctx(), "../parent"},
	} {
		if _, err := r.f.Spawn(c.ctx, SubagentRequest{Prompt: "more", Resume: c.id}); err == nil || !strings.Contains(err.Error(), "no such task") {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	r.f.Store = notMine{r.store}
	if _, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: id}); err == nil || !strings.Contains(err.Error(), "no such task") {
		t.Fatalf("a store saying it is not ours: %v", err)
	}
}

// notMine is a store whose owner check refuses every child.
type notMine struct{ *MemStore }

func (notMine) SubSessionOf(context.Context, string, string) (bool, error) { return false, nil }

// A child still running is not resumed, in the background or mid-resume.
func TestResumeRunningChildRefused(t *testing.T) {
	r := newResumeRig(t, "")
	gate := make(chan struct{})
	defer close(gate)
	r.f.Adapter = gated{r.m, gate}
	id, err := r.f.SpawnBackground(r.ctx(), SubagentRequest{Prompt: "work", Description: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: id}); err == nil || !strings.Contains(err.Error(), "no such task") {
		t.Fatalf("a running child was resumed: %v", err)
	}
}

// gated holds every call until the gate closes.
type gated struct {
	*resumeModel
	gate chan struct{}
}

func (g gated) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	select {
	case <-g.gate:
	case <-ctx.Done():
	}
	return g.resumeModel.Complete(ctx, req)
}

// A worktree child resumes in its worktree, and is refused once the
// worktree is gone.
func TestResumeWorktreeChildInWorktree(t *testing.T) {
	ws := gitRepo(t)
	r := newResumeRig(t, ws)
	r.m.write = "made.txt"
	r.f.Definitions = WithDefinitions(&Definition{Name: "fixer", Description: "d", Instruction: "i", Isolation: "worktree"})
	task := Task{Spawn: r.f.Spawn, Agents: r.f.Definitions, Workspace: ws}
	res := task.Run(r.ctx(), nil, json.RawMessage(`{"prompt":"work","description":"d","agent_type":"fixer"}`))
	if res.IsError || !strings.Contains(res.Content, "UNCOMMITTED") {
		t.Fatalf("first run: %+v", res)
	}
	var id, dir string
	for _, s := range payloads[map[string]any](r.events(t), EvSubagentSpawned) {
		id, dir = s["session"].(string), s["workspace"].(string)
	}
	res = task.Run(r.ctx(), nil, json.RawMessage(`{"prompt":"more","resume":"`+id+`"}`))
	if res.IsError || !strings.Contains(res.Content, "second answer") || !strings.Contains(res.Content, "Worktree") {
		t.Fatalf("resume: %+v", res)
	}
	var resumedIn string
	for _, s := range payloads[map[string]any](r.events(t), EvSubagentSpawned) {
		if s["resume"] == true {
			resumedIn = s["workspace"].(string)
		}
	}
	if resumedIn != dir {
		t.Fatalf("resumed in %s, not its worktree %s", resumedIn, dir)
	}
	// Moved to another branch: not the checkout the task left.
	if _, err := git(context.Background(), hostgit.New(context.Background(), dir), "checkout", "-q", "-b", "elsewhere"); err != nil {
		t.Fatal(err)
	}
	if res := task.Run(r.ctx(), nil, json.RawMessage(`{"prompt":"more","resume":"`+id+`"}`)); !res.IsError || !strings.Contains(res.Content, "its worktree was removed") {
		t.Fatalf("resume on another branch: %+v", res)
	}
	if _, err := git(context.Background(), hostgit.New(context.Background(), dir), "checkout", "-q", "abhed/"+filepath.Base(dir)); err != nil {
		t.Fatal(err)
	}
	// The worktree goes: no resume, and never in the main tree instead.
	removeWorktree(context.Background(), ws, &worktree{Dir: dir, Branch: "abhed/" + filepath.Base(dir)})
	res = task.Run(r.ctx(), nil, json.RawMessage(`{"prompt":"more","resume":"`+id+`"}`))
	if !res.IsError || !strings.Contains(res.Content, "its worktree was removed") {
		t.Fatalf("resume without its worktree: %+v", res)
	}
}

// A child is resumed on the model it ran on, or not at all, and no spawn is
// counted when it cannot be.
func TestResumeRecordedModelUnavailableFails(t *testing.T) {
	r := newResumeRig(t, "")
	fast := &resumeModel{name: "fast-m"}
	r.f.Models, r.f.ModelNames = func(name string) (model.Adapter, error) {
		if name == "fast" {
			return fast, nil
		}
		return nil, os.ErrNotExist
	}, []string{"fast", "main"}
	id := r.spawn(t, SubagentRequest{Prompt: "work", Description: "d", Model: "fast"})
	if s, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: id}); err != nil || s != "second answer" || len(fast.seen) != 2 {
		t.Fatalf("resume on its model: %q %v, fast called %d", s, err, len(fast.seen))
	}
	if _, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: id, Model: "main"}); err == nil {
		t.Fatal("a resume moved the task to another model")
	}
	r.f.Models = func(string) (model.Adapter, error) { return nil, os.ErrNotExist }
	spent := r.f.Budget.spawned.Load()
	if _, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: id}); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("resume on a model gone: %v", err)
	}
	if r.f.Budget.spawned.Load() != spent || len(r.m.seen) != 0 {
		t.Fatal("a refused resume counted a spawn, or ran on the parent's model")
	}
}

// Each resume counts as one spawn.
func TestResumeCountsAgainstMaxSubagents(t *testing.T) {
	r := newResumeRig(t, "")
	r.f.Budget = NewBudget(1_000_000, 1, false)
	id := r.spawn(t, SubagentRequest{Prompt: "work", Description: "d"})
	if _, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: id}); err == nil || !strings.Contains(err.Error(), "subagent limit") {
		t.Fatalf("a resume past max_subagents: %v", err)
	}
}

// A resume takes the role as it is now: tools no wider than today's, the
// change recorded; a role no longer offered is refused.
func TestResumeUsesCurrentNarrowTools(t *testing.T) {
	r := newResumeRig(t, "")
	r.f.Definitions = WithDefinitions(&Definition{Name: "worker", Description: "d", Instruction: "i", SHA256: "v1"})
	id := r.spawn(t, SubagentRequest{Prompt: "work", Description: "d", AgentType: "worker"})
	r.f.Definitions = WithDefinitions(&Definition{Name: "worker", Description: "d", Instruction: "i", SHA256: "v2", Tools: []string{"read"}})
	if _, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: id}); err != nil {
		t.Fatal(err)
	}
	last := r.m.seen[len(r.m.seen)-1]
	_ = last
	var resumed map[string]any
	for _, s := range payloads[map[string]any](r.events(t), EvSubagentSpawned) {
		if s["resume"] == true {
			resumed = s
		}
	}
	if resumed["definition_changed"] != true || len(resumed["tools"].([]any)) != 1 { // read only
		t.Fatalf("resumed with %v", resumed)
	}
	r.f.Definitions = WithDefinitions()
	if _, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: id}); err == nil || !strings.Contains(err.Error(), "no longer offers") {
		t.Fatalf("a resume of a role gone: %v", err)
	}
	if _, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: id, AgentType: "explore"}); err == nil || !strings.Contains(err.Error(), "start a new task") {
		t.Fatalf("a resume as another role: %v", err)
	}
}

// A resume gets a fresh allowance of turns, counted on top of those spent.
func TestResumeTurnAllowance(t *testing.T) {
	r := newResumeRig(t, "")
	id := r.spawn(t, SubagentRequest{Prompt: "work", Description: "d", MaxTurns: 3})
	end, _ := LastEnd(childEvents(t, r.store, id))
	c, err := r.f.prepare(r.ctx(), SubagentRequest{Prompt: "more", Resume: id, MaxTurns: 2}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.release()
	if c.sub.Config.MaxTurns != end.Turns+2 || c.sub.turns != end.Turns {
		t.Fatalf("cap %d, turns %d, after %d spent", c.sub.Config.MaxTurns, c.sub.turns, end.Turns)
	}
}

func childEvents(t *testing.T, s *MemStore, id string) []Event {
	t.Helper()
	evs, err := s.Events(id)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// A conversation too large for the model's window is not resumed.
func TestResumeOversizedContextRefused(t *testing.T) {
	r := newResumeRig(t, "")
	id := r.spawn(t, SubagentRequest{Prompt: "work", Description: "d"})
	r.m.name = "rm" // same model
	r.f.Adapter = small{r.m}
	r.l.Adapter = small{r.m}
	if _, err := r.f.Spawn(r.l.asParent(context.Background()), SubagentRequest{Prompt: "more", Resume: id}); err == nil || !strings.Contains(err.Error(), "fills") {
		t.Fatalf("an oversized resume: %v", err)
	}
}

// small is the same model with a window too small for the rebuilt conversation.
type small struct{ *resumeModel }

func (s small) Profile() model.Profile { return model.Profile{Name: s.name, ContextWindow: 100} }

// CountTokens says the conversation fills 85% of the window: past the 80%
// a resume needs to leave free, short of the window itself.
func (small) CountTokens(model.Request) (int, error) { return 85, nil }

// A resume may run in the background, as the same task: its id, its record.
func TestResumeInBackground(t *testing.T) {
	r := newResumeRig(t, "")
	id := r.spawn(t, SubagentRequest{Prompt: "work", Description: "d"})
	got, err := r.f.SpawnBackground(r.ctx(), SubagentRequest{Prompt: "more", Resume: id})
	if err != nil || got != id {
		t.Fatalf("background resume: %q %v", got, err)
	}
	waitFor(t, "the resumed task to end", func() bool {
		ti, ok := r.l.Background.Task(id)
		return ok && ti.Status == "completed"
	})
	if ti, _ := r.l.Background.Task(id); !strings.Contains(ti.Summary, "second answer") {
		t.Fatalf("the resumed task: %+v", ti)
	}
	if len(r.l.Background.Tasks()) != 1 {
		t.Fatal("the resumed task is listed twice")
	}
}

// A task already being resumed, in the background, is not resumed again
// until that run ends.
func TestResumeWhileResumedRefused(t *testing.T) {
	r := newResumeRig(t, "")
	id := r.spawn(t, SubagentRequest{Prompt: "work", Description: "d"})
	gate := make(chan struct{})
	r.f.Adapter = gated{r.m, gate}
	r.l.Adapter = gated{r.m, gate}
	if _, err := r.f.SpawnBackground(r.l.asParent(context.Background()), SubagentRequest{Prompt: "more", Resume: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.f.Spawn(r.l.asParent(context.Background()), SubagentRequest{Prompt: "again", Resume: id}); err == nil || !strings.Contains(err.Error(), "no such task") {
		t.Fatalf("a task being resumed was resumed again: %v", err)
	}
	close(gate)
	waitFor(t, "the resume to end", func() bool { ti, _ := r.l.Background.Task(id); return ti.Status == "completed" })
	if _, err := r.f.Spawn(r.l.asParent(context.Background()), SubagentRequest{Prompt: "more", Resume: id}); err != nil {
		t.Fatalf("once ended it may be resumed again: %v", err)
	}
}

// recordOldChild writes a finished child's record as an older build did: its
// spawn names a workspace and no branch.
func recordOldChild(t *testing.T, r *resumeRig, id, workspace string) {
	t.Helper()
	rec := NewRecorder(r.store, id, "parent")
	spawned := map[string]any{"description": "old", "agent_type": "general", "definition": "general", "depth": 0,
		"workspace": workspace, "session": id, "model": "rm"}
	_, _ = rec.Record(EvSubagentSpawned, ActorAgent, Trusted, spawned)
	_, _ = rec.Record(EvUserMessage, ActorUser, Trusted, Message{Text: "work"})
	_, _ = rec.Record(EvAgentMessage, ActorAgent, Trusted, Message{Text: "first answer"})
	_, _ = rec.Record(EvSessionEnded, ActorSystem, Trusted, SessionEnded{Reason: TermCompleted, Turns: 1})
	prec := NewRecorder(r.store, "parent", "")
	prec.Advance(lastSeq(r.events(t)))
	_, _ = prec.Record(EvSubagentSpawned, ActorAgent, Trusted, spawned)
	_, _ = prec.Record(EvSubagentReturn, ActorSystem, Trusted, map[string]any{"session": id, "task_id": id, "reason": "completed"})
}

func lastSeq(evs []Event) int64 {
	if len(evs) == 0 {
		return 0
	}
	return evs[len(evs)-1].Seq
}

// A child recorded in another directory with no branch to verify is refused,
// never resumed in the main tree; one recorded in the workspace itself,
// under any path that resolves to it, goes on there.
func TestResumeNeverFallsBackToTheMainTree(t *testing.T) {
	r := newResumeRig(t, "")
	recordOldChild(t, r, "OLDCHILD1", tempDir(t))
	_, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: "OLDCHILD1"})
	if err == nil || !strings.Contains(err.Error(), "its worktree was removed") {
		t.Fatalf("a branchless child from elsewhere: %v", err)
	}
	link := filepath.Join(tempDir(t), "ws-link")
	if err := os.Symlink(r.ws, link); err != nil {
		t.Fatal(err)
	}
	recordOldChild(t, r, "OLDCHILD2", link)
	if _, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: "OLDCHILD2"}); err != nil {
		t.Fatalf("a child recorded in the workspace, by another path to it: %v", err)
	}
}
