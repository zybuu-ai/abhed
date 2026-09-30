package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// bgModel plays a parent that starts background children and a set of
// children that each wait for their gate. It answers by what it is sent.
type bgModel struct {
	mu     sync.Mutex
	gates  map[string]chan struct{} // a child's prompt → its gate
	starts int                      // background starts the parent asks for on "go"
	calls  atomic.Int32
	// parentAnswers counts the parent's plain answers.
	parentAnswers atomic.Int32
	saw           []string // task_status results the parent saw
	// workOnNotice makes the parent answer a result with more work.
	workOnNotice bool
	// inCall counts children that have reached their model call.
	inCall int
	// hold, when set, keeps a "slow" or "fail" turn until closed; a "fail"
	// turn then fails its model call.
	hold chan struct{}
	// noticeHold, when set, keeps the parent's answer to a result until closed.
	noticeHold chan struct{}
}

// childrenInCall is how many children have reached their model call.
func (m *bgModel) childrenInCall() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inCall
}

func newBGModel(children ...string) *bgModel {
	m := &bgModel{gates: map[string]chan struct{}{}, starts: len(children)}
	for _, c := range children {
		m.gates[c] = make(chan struct{})
	}
	return m
}

func (m *bgModel) release(child string) { close(m.gates[child]) }

func (*bgModel) Name() string                           { return "bg" }
func (*bgModel) Profile() model.Profile                 { return model.Profile{Name: "bg-m", ContextWindow: 100000} }
func (*bgModel) CountTokens(model.Request) (int, error) { return 0, nil }

func (m *bgModel) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	m.calls.Add(1)
	ch := make(chan model.Chunk, 16)
	first := req.Messages[0].Content
	last := req.Messages[len(req.Messages)-1]
	m.mu.Lock()
	gate, isChild := m.gates[first]
	m.mu.Unlock()
	switch {
	case isChild && first == "asker" && last.Role == model.RoleUser:
		c := model.ToolCall{ID: "ask1", Name: "touchy", Args: json.RawMessage(`{}`)}
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &c}
	case isChild:
		m.mu.Lock()
		m.inCall++
		m.mu.Unlock()
		select {
		case <-gate:
			ch <- model.Chunk{Type: model.ChunkText, Text: "result of " + first + " sk-live-abcdefghijklmnop"}
		case <-ctx.Done():
			ch <- model.Chunk{Type: model.ChunkText, Text: "stopped"}
		}
	case last.Role == model.RoleUser && last.Content == "go":
		i := 0
		for name := range m.gates {
			c := model.ToolCall{ID: "s" + name, Name: "bg", Args: json.RawMessage(`{"prompt":"` + name + `"}`)}
			ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &c}
			i++
		}
	case last.Role == model.RoleTool && strings.HasPrefix(last.ToolCallID, "bgn_"):
		if m.noticeHold != nil {
			<-m.noticeHold
		}
		m.mu.Lock()
		m.saw = append(m.saw, last.Content)
		more := m.workOnNotice
		m.mu.Unlock()
		if more {
			c := model.ToolCall{ID: "r" + newID(), Name: "read", Args: json.RawMessage(`{"path":"nothing.txt"}`)}
			ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &c}
		} else {
			ch <- model.Chunk{Type: model.ChunkText, Text: "noted"}
		}
	case last.Role == model.RoleUser && last.Content == "work":
		c := model.ToolCall{ID: "w" + newID(), Name: "read", Args: json.RawMessage(`{"path":"nothing.txt"}`)}
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &c}
	case last.Role == model.RoleUser && last.Content == "fail":
		<-m.hold
		return nil, errors.New("the provider is down")
	default:
		if last.Role == model.RoleUser && last.Content == "slow" {
			if m.hold != nil {
				<-m.hold
			} else {
				time.Sleep(200 * time.Millisecond) // a turn long enough for a result to land in it
			}
		}
		m.parentAnswers.Add(1)
		ch <- model.Chunk{Type: model.ChunkText, Text: "parent done"}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

// bgTool starts a background child with the prompt it is given.
type bgTool struct{ f *SubagentFactory }

func (bgTool) Name() string            { return "bg" }
func (bgTool) Description() string     { return "start one in the background" }
func (bgTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (bgTool) Mutates() bool           { return false }
func (b bgTool) Run(ctx context.Context, _ *tools.Session, raw json.RawMessage) tools.Result {
	var a struct {
		Prompt string `json:"prompt"`
	}
	_ = json.Unmarshal(raw, &a)
	id, err := b.f.SpawnBackground(ctx, SubagentRequest{Prompt: a.Prompt, Description: a.Prompt})
	if err != nil {
		return tools.Result{Content: err.Error(), IsError: true}
	}
	return tools.Result{Content: "Started in background: task_id " + id}
}

// bgRig is a parent loop with a background manager and the bg tool.
type bgRig struct {
	l     *Loop
	f     *SubagentFactory
	m     *bgModel
	store *MemStore
}

func newBGRig(t *testing.T, mode WakeMode, children ...string) *bgRig {
	t.Helper()
	dir := tempDir(t)
	sess, err := tools.NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := newBGModel(children...)
	store := NewMemStore()
	reg := tools.NewRegistry(tools.Read{})
	f := &SubagentFactory{Adapter: m, Tools: reg, Policy: policy.New(policy.ModeDefault),
		Session: sess, Store: store, Budget: NewBudget(1_000_000, 20, false), Config: DefaultConfig(), Workspace: dir}
	reg.Add(bgTool{f})
	l := NewLoop(m, reg, policy.New(policy.ModeDefault), AutoApprove{}, sess, NewRecorder(store, "parent", ""), DefaultConfig())
	l.Budget = f.Budget
	NewBackground(l, BackgroundPolicy{Wake: mode, MaxLive: 4, Settle: 20 * time.Millisecond})
	t.Cleanup(func() { l.Background.Close(TermSessionClosed) })
	return &bgRig{l: l, f: f, m: m, store: store}
}

func (r *bgRig) events(t *testing.T) []Event {
	t.Helper()
	evs, err := r.store.Events("parent")
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// messagesEqualFork is the invariant every delivery path keeps: the
// conversation rebuilt from the record is the one the loop holds.
func messagesEqualFork(t *testing.T, r *bgRig) {
	t.Helper()
	forked, err := Fork(r.events(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	r.l.runMu.Lock()
	live := append([]model.Message(nil), r.l.Messages()...)
	r.l.runMu.Unlock()
	if !reflect.DeepEqual(forked, live) {
		t.Fatalf("Fork differs from the live conversation:\nfork %+v\nlive %+v", forked, live)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// A background task returns at once with its id, while its child still runs.
func TestBackgroundTaskReturnsImmediately(t *testing.T) {
	r := newBGRig(t, WakeNotify, "one")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.l.Run(context.Background(), "go")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run waited for a background child")
	}
	if r.l.Background.Live() != 1 {
		t.Fatalf("live children: %d", r.l.Background.Live())
	}
	obs := payloads[Observation](r.events(t), EvObservation)
	if len(obs) != 1 || !strings.Contains(obs[0].Content, "Started in background: task_id ") {
		t.Fatalf("the call's result: %+v", obs)
	}
	r.m.release("one")
}

// In join mode (off) the run that started a child waits for it, the result
// arrives at the boundary as a task_status call and its result, and the run
// ends only after the model has seen it.
func TestOffModeJoinsAndDeliversAtBoundary(t *testing.T) {
	r := newBGRig(t, WakeOff, "one")
	go func() {
		time.Sleep(100 * time.Millisecond)
		r.m.release("one")
	}()
	reason, err := r.l.Run(context.Background(), "go")
	if err != nil || reason != TermCompleted {
		t.Fatalf("run: %s %v", reason, err)
	}
	if r.l.Background.Live() != 0 || len(r.m.saw) != 1 || !strings.Contains(r.m.saw[0], "result of one") {
		t.Fatalf("the run ended before its child's result: live %d, saw %q", r.l.Background.Live(), r.m.saw)
	}
	evs := r.events(t)
	notices := payloads[Notice](evs, EvSubagentNotice)
	if len(notices) != 1 || notices[0].Delivery != "boundary" || notices[0].Status != "completed" {
		t.Fatalf("notices: %+v", notices)
	}
	msgs := r.l.Messages()
	var call *model.Message
	for i := range msgs {
		if len(msgs[i].ToolCalls) == 1 && msgs[i].ToolCalls[0].Name == "task_status" {
			call = &msgs[i]
			if next := msgs[i+1]; next.Role != model.RoleTool || next.ToolCallID != call.ToolCalls[0].ID {
				t.Fatalf("the notice is not a tool result: %+v", next)
			}
		}
		if msgs[i].Role == model.RoleUser && strings.Contains(msgs[i].Content, "result of one") {
			t.Fatal("a background result reached the conversation as the person's message")
		}
	}
	if call == nil {
		t.Fatal("no task_status call in the conversation")
	}
	// Recorded first, then applied: the notice precedes the next model call.
	var noticeSeq, returnedSeq int64
	for _, e := range evs {
		switch e.Type {
		case EvSubagentNotice:
			noticeSeq = e.Seq
		case EvSubagentReturn:
			returnedSeq = e.Seq
		}
	}
	if returnedSeq == 0 || noticeSeq < returnedSeq {
		t.Fatalf("returned %d, notice %d", returnedSeq, noticeSeq)
	}
	messagesEqualFork(t, r)
}

// A notice is recorded as untrusted, from the system, and its content is
// redacted as the parent's record is, live and in the record alike.
func TestNoticeRedactedAndUntrusted(t *testing.T) {
	r := newBGRig(t, WakeOff, "one")
	st := secrets.Open(t.TempDir() + "/s.json")
	if err := st.Set("KEY", "sk-live-abcdefghijklmnop"); err != nil {
		t.Fatal(err)
	}
	red, err := st.LoadRedactor()
	if err != nil {
		t.Fatal(err)
	}
	r.l.Recorder.Redact = red
	go func() { time.Sleep(50 * time.Millisecond); r.m.release("one") }()
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	for _, e := range r.events(t) {
		if e.Type == EvSubagentNotice {
			if e.Trust != Untrusted || e.Actor != ActorSystem {
				t.Fatalf("notice trust %s actor %s", e.Trust, e.Actor)
			}
			if strings.Contains(string(e.Payload), "sk-live-abcdefghijklmnop") {
				t.Fatal("the notice's record holds the secret")
			}
		}
	}
	if len(r.m.saw) != 1 || strings.Contains(r.m.saw[0], "sk-live-abcdefghijklmnop") {
		t.Fatalf("the model saw the secret in the notice: %q", r.m.saw)
	}
	messagesEqualFork(t, r)
}

// A message steered to a run waiting on its children is taken at once.
func TestSteerWakesWaitingRun(t *testing.T) {
	r := newBGRig(t, WakeOff, "one")
	done := make(chan TerminalReason, 1)
	go func() {
		reason, _ := r.l.Run(context.Background(), "go")
		done <- reason
	}()
	waitFor(t, "the run to wait on its child", func() bool { return r.m.parentAnswers.Load() == 1 })
	before := r.m.calls.Load()
	r.l.Steer("also this")
	waitFor(t, "the steer to be answered", func() bool { return r.m.calls.Load() > before })
	r.m.release("one")
	if reason := <-done; reason != TermCompleted {
		t.Fatalf("run: %s", reason)
	}
	messagesEqualFork(t, r)
}

// A run that ends without completing takes its joined children with it:
// each ends with the parent's reason, and the parent records its return.
func TestNonCompletedEndCancelsChildren(t *testing.T) {
	t.Run("interrupt", func(t *testing.T) {
		r := newBGRig(t, WakeOff, "one")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan TerminalReason, 1)
		go func() {
			reason, _ := r.l.Run(ctx, "go")
			done <- reason
		}()
		waitFor(t, "the run to wait", func() bool { return r.m.parentAnswers.Load() >= 1 })
		cancel()
		checkCancelled(t, r, <-done, TermUserInterrupt, "one")
	})
	t.Run("max turns", func(t *testing.T) {
		// One child's result takes the run to its cap while the other runs.
		r := newBGRig(t, WakeOff, "one", "two")
		r.l.Config.MaxTurns = 2
		done := make(chan TerminalReason, 1)
		go func() {
			reason, _ := r.l.Run(context.Background(), "go")
			done <- reason
		}()
		waitFor(t, "the run to wait", func() bool { return r.m.parentAnswers.Load() >= 1 })
		r.m.release("one")
		checkCancelled(t, r, <-done, TermMaxTurns, "two")
	})
}

// checkCancelled holds that the run ended with reason, and that the named
// child ended with it too, in its own record and in its parent's.
func checkCancelled(t *testing.T, r *bgRig, got, reason TerminalReason, name string) {
	t.Helper()
	if got != reason {
		t.Fatalf("run ended %s, want %s", got, reason)
	}
	if r.l.Background.Live() != 0 {
		t.Fatal("a joined child outlived its run")
	}
	for _, ret := range payloads[map[string]any](r.events(t), EvSubagentReturn) {
		if ret["description"] != name {
			continue
		}
		if ret["reason"] != string(reason) {
			t.Fatalf("returned: %v", ret)
		}
		child, _ := r.store.Events(ret["session"].(string))
		if end, _ := LastEnd(child); end.Reason != reason {
			t.Fatalf("the child's own end: %s", end.Reason)
		}
		return
	}
	t.Fatalf("no return recorded for %s", name)
}

// The number of live children is bounded per session, across runs.
func TestBackgroundLimit(t *testing.T) {
	r := newBGRig(t, WakeNotify, "a", "b", "c")
	r.l.Background.policy.MaxLive = 2
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if n := r.l.Background.Live(); n != 2 {
		t.Fatalf("live: %d", n)
	}
	var refused int
	for _, o := range payloads[Observation](r.events(t), EvObservation) {
		if strings.Contains(o.Content, "background task limit reached (2 of 2 running)") {
			refused++
		}
	}
	if refused != 1 || r.f.Budget.spawned.Load() != 2 {
		t.Fatalf("refused %d, spawned %d", refused, r.f.Budget.spawned.Load())
	}
	for _, c := range []string{"a", "b", "c"} {
		r.m.release(c)
	}
}

// A child that outlives its lifetime ends deadline, and its notice says so.
func TestBackgroundMaxLifetime(t *testing.T) {
	r := newBGRig(t, WakeOff, "slow")
	r.l.Background.policy.Lifetime = 150 * time.Millisecond
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	n := payloads[Notice](r.events(t), EvSubagentNotice)
	if len(n) != 1 || n[0].Reason != string(TermDeadline) || !strings.Contains(n[0].Content, "ended early: deadline") {
		t.Fatalf("notice: %+v", n)
	}
}

// Results the record holds but the conversation never took are rebuilt, for
// a session continued elsewhere or a fork cut between the two.
func TestPendingNoticeRestoredOnResume(t *testing.T) {
	r := newBGRig(t, WakeOff, "one")
	go func() { time.Sleep(50 * time.Millisecond); r.m.release("one") }()
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs := r.events(t)
	if got := PendingNotices(evs, r.store.Events); len(got) != 0 {
		t.Fatalf("a delivered notice is pending again: %+v", got)
	}
	// Cut the record just before the notice: its return is there, it is not.
	var cut []Event
	for _, e := range evs {
		if e.Type == EvSubagentNotice {
			break
		}
		cut = append(cut, e)
	}
	got := PendingNotices(cut, r.store.Events)
	if len(got) != 1 || !strings.Contains(got[0].Content, "result of one") || got[0].Status != "completed" {
		t.Fatalf("rebuilt: %+v", got)
	}

	// A new loop continuing the cut record takes it at its first boundary.
	next := newBGRig(t, WakeOff)
	msgs, err := Fork(cut, 0)
	if err != nil {
		t.Fatal(err)
	}
	next.l.SetHistory(msgs, 1)
	next.l.QueueNotices(got)
	if _, err := next.l.Run(context.Background(), "and now?"); err != nil {
		t.Fatal(err)
	}
	if n := payloads[Notice](next.events(t), EvSubagentNotice); len(n) != 1 || n[0].TaskID != got[0].TaskID {
		t.Fatalf("the rebuilt notice was not delivered: %+v", n)
	}
}

// A notice the record refuses is not applied, and the run ends as an error.
func TestRefusedNoticeNotApplied(t *testing.T) {
	r := newBGRig(t, WakeOff)
	r.l.Recorder = NewRecorder(refusing{r.store, EvSubagentNotice}, "parent", "")
	r.l.QueueNotices([]Notice{{TaskID: "t", Session: "t", CallID: "bgn_x", Content: "x"}})
	reason, err := r.l.Run(context.Background(), "hi")
	if err == nil || reason != TermError {
		t.Fatalf("run: %s %v", reason, err)
	}
	for _, m := range r.l.Messages() {
		if m.ToolCallID == "bgn_x" {
			t.Fatal("a notice the record refused reached the conversation")
		}
	}
	if r.l.Background.Pending() != 1 {
		t.Fatal("the refused notice was lost")
	}
}

// refusing is a store that refuses one event type.
type refusing struct {
	Store
	typ EventType
}

func (s refusing) Append(ev Event) error {
	if ev.Type == s.typ {
		return context.Canceled
	}
	return s.Store.Append(ev)
}

// noNoticeAfterClosingEnd holds the owed-work invariant: once a run's end
// says nothing is owed (no background), no result follows it until another
// run starts, since that end released the session and closed its stream.
func noNoticeAfterClosingEnd(t *testing.T, events []Event) {
	t.Helper()
	closed := false
	for _, e := range events {
		switch e.Type {
		case EvUserMessage, EvSessionWoken:
			closed = false
		case EvSessionEnded:
			var end SessionEnded
			_ = json.Unmarshal(e.Payload, &end)
			closed = end.Background == 0
		case EvSubagentNotice:
			if closed {
				t.Fatalf("a result (seq %d) followed an end that said nothing was owed", e.Seq)
			}
		}
	}
}

// A run that ends as max_turns, max_budget or error while a finished child's
// result waits undelivered counts it as owed: the end keeps the session open,
// the result is delivered, and the closing end follows it.
func TestRunEndCountsOwedResults(t *testing.T) {
	for _, c := range []struct {
		name   string
		prompt string
		reason TerminalReason
		arm    func(r *bgRig)
		during func(r *bgRig)
	}{
		{"max_turns", "slow", TermMaxTurns, func(r *bgRig) { r.l.Config.MaxTurns = r.l.turns + 1 }, nil},
		{"max_budget", "slow", TermMaxBudget, nil, func(r *bgRig) { r.l.Budget.Spend(10_000_000) }},
		{"error", "fail", TermError, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newBGRig(t, WakeNotify, "one")
			r.m.hold = make(chan struct{})
			if reason, err := r.l.Run(context.Background(), "go"); err != nil || reason != TermCompleted {
				t.Fatalf("first run: %s %v", reason, err)
			}
			if c.arm != nil {
				c.arm(r)
			}
			calls := r.m.calls.Load()
			done := make(chan TerminalReason, 1)
			go func() {
				reason, _ := r.l.Run(context.Background(), c.prompt)
				done <- reason
			}()
			waitFor(t, "the turn to start", func() bool { return r.m.calls.Load() > calls })
			r.m.release("one")
			waitFor(t, "the result to wait", func() bool { return r.l.Background.Pending() == 1 })
			if c.during != nil {
				c.during(r)
			}
			close(r.m.hold)
			if got := <-done; got != c.reason {
				t.Fatalf("ended %s, want %s", got, c.reason)
			}
			var ended SessionEnded
			for _, e := range r.events(t) {
				if e.Type == EvSessionEnded {
					_ = json.Unmarshal(e.Payload, &ended)
				}
				if e.Type == EvSessionEnded && ended.Reason == c.reason {
					break
				}
			}
			if ended.Reason != c.reason || ended.Background != 1 {
				t.Fatalf("the %s end: %+v, want background 1 for the owed result", c.reason, ended)
			}
			waitFor(t, "the closing end", func() bool { e, _ := LastEnd(r.events(t)); return e.Settled })
			if n := len(payloads[map[string]any](r.events(t), EvSubagentNotice)); n != 1 {
				t.Fatalf("%d notices, want 1", n)
			}
			noNoticeAfterClosingEnd(t, r.events(t))
		})
	}
}

// A child's end and its result are one step: at no moment is it neither
// running nor owed, so a run ending just then still counts it.
func TestChildEndAndNoticeAreAtomic(t *testing.T) {
	seen := make(chan int, 1)
	testHookChildEnded = func(b *Background) { seen <- b.Owed() }
	t.Cleanup(func() { testHookChildEnded = nil })
	r := newBGRig(t, WakeNotify, "one")
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	r.m.release("one")
	select {
	case owed := <-seen:
		if owed != 1 {
			t.Fatalf("owed %d as the child ended, want 1: its result was not yet counted", owed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the child never ended")
	}
	waitFor(t, "the closing end", func() bool { e, _ := LastEnd(r.events(t)); return e.Settled })
	noNoticeAfterClosingEnd(t, r.events(t))
}

// A task's result rebuilt from the record twice is queued once; a resumed
// task owing two results has both queued.
func TestQueueNoticesOncePerTask(t *testing.T) {
	r := newBGRig(t, WakeOff)
	n := Notice{TaskID: "t-1", Session: "t-1", CallID: "bgn_1", Content: "x"}
	r.l.QueueNotices([]Notice{n})
	r.l.QueueNotices([]Notice{n, {TaskID: "t-2", Session: "t-2", CallID: "bgn_2"}})
	if got := r.l.Background.Pending(); got != 2 {
		t.Fatalf("pending %d, want 2", got)
	}
	r.l.QueueNotices([]Notice{n, {TaskID: "t-1", Session: "t-1", CallID: "bgn_3", Content: "y"}})
	if got := r.l.Background.Pending(); got != 3 {
		t.Fatalf("pending %d, want 3: the resumed task's second result was dropped", got)
	}
}

// Close records the closing end and tells the host, as an idle settle does.
func TestCloseFiresIdle(t *testing.T) {
	r := newBGRig(t, WakeNotify, "one")
	settled := make(chan bool, 4)
	r.l.Background.SetHooks(BackgroundHooks{Idle: func(ev IdleEvent) { settled <- ev.Settled }})
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	r.l.Background.Close(TermSessionDeleted)
	select {
	case s := <-settled:
		if !s {
			t.Fatal("Idle fired without the closing end")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close recorded the closing end and never told the host")
	}
	if e, _ := LastEnd(r.events(t)); !e.Settled {
		t.Fatalf("last end %+v", e)
	}
}

// touchy is a tool that changes things, so the default mode asks for it.
type touchy struct{}

func (touchy) Name() string            { return "touchy" }
func (touchy) Description() string     { return "changes something" }
func (touchy) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (touchy) Mutates() bool           { return true }
func (touchy) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	return tools.Result{Content: "changed"}
}

// taskApprover records the background task each ask names.
type taskApprover struct {
	mu    sync.Mutex
	tasks []string
}

func (a *taskApprover) Approve(ctx context.Context, _ string, _ json.RawMessage, _ policy.Result) (bool, error) {
	a.mu.Lock()
	a.tasks = append(a.tasks, BackgroundTaskOf(ctx))
	a.mu.Unlock()
	return false, nil
}

// A background task's ask carries its task id, so a surface can say which
// task is waiting.
func TestBackgroundAskNamesItsTask(t *testing.T) {
	r := newBGRig(t, WakeOff, "asker")
	appr := &taskApprover{}
	r.l.Approver = appr
	r.f.Tools.Add(touchy{})
	done := make(chan struct{})
	go func() {
		_, _ = r.l.Run(context.Background(), "go")
		close(done)
	}()
	waitFor(t, "the ask", func() bool { appr.mu.Lock(); defer appr.mu.Unlock(); return len(appr.tasks) == 1 })
	r.m.release("asker")
	<-done
	spawned := payloads[map[string]any](r.events(t), EvSubagentSpawned)
	if len(spawned) != 1 || appr.tasks[0] == "" || appr.tasks[0] != spawned[0]["task_id"] {
		t.Fatalf("the ask named task %q; spawned %v", appr.tasks[0], spawned)
	}
}

// A long summary is cut on a rune boundary: the record keeps the text the
// conversation has, so Fork rebuilds it exactly.
func TestSummaryCutOnARuneBoundary(t *testing.T) {
	prompt := "x" + strings.Repeat("é", 5000)
	r := newBGRig(t, WakeNotify, prompt)
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	r.m.release(prompt)
	waitFor(t, "the closing end", func() bool { e, _ := LastEnd(r.events(t)); return e.Settled })
	n := payloads[Notice](r.events(t), EvSubagentNotice)
	if len(n) != 1 || !utf8.ValidString(n[0].Content) || !strings.Contains(n[0].Content, "[summary truncated]") {
		t.Fatalf("notice: %d, valid %v", len(n), len(n) == 1 && utf8.ValidString(n[0].Content))
	}
	messagesEqualFork(t, r)
	if got := truncateSummary(strings.Repeat("é", MaxSummaryChars)); !utf8.ValidString(got) {
		t.Fatal("truncateSummary cut a rune")
	}
}

// A result rebuilt from the record is cut on a rune boundary too.
func TestPendingNoticeCutOnARuneBoundary(t *testing.T) {
	store := NewMemStore()
	child := NewRecorder(store, "c1", "p")
	_, _ = child.Record(EvAgentMessage, ActorAgent, Trusted, Message{Text: "x" + strings.Repeat("é", MaxSummaryChars)})
	_, _ = child.Record(EvSessionEnded, ActorSystem, Trusted, SessionEnded{Reason: TermCompleted, Turns: 1})
	parent := NewRecorder(store, "p", "")
	_, _ = parent.Record(EvSubagentReturn, ActorAgent, Trusted, map[string]any{"background": true, "task_id": "c1", "session": "c1", "reason": "completed"})
	evs, _ := store.Events("p")
	pend := PendingNotices(evs, store.Events)
	if len(pend) != 1 || !utf8.ValidString(pend[0].Content) || !strings.Contains(pend[0].Content, "[summary truncated]") {
		t.Fatalf("owed: %d, valid %v", len(pend), len(pend) == 1 && utf8.ValidString(pend[0].Content))
	}
}
