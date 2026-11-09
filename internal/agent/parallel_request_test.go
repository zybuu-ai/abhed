package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// barrierAdapter is a model for a parent that starts subagents and for those
// subagents. The parent's first turn is calls; each child holds its answer
// until every child has asked, or a deadline passes. Children run one after
// another never meet, so the peak says whether they ran at the same time.
type barrierAdapter struct {
	calls    []model.ToolCall
	children int

	mu      sync.Mutex
	arrived int
	peak    int
	inside  int
	all     chan struct{}
	parent  int
}

func newBarrier(children int, calls []model.ToolCall) *barrierAdapter {
	return &barrierAdapter{calls: calls, children: children, all: make(chan struct{})}
}

func (b *barrierAdapter) Name() string                           { return "barrier" }
func (b *barrierAdapter) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (b *barrierAdapter) CountTokens(model.Request) (int, error) { return 0, nil }

func (b *barrierAdapter) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, len(b.calls)+4)
	child := false
	for _, m := range req.Messages {
		if m.Role == model.RoleUser && strings.Contains(m.Content, "child-") {
			child = true
		}
	}
	if child {
		b.mu.Lock()
		b.arrived++
		b.inside++
		b.peak = max(b.peak, b.inside)
		if b.arrived == b.children {
			close(b.all)
		}
		b.mu.Unlock()
		select {
		case <-b.all:
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
		b.mu.Lock()
		b.inside--
		b.mu.Unlock()
		ch <- model.Chunk{Type: model.ChunkText, Text: "child finished"}
	} else {
		b.mu.Lock()
		b.parent++
		first := b.parent == 1
		b.mu.Unlock()
		if first {
			for i := range b.calls {
				ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &b.calls[i]}
			}
		} else {
			ch <- model.Chunk{Type: model.ChunkText, Text: "all parts reported"}
		}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

func partCall(i int) model.ToolCall {
	args, _ := json.Marshal(map[string]string{"prompt": fmt.Sprintf("child-%d: do part %d", i, i), "description": fmt.Sprintf("part %d", i)})
	return model.ToolCall{ID: fmt.Sprintf("call-%d", i), Name: "task", Args: args}
}

// spawnsOverlap reports whether every subagent the parent's record shows
// starting started before any returned.
func spawnsOverlap(t *testing.T, store *MemStore, want int) {
	t.Helper()
	evs, _ := store.Events("parent")
	spawned, returnedFirst := 0, false
	for _, e := range evs {
		switch e.Type {
		case EvSubagentSpawned:
			if returnedFirst {
				t.Fatalf("a subagent started after another had returned: they ran one after another")
			}
			spawned++
		case EvSubagentReturn:
			returnedFirst = true
		}
	}
	if spawned != want {
		t.Fatalf("the record shows %d subagents started, want %d", spawned, want)
	}
}

// Several task calls in one response run at the same time: this is what the
// tool descriptions now tell the model a parallel request should become.
func TestSeveralTaskCallsInOneTurnRunConcurrently(t *testing.T) {
	adapter := newBarrier(3, []model.ToolCall{partCall(1), partCall(2), partCall(3)})
	store := NewMemStore()
	l, _, _ := taskTree(t, adapter, AutoApprove{Yes: true}, store, store, false)
	reason, err := l.Run(context.Background(), "run the three parts in parallel")
	if err != nil || reason != TermCompleted {
		t.Fatalf("run: %v %s", err, reason)
	}
	if adapter.peak != 3 {
		t.Fatalf("at most %d subagents were in flight at once, want 3", adapter.peak)
	}
	spawnsOverlap(t, store, 3)
}

// One tasks call with three entries runs them at the same time too.
func TestOneTasksCallRunsItsSubagentsConcurrently(t *testing.T) {
	list := []map[string]string{}
	for i := 1; i <= 3; i++ {
		list = append(list, map[string]string{"prompt": fmt.Sprintf("child-%d: do part %d", i, i)})
	}
	args, _ := json.Marshal(map[string]any{"tasks": list})
	adapter := newBarrier(3, []model.ToolCall{{ID: "call-tasks", Name: "tasks", Args: args}})
	store := NewMemStore()
	l, _, _ := taskTree(t, adapter, AutoApprove{Yes: true}, store, store, false)
	if _, err := l.Run(context.Background(), "use parallel subagents"); err != nil {
		t.Fatal(err)
	}
	if adapter.peak != 3 {
		t.Fatalf("at most %d subagents were in flight at once, want 3", adapter.peak)
	}
	spawnsOverlap(t, store, 3)
	// A task given no description is labelled from its prompt, so its card says what it is.
	evs, _ := store.Events("parent")
	for _, e := range evs {
		if e.Type == EvSubagentSpawned && !strings.Contains(string(e.Payload), `"description":"child-`) {
			t.Fatalf("a subagent started with no label: %s", e.Payload)
		}
	}
}

// The model is told what a request for parallel work means, on the tools and
// in the system prompt: start subagents, not make the code concurrent.
func TestParallelRequestsAreDescribedAsSubagents(t *testing.T) {
	task := Task{}.Description()
	for _, want := range []string{"Calls in ONE response run concurrently"} {
		if !strings.Contains(task, want) {
			t.Errorf("task's description lacks %q:\n%s", want, task)
		}
	}
	tasks := Tasks{}.Description()
	for _, want := range []string{"AT THE SAME TIME", "in parallel or by several agents means this, not concurrent code", "Set isolation to"} {
		if !strings.Contains(tasks, want) {
			t.Errorf("tasks' description lacks %q:\n%s", want, tasks)
		}
	}
	ws := tempDir(t)
	outside := Tasks{Workspace: ws}
	if d := outside.Description(); !strings.Contains(d, "Not a git repository") || strings.Contains(d, "Set isolation") {
		t.Errorf("outside git, tasks still offers worktree isolation:\n%s", d)
	}
	if strings.Contains(string(outside.Schema()), "worktree") {
		t.Errorf("outside git, the schema still offers worktree: %s", outside.Schema())
	}
	// A git init during the session offers it from then on.
	if err := os.Mkdir(filepath.Join(ws, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outside.Description(), "Set isolation") || !strings.Contains(string(outside.Schema()), "worktree") {
		t.Error("in a repository made since, tasks does not offer worktree isolation")
	}
}

// A role that works in its own worktree runs alone as a task call; the task
// tool says so only when the session offers such a role.
func TestTaskSaysWorktreeRolesRunAlone(t *testing.T) {
	if strings.Contains(Task{}.Description(), "runs alone") {
		t.Error("the built-in roles have no worktree, yet the note is there")
	}
	defs := WithDefinitions(&Definition{Name: "fixer", Description: "fixes", Isolation: "worktree"})
	if !strings.Contains(Task{Agents: defs}.Description(), "runs alone") {
		t.Error("a worktree role's task calls run one at a time, and the tool does not say so")
	}
}

// /parallel's turn names the tasks tool and carries the work.
func TestParallelPrompt(t *testing.T) {
	p := ParallelPrompt("  review pkg/auth, test pkg/store  ")
	if !strings.Contains(p, "ONE tasks call") || !strings.HasSuffix(p, "review pkg/auth, test pkg/store") {
		t.Fatalf("prompt:\n%s", p)
	}
}

var _ tools.Tool = Tasks{}

// The trigger: work asked for in parallel, or by several agents, and not a
// message about the program's own concurrency.
func TestAsksForParallelWork(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		// Work the person wants done at once.
		{"Speed up the project by running things in parallel.", true},
		{"review src, test pkg/store and document the server package in parallel", true},
		{"do these three at the same time", true},
		{"use several agents to audit the packages", true},
		{"spin up multiple subagents for this", true},
		{"run parallel subagents over each service", true},
		{"handle these concurrently", true},
		{"split this up and work on the parts simultaneously", true},
		{"parallelize the review of these four modules", true},
		{"fan out across the services and check each", true},
		{"do these in parallel please", true},
		{"use 3 agents to look at auth, store and server", true},
		{"run them side by side", true},
		{"work on all of them at once", true},
		{"यह काम समानांतर में करो", true},
		{"en paralelo, revisa los tres módulos", true},
		{"in-parallel review of pkg/a and pkg/b", true},
		{"The three parts all need work. Make it faster, do it in parallel.", true},
		// The program's own concurrency, or no request for parallel work.
		{"make the ingestion pipeline process files concurrently", false},
		{"parallelize the for loop in walk.go", false},
		{"run the test suite in parallel with -p 8", false},
		{"why does the parallel test fail?", false},
		{"add a worker pool so requests are handled at the same time", false},
		{"the server must handle multiple agents connecting simultaneously", false},
		{"fix the race when two goroutines write the map at the same time", false},
		{"explain how the scheduler runs jobs concurrently", false},
		{"document that the API supports multiple agents", false},
		{"use asyncio.gather so the HTTP calls run concurrently", false},
		{"rename ParallelRunner to Runner", false},
		{"wire the GPUs in parallel for training", false},
		{"is it safe to call this in parallel?", false},
		{"make this function thread-safe so it can run concurrently", false},
		{"go test -parallel 4 keeps timing out", false},
		{"add a mutex so both writers can run at once", false},
		{"summarise the notes", false},
	} {
		if got := asksForParallelWork(tc.text); got != tc.want {
			t.Errorf("%q: %v, want %v", tc.text, got, tc.want)
		}
	}
}

// noteLoop is a top-level loop on a scripted model, with tasks when withTasks.
func noteLoop(t *testing.T, withTasks bool, budget *Budget) (*Loop, *scriptedAdapter, *MemStore) {
	t.Helper()
	adapter := &scriptedAdapter{turns: []scriptedTurn{{text: "ok"}, {text: "ok"}}}
	reg := tools.NewRegistry(tools.Read{})
	if withTasks {
		reg.Add(Tasks{Spawn: func(context.Context, SubagentRequest) (string, error) { return "", nil }})
	}
	store := NewMemStore()
	sess, _ := tools.NewSession(tempDir(t))
	l := NewLoop(adapter, reg, nil, AutoApprove{Yes: true}, sess, NewRecorder(store, "s", ""), DefaultConfig())
	l.Budget = budget
	return l, adapter, store
}

// lastSent is the last user message the model was sent.
func lastSent(a *scriptedAdapter) string {
	sent := a.gotRequests[len(a.gotRequests)-1].Messages
	for i := len(sent) - 1; i >= 0; i-- {
		if sent[i].Role == model.RoleUser {
			return sent[i].Content
		}
	}
	return ""
}

// The note goes with the person's own words when they ask for parallel
// work, is recorded beside their text, and is sent again on replay. It is
// judged on what they typed, never on what they attached.
func TestParallelNoteGoesWithTheTypedRequest(t *testing.T) {
	ask := "review the three packages in parallel"
	attached := "summarise this file\n\nFiles the person attached: The workers run in parallel."
	for _, tc := range []struct {
		name  string
		msg   Message
		ctx   string
		tasks bool
		want  bool
	}{
		{"typed request", Message{Text: ask, Typed: ask}, "", true, true},
		{"typed through the context", Message{Text: ask}, ask, true, true},
		{"phrase only in an attachment", Message{Text: attached, Typed: "summarise this file"}, "", true, false},
		{"nobody's typed words", Message{Text: ask}, "", true, false},
		{"no tasks tool", Message{Text: ask, Typed: ask}, "", false, false},
		{"/parallel's own turn", Message{Text: ParallelPrompt("a, b"), Typed: ParallelPrompt("a, b")}, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, adapter, store := noteLoop(t, tc.tasks, nil)
			ctx := context.Background()
			if tc.ctx != "" {
				ctx = WithTyped(ctx, tc.ctx)
			}
			if _, err := l.RunMessage(ctx, tc.msg); err != nil {
				t.Fatal(err)
			}
			got := lastSent(adapter)
			if has := strings.HasSuffix(got, ParallelNote); has != tc.want {
				t.Fatalf("note sent %v, want %v: %q", has, tc.want, got)
			}
			evs, _ := store.Events("s")
			var m Message
			for _, e := range evs {
				if e.Type == EvUserMessage {
					_ = json.Unmarshal(e.Payload, &m)
				}
			}
			if m.Text != tc.msg.Text || (m.Note != "") != tc.want || m.ModelText() != got {
				t.Fatalf("recorded %+v, sent %q", m, got)
			}
			if msgs, err := Fork(evs, 0); err != nil || len(msgs) == 0 || msgs[0].Content != got {
				t.Fatalf("rebuilt %v %v, want %q first", msgs, err, got)
			}
		})
	}
}

// A message queued or steered while a run works is judged as one sent on
// its own.
func TestParallelNoteOnAQueuedMessage(t *testing.T) {
	l, adapter, _ := noteLoop(t, true, nil)
	ask := "and check the docs and the tests at the same time"
	l.QueueMessage(Message{Text: ask, Typed: ask, Steered: true})
	if _, err := l.Run(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range adapter.gotRequests {
		for _, m := range r.Messages {
			found = found || m.Content == ask+"\n\n"+ParallelNote
		}
	}
	if !found {
		t.Fatal("the queued request went without the note")
	}
}

// Only a top-level loop that can still start a subagent adds the note: a
// subagent's prompt is the model's writing, and a spent budget would refuse
// the call the note asks for.
func TestParallelNoteOnlyWhereASubagentCanStart(t *testing.T) {
	ask := "review the three packages in parallel"
	child, adapter, _ := noteLoop(t, true, nil)
	child.depth = 1
	if _, err := child.RunMessage(context.Background(), Message{Text: ask, Typed: ask}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(lastSent(adapter), ParallelNote) {
		t.Fatal("a subagent's prompt got the note")
	}
	spent := NewBudget(0, 1, false)
	if err := spent.TryReserveSubagent(); err != nil {
		t.Fatal(err)
	}
	full, adapter, _ := noteLoop(t, true, spent)
	if _, err := full.RunMessage(context.Background(), Message{Text: ask, Typed: ask}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(lastSent(adapter), ParallelNote) {
		t.Fatal("the note asked for a subagent the budget refuses")
	}
}

// A real subagent never gets the note, whatever its prompt says.
func TestSubagentPromptGetsNoNote(t *testing.T) {
	adapter := newBarrier(1, []model.ToolCall{{ID: "c1", Name: "task", Args: json.RawMessage(
		`{"prompt":"child-1: check these two files in parallel","description":"check"}`)}})
	store := NewMemStore()
	l, _, _ := taskTree(t, adapter, AutoApprove{Yes: true}, store, store, false)
	if _, err := l.RunMessage(context.Background(), Message{Text: "go", Typed: "go"}); err != nil {
		t.Fatal(err)
	}
	if len(store.Ends()) < 2 {
		t.Fatal("the subagent recorded nothing")
	}
	for id := range store.Ends() {
		evs, _ := store.Events(id)
		for _, e := range evs {
			if e.Type == EvUserMessage && strings.Contains(string(e.Payload), "harness_note") {
				t.Fatalf("%s: a user.message carries the note: %s", id, e.Payload)
			}
		}
	}
}

// The check behind tasks' description finds .git in the folder or above it.
func TestInGitRepo(t *testing.T) {
	d := tempDir(t)
	if InGitRepo(d) || InGitRepo("") {
		t.Fatal("a folder with no .git above it reads as a repository")
	}
	if err := os.MkdirAll(filepath.Join(d, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(d, "a", "b")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if !InGitRepo(d) || !InGitRepo(sub) {
		t.Fatal("a repository, or a folder in one, does not read as one")
	}
}

// waitingAdapter reports a wait for a request slot before it answers.
type waitingAdapter struct{ scriptedAdapter }

func (w *waitingAdapter) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	if tell := model.SlotWaitOf(ctx); tell != nil {
		tell(model.SlotWait{Ahead: 2}, false)
		time.Sleep(120 * time.Millisecond)
		tell(model.SlotWait{Ahead: 2, Waited: 120 * time.Millisecond}, true)
	}
	return w.scriptedAdapter.Complete(ctx, req)
}

// A wait for a request slot is recorded, and kept out of the call's timings.
func TestModelWaitIsRecordedAndLeftOutOfLatency(t *testing.T) {
	adapter := &waitingAdapter{scriptedAdapter{turns: []scriptedTurn{{text: "ok"}}}}
	store := NewMemStore()
	sess, _ := tools.NewSession(tempDir(t))
	l := NewLoop(adapter, tools.NewRegistry(tools.Read{}), nil, AutoApprove{Yes: true}, sess, NewRecorder(store, "s", ""), DefaultConfig())
	if _, err := l.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("s")
	var waited []ModelWaiting
	var call ModelCall
	for _, e := range evs {
		switch e.Type {
		case EvModelWaiting:
			var w ModelWaiting
			_ = json.Unmarshal(e.Payload, &w)
			waited = append(waited, w)
		case EvModelCall:
			_ = json.Unmarshal(e.Payload, &call)
		}
	}
	if len(waited) != 1 || waited[0].Ahead != 2 {
		t.Fatalf("model.waiting: %+v", waited)
	}
	if call.QueueMS < 120 || call.LatencyMS >= 120 {
		t.Fatalf("model.call queue %dms, latency %dms: the wait is not kept apart", call.QueueMS, call.LatencyMS)
	}
}
