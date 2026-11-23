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

// parallelCase is one phrasing and the note it should get.
type parallelCase struct {
	text string
	want noteTier
}

const (
	none    = noNote
	clarify = clarifyNote
	strong  = strongNote
)

func checkParallelCases(t *testing.T, cases []parallelCase) {
	t.Helper()
	names := map[noteTier]string{noNote: "none", clarifyNote: "clarify", strongNote: "strong"}
	for _, tc := range cases {
		if got := parallelTier(tc.text); got != tc.want {
			t.Errorf("%q: %s, want %s", tc.text, names[got], names[tc.want])
		}
	}
}

// The phrasings of the first review, which the trigger was first written
// against.
func TestParallelTier(t *testing.T) {
	checkParallelCases(t, []parallelCase{
		// Reading or analysis work asked for at once: the strong note.
		{"review src, test pkg/store and document the server package in parallel", strong},
		// It must open the message, so these only ask.
		{"parallelize the review of these four modules", clarify},
		{"fan out across the services and check each", clarify},
		{"in-parallel review of pkg/a and pkg/b", clarify},
		// Parallel work with nothing said of what kind: ask.
		{"Speed up the project by running things in parallel.", clarify},
		{"do these three at the same time", clarify},
		{"spin up multiple subagents for this", clarify},
		{"run parallel subagents over each service", clarify},
		{"split this up and work on the parts simultaneously", clarify},
		{"do these in parallel please", clarify},
		{"run them side by side", clarify},
		{"work on all of them at once", clarify},
		{"यह काम समानांतर में करो", clarify},
		{"en paralelo, revisa los tres módulos", clarify},
		// Missed on purpose: a count of agents, "concurrently", or a
		// code-change verb leaves the note out; /parallel asks outright.
		{"use several agents to audit the packages", none},
		{"use 3 agents to look at auth, store and server", none},
		{"handle these concurrently", none},
		{"The three parts all need work. Make it faster, do it in parallel.", none},
		// The program's own concurrency, or no request for parallel work.
		{"make the ingestion pipeline process files concurrently", none},
		{"parallelize the for loop in walk.go", none},
		{"run the test suite in parallel with -p 8", none},
		{"why does the parallel test fail?", none},
		{"add a worker pool so requests are handled at the same time", none},
		{"the server must handle multiple agents connecting simultaneously", none},
		{"fix the race when two goroutines write the map at the same time", none},
		{"explain how the scheduler runs jobs concurrently", none},
		{"document that the API supports multiple agents", none},
		{"use asyncio.gather so the HTTP calls run concurrently", none},
		{"rename ParallelRunner to Runner", none},
		{"wire the GPUs in parallel for training", none},
		{"is it safe to call this in parallel?", none},
		{"make this function thread-safe so it can run concurrently", none},
		{"go test -parallel 4 keeps timing out", none},
		{"add a mutex so both writers can run at once", none},
		{"summarise the notes", none},
	})
}

// Held out from the second review: phrasings the trigger was not written
// against when they were added.
func TestParallelTierHeldOutSecondReview(t *testing.T) {
	checkParallelCases(t, []parallelCase{
		{"run the test suite in parallel", none},
		{"make the downloads happen at once instead of one by one", none},
		{"build a LangGraph app with multiple agents that talk to each other", none},
		{"deploy two agents to the staging cluster", none},
		{"start 4 agents in docker compose for the load test", none},
		{"make the indexer process documents concurrently", none},
		{"the crawler should fetch pages simultaneously", none},
		{"parallelise the image resize in resize.py", none},
		{"add Promise.all so the fetches run at the same time", none},
		{"use errgroup so the uploads run concurrently", none},
		{"make each shard parallel", none},
		{"review the ingestion pipeline and the retriever in parallel", strong},
		{"check every request handler in parallel", strong},
		// "code" suppresses both notes: precision over recall.
		{"audit the locking code and the auth code at the same time", none},
		{"look at the training and eval scripts in parallel", strong},
		{"review pkg/auth, pkg/store and pkg/server in parallel", strong},
		{"audit these three repos at the same time", strong},
		{"summarise the five design docs side by side", strong},
		{"fan out over the services and report what each one does", clarify},
		{"spin up subagents to read each module", clarify},
		// "CI" now suppresses both notes.
		{"investigate the three failing CI jobs simultaneously", none},
		{"implement a worker that processes jobs in parallel", none},
		{"refactor the scheduler so jobs run at once", none},
		{"write a script that downloads the files at once", none},
		{"use rayon to sort the chunks in parallel", none},
	})
}

// Held out from the third review: the eight false notes now get none, or
// only the note that asks where nothing names code; agent-building requests
// get none.
func TestParallelTierHeldOutThirdReview(t *testing.T) {
	checkParallelCases(t, []parallelCase{
		{"let the CI jobs run in parallel", none},
		{"have the ETL steps happen at once rather than serially", none},
		{"turn on parallel builds in the Makefile", none},
		{"switch the uploader to a subagent-style worker that fans out requests", none},
		{"create a supervisor with three subagents in our agent framework", none},
		{"design a multi-agent system where subagents run side by side", none},
		// Nothing names code, so it may only ask.
		{"the importer needs to parse the CSV files simultaneously", clarify},
		{"spawn the shards side by side with multiprocessing", none},
		{"rewrite the batch job so records are processed in parallel", none},
		{"enable parallel tool calls for the OpenAI client", none},
		// "code" suppresses both notes: precision over recall.
		{"split the code review of auth, store and server across subagents", none},
		{"investigate the three failing tests at the same time", strong},
		{"summarise these five docs in parallel", strong},
		{"compare the two designs side by side and give me a verdict", strong},
		{"fan out and read every README under docs/", strong},
		// Agent-building requests get no note.
		{"build three subagents for the support bot", none},
		{"create subagents that run side by side in our framework", none},
		{"design how our subagents hand off work", none},
		// Subagents named as the doer of reading work.
		{"use subagents to review pkg/auth and pkg/store", strong},
		// It must open the message, so this only asks.
		{"split the audit across subagents", clarify},
	})
}

// Held out from the fourth review: a parallel phrase that describes how a
// program runs gets the note that asks, or none, never the strong one.
func TestParallelTierHeldOutFourthReview(t *testing.T) {
	checkParallelCases(t, []parallelCase{
		{"check that the workers really run in parallel", clarify},
		{"find out why the jobs no longer run at the same time", clarify},
		{"read the files in parallel instead of one after another", clarify},
		{"review whether the uploads can safely happen at once", clarify},
		{"test the API with 50 requests at the same time", clarify},
		{"look at how the orchestrator dispatches subagents and simplify it", clarify},
		{"analyse why our subagents deadlock when they run side by side", clarify},
		{"update the orchestrator to launch subagents in parallel", clarify},
		{"have the retriever query both indexes in parallel", clarify},
		{"our subagents keep timing out, investigate", clarify},
		{"search the logs for when both cron jobs fired at once", clarify},
		// The veto matches by stem.
		{"do the three refactors at the same time", none},
		{"keep refactoring these in parallel", none},
		{"rewriting the three modules at once", none},
		{"creating the reports side by side", none},
		{"designing both screens at the same time", none},
		{"enabled the fetches in parallel", none},
		{"switching both services at once", none},
		{"spawning the workers in parallel", none},
		// Work the person wants done at once.
		{"handle the three tickets at the same time", clarify},
		{"use two subagents: one reads the RFC, one reads the spec", strong},
		{"with subagents, research pricing for the three vendors", strong},
	})
}

// Held out from the fifth review: a program to be changed so that it reads
// gets the note that asks, or none, never the strong one; reading work that
// opens the message keeps it.
func TestParallelTierHeldOutFifthReview(t *testing.T) {
	checkParallelCases(t, []parallelCase{
		{"update the crawler to read pages in parallel", clarify},
		{"change the loader so it reads all shards at once", clarify},
		{"fix the indexer so it searches both indexes in parallel", clarify},
		{"convert the scanner to analyse files side by side", clarify},
		{"set up the GitHub Action to test each package in parallel", none},
		{"configure pytest-xdist to test modules in parallel", none},
		{"port the summariser to summarise chapters in parallel", clarify},
		{"let the agent framework review PRs with subagents", clarify},
		{"modify the reviewer bot to check every file at once", clarify},
		{"wire the linter into the pre-commit hook so it checks files in parallel", none},
		{"give our support agent subagents that search the KB in parallel", clarify},
		{"tune the Jenkins pipeline to test the services simultaneously", none},
		{"upgrade the exporter to compare snapshots side by side", clarify},
		{"review the three open PRs in parallel and leave comments", strong},
		{"research the three libraries at the same time and recommend one", strong},
		// The opening may be polite, and subagents named first are the doers.
		{"please review these four modules in parallel", strong},
		{"could you summarise the three reports at the same time", strong},
		{"use subagents to review pkg/auth and pkg/store", strong},
		// A reading verb that does not open the message only asks.
		{"the three reports need someone to read them at once", clarify},
	})
}

// Held out from the sixth review: a reading verb that opens a compound
// request no longer carries a code change behind it to the strong note.
func TestParallelTierHeldOutSixthReview(t *testing.T) {
	checkParallelCases(t, []parallelCase{
		{"can you analyse and speed up the importer by reading files in parallel", clarify},
		{"review the loader and parallelize its reads", clarify},
		{"check the crawler and then have it read pages in parallel", clarify},
		{"look at making the crawler fetch pages in parallel", clarify},
		{"please check and update the crawler to read pages in parallel", clarify},
		{"use subagents to update the crawler so it reads pages in parallel", clarify},
		{"investigate the slow importer and get it reading the CSVs side by side", clarify},
		{"read the RFC and change our client to search both regions at once", clarify},
		{"with subagents, set up the agent framework to triage tickets in parallel", clarify},
		{"explore how to let the retriever search both indexes simultaneously", clarify},
	})
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
	ask := "check the docs and the tests at the same time"
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

// A mention of parallel work that names no reading or analysis work gets
// the note that asks, recorded as the strong one is.
func TestClarifyNoteGoesWithAnUnclearRequest(t *testing.T) {
	l, adapter, store := noteLoop(t, true, nil)
	ask := "Speed up the project by running things in parallel."
	if _, err := l.RunMessage(context.Background(), Message{Text: ask, Typed: ask}); err != nil {
		t.Fatal(err)
	}
	if got := lastSent(adapter); got != ask+"\n\n"+ClarifyNote {
		t.Fatalf("sent %q", got)
	}
	evs, _ := store.Events("s")
	for _, e := range evs {
		if e.Type == EvUserMessage && !strings.Contains(string(e.Payload), "ask one short question") {
			t.Fatalf("the record lacks the note: %s", e.Payload)
		}
	}
}

// A run nobody can answer gets no note asking a question: a vague request
// gets the note that holds it to a plan or the question, with no code
// edits; the strong note still goes, and a message with no parallel words
// gets none.
func TestUnattendedRunGetsNoClarifyNote(t *testing.T) {
	for _, tc := range []struct {
		ask  string
		want string
	}{
		{"do these three at the same time", UnattendedClarifyNote},
		{"Speed up the project by running things in parallel", UnattendedClarifyNote},
		{"review the three packages in parallel", ParallelNote},
		{"fix the failing test in pkg/auth", ""},
		// Ordinary work that mentions subagents or "at once" is done, not
		// turned into a plan.
		{"Fix the subagent timeout handling in the loop", ""},
		{"Delete these three files at once", ""},
		{"Update the copyright year in all files at once", ""},
		{"Bump the version in all three manifests at once", ""},
		{"Fix the bug where two tabs opened at the same time crash the editor", ""},
		// Subagents named with no speed or parallel word: nothing to clarify.
		{"use subagents for this", ""},
		// Concrete work whose verb no list names: the object makes it concrete.
		{"Run the three migrations in parallel", ""},
		{"Merge these three branches at once", ""},
		{"Close the three tickets at once", ""},
		{"Ship the three releases at once", ""},
		{"Send the three emails at the same time", ""},
		{"Answer the three questions at once", ""},
		{"Run all three checks at once and report results", ""},
		{"Merge these branches at once", ""},
		{"Run 3 migrations in parallel", ""},
		{"Restart each of the services at the same time", ""},
		{"Make the parser faster in parallel", ""},
		// Near misses: only generic objects, or a count with no noun, are
		// still vague.
		{"speed up the project by running things in parallel.", UnattendedClarifyNote},
		{"Run things in parallel", UnattendedClarifyNote},
		{"Run everything at once", UnattendedClarifyNote},
		{"Run all of them at the same time", UnattendedClarifyNote},
		{"Speed up the whole project by running all of it in parallel", UnattendedClarifyNote},
		{"do all three at once", UnattendedClarifyNote},
		// "make" alone does not make a vague request concrete.
		{"Make it faster by doing things in parallel", UnattendedClarifyNote},
		{"Can you make everything run in parallel?", UnattendedClarifyNote},
		// ...but "make" with a sign of the program's own concurrency is code.
		{"Make the workers use goroutines in parallel", ""},
		{"Make the tests faster", ""},
	} {
		l, adapter, _ := noteLoop(t, true, nil)
		l.Unattended = true
		if _, err := l.RunMessage(context.Background(), Message{Text: tc.ask, Typed: tc.ask}); err != nil {
			t.Fatal(err)
		}
		want := tc.ask
		if tc.want != "" {
			want += "\n\n" + tc.want
		}
		if got := lastSent(adapter); got != want {
			t.Errorf("%q: sent %q", tc.ask, got)
		}
	}
}
