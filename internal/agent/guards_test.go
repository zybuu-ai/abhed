package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// A resume as another role is a new task, not this one going on.
func TestResumeAsAnotherRoleRefused(t *testing.T) {
	r := newResumeRig(t, "")
	r.f.Definitions = WithDefinitions(&Definition{Name: "other", Description: "d", Instruction: "i"})
	id := r.spawn(t, SubagentRequest{Prompt: "work", Description: "d"})
	if _, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: id, AgentType: "other"}); err == nil ||
		!strings.Contains(err.Error(), "resume it as that") {
		t.Fatalf("a resume as another role: %v", err)
	}
}

// A task recorded with no provider goes on only on a model of the same name:
// never on the current one when the names differ.
func TestResumeWithoutProviderNeedsTheSameModel(t *testing.T) {
	r := newResumeRig(t, "")
	rec := NewRecorder(r.store, "NOPROV1", "parent")
	spawned := map[string]any{"description": "old", "agent_type": "general", "definition": "general", "depth": 0,
		"workspace": r.ws, "session": "NOPROV1", "model": "another-model"}
	_, _ = rec.Record(EvSubagentSpawned, ActorAgent, Trusted, spawned)
	_, _ = rec.Record(EvUserMessage, ActorUser, Trusted, Message{Text: "work"})
	_, _ = rec.Record(EvAgentMessage, ActorAgent, Trusted, Message{Text: "first answer"})
	_, _ = rec.Record(EvSessionEnded, ActorSystem, Trusted, SessionEnded{Reason: TermCompleted, Turns: 1})
	if _, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: "NOPROV1"}); err == nil ||
		!strings.Contains(err.Error(), "not available here") {
		t.Fatalf("resumed on a model of another name: %v", err)
	}
}

// Two resumes of one finished task at once: exactly one goes on.
func TestConcurrentResumesOneWins(t *testing.T) {
	r := newResumeRig(t, "")
	id := r.spawn(t, SubagentRequest{Prompt: "work", Description: "d"})
	gate := make(chan struct{})
	r.f.Adapter, r.l.Adapter = gated{r.m, gate}, gated{r.m, gate} // the resume's model call waits
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: id})
		}()
	}
	time.Sleep(200 * time.Millisecond) // both have asked; the first holds the task
	close(gate)
	wg.Wait()
	refused := errs[0]
	if refused == nil {
		refused = errs[1]
	}
	if (errs[0] == nil) == (errs[1] == nil) || !strings.Contains(refused.Error(), "no such task") {
		t.Fatalf("two concurrent resumes: %v and %v, want one to go on and one refused", errs[0], errs[1])
	}
	resumes := 0
	for _, s := range payloads[map[string]any](r.events(t), EvSubagentSpawned) {
		if s["resume"] == true {
			resumes++
		}
	}
	if resumes != 1 {
		t.Fatalf("%d resumes recorded, want 1", resumes)
	}
}

// A task this session reports as running is not resumed, whatever its
// record says.
func TestResumeOfARunningTaskRefused(t *testing.T) {
	r := newResumeRig(t, "")
	id := r.spawn(t, SubagentRequest{Prompt: "work", Description: "d"})
	b := r.l.Background
	b.mu.Lock()
	b.tasks[id] = &bgTask{ID: id, done: make(chan struct{}), cancel: func(error) {}}
	b.order = append(b.order, id)
	b.mu.Unlock()
	_, err := r.f.Spawn(r.ctx(), SubagentRequest{Prompt: "more", Resume: id})
	b.mu.Lock()
	b.tasks[id].ended = true
	close(b.tasks[id].done)
	b.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "no such task") {
		t.Fatalf("a running task was resumed: %v", err)
	}
}

// A subagent cannot start a background task, even on a loop with a manager.
func TestSubagentCannotStartBackground(t *testing.T) {
	r := newBGRig(t, WakeNotify, "a")
	r.l.depth = 1
	if _, err := r.f.SpawnBackground(r.l.asParent(context.Background()), SubagentRequest{Prompt: "a", Description: "a", AgentType: "general"}); err == nil ||
		!strings.Contains(err.Error(), "a subagent cannot start a background task") {
		t.Fatalf("a nested background task: %v", err)
	}
}

// At a turn boundary, background results come before the person's steering,
// each in arrival order.
func TestNoticesBeforeSteeringAtABoundary(t *testing.T) {
	r := newBGRig(t, WakeNotify, "one")
	r.m.hold = make(chan struct{})
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	calls := r.m.calls.Load()
	done := make(chan struct{})
	go func() { _, _ = r.l.Run(context.Background(), "slow"); close(done) }()
	waitFor(t, "the slow turn", func() bool { return r.m.calls.Load() > calls })
	r.l.QueueMessage(Message{Text: "and also"})
	r.m.release("one")
	waitFor(t, "the result", func() bool { return r.l.Background.Pending() == 1 })
	close(r.m.hold)
	<-done
	var notice, steer int64
	for _, e := range r.events(t) {
		switch {
		case e.Type == EvSubagentNotice:
			notice = e.Seq
		case e.Type == EvUserMessage && strings.Contains(string(e.Payload), "and also"):
			steer = e.Seq
		}
	}
	if notice == 0 || steer == 0 || notice > steer {
		t.Fatalf("notice at %d, steering at %d: the result must come first", notice, steer)
	}
	messagesEqualFork(t, r)
}

// The closing end is not due while a result waits to be delivered, even
// with no child running and no wake starting.
func TestNoClosingEndWithAResultWaiting(t *testing.T) {
	r := newBGRig(t, WakeNotify)
	b := r.l.Background
	r.l.runMu.Lock()
	defer r.l.runMu.Unlock()
	b.mu.Lock()
	b.owed = true
	b.lastEnd = SessionEnded{Reason: TermCompleted, Background: 1}
	b.notices = []Notice{{TaskID: "t1", CallID: "bgn_1", Content: "x"}}
	b.mu.Unlock()
	if b.settleIfDue() {
		t.Fatal("the closing end was recorded with a result waiting")
	}
	b.mu.Lock()
	b.notices = nil
	b.mu.Unlock()
	if !b.settleIfDue() {
		t.Fatal("the closing end was not recorded once nothing was owed")
	}
}

// A wake being started with results waiting is not a moment to record the
// closing end: it follows the results the wake delivers.
func TestNoClosingEndWhileAWakeStarts(t *testing.T) {
	r := newBGRig(t, WakeAuto, "one", "two")
	r.l.Background.policy.MaxWakesPerHour = 4
	h := hostFor(r, true)
	h.delay = 300 * time.Millisecond // the surface starts its wake run a while after it is asked
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	r.m.release("one")
	time.Sleep(60 * time.Millisecond) // the wake is being started
	r.m.release("two")
	time.Sleep(100 * time.Millisecond) // another idle delivery comes round
	waitFor(t, "the wake run's end", func() bool {
		h.mu.Lock()
		ran := len(h.runs)
		h.mu.Unlock()
		e, _ := LastEnd(r.events(t))
		return ran >= 1 && e.Background == 0 && len(payloads[Notice](r.events(t), EvSubagentNotice)) == 2
	})
	noNoticeAfterClosingEnd(t, r.events(t))
}
