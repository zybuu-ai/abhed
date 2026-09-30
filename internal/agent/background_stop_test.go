package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
)

// A Stop while a tasks call is still starting its children (here, resolving
// a slow model) stops it: the child being prepared is refused, the rest are
// not started, and none is left running.
func TestStopDuringTasksStartStartsNoMore(t *testing.T) {
	r := newBGRig(t, WakeNotify, "a", "b", "c")
	block := make(chan struct{})
	inSlow := make(chan struct{})
	var laterAsked atomic.Int32
	r.f.ModelNames = []string{"fast", "slow", "later"}
	r.f.Models = func(name string) (model.Adapter, error) {
		switch name {
		case "slow":
			close(inSlow)
			<-block
		case "later":
			laterAsked.Add(1)
		}
		return r.m, nil
	}
	tk := Tasks{Spawn: r.f.Spawn, Background: r.f.SpawnBackground, Workspace: r.f.Workspace, Models: r.f.ModelNames}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan string)
	go func() {
		res := tk.Run(r.l.asParent(ctx), nil, json.RawMessage(`{"background":true,"tasks":[`+
			`{"prompt":"a","description":"a","model":"fast"},{"prompt":"b","description":"b","model":"slow"},`+
			`{"prompt":"c","description":"c","model":"later"}]}`))
		done <- res.Content
	}()
	<-inSlow
	cancel() // Stop: the run's context ends, and every task is cancelled
	r.l.Background.CancelAll(TermUserInterrupt)
	close(block)
	out := <-done
	time.Sleep(100 * time.Millisecond)
	if live := r.l.Background.Live(); live != 0 {
		t.Fatalf("%d child(ren) running after Stop:\n%s", live, out)
	}
	if laterAsked.Load() != 0 || strings.Count(out, ErrStopped.Error()) != 2 {
		t.Fatalf("tasks after the stop were started (later asked %d):\n%s", laterAsked.Load(), out)
	}
	for _, s := range payloads[map[string]any](r.events(t), EvSubagentSpawned) {
		if s["description"] != "a" {
			t.Fatalf("a task was recorded as spawned after the stop: %v", s)
		}
	}
	r.m.release("a")
}

// A stop that lands after a background task's spawn is counted and recorded,
// but before it is running, still takes it: it ends at once with the stop's
// reason, and its return is recorded.
func TestStopAfterSpawnRecordedEndsTheChild(t *testing.T) {
	r := newBGRig(t, WakeNotify, "a")
	parent := r.l.asParent(context.Background())
	req := SubagentRequest{Prompt: "a", Description: "a", AgentType: "general", epoch: r.l.Background.stopEpoch(), epochSet: true}
	// The stop comes once the spawn is counted: the reserve step has passed.
	r.f.Budget = NewBudget(1_000_000, 20, false)
	testHookSpawnCounted = func() { r.l.Background.CancelAll(TermUserInterrupt) }
	t.Cleanup(func() { testHookSpawnCounted = nil })
	if _, err := r.f.SpawnBackground(parent, req); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the return", func() bool { return len(payloads[map[string]any](r.events(t), EvSubagentReturn)) == 1 })
	ret := payloads[map[string]any](r.events(t), EvSubagentReturn)[0]
	if ret["reason"] != string(TermUserInterrupt) || r.l.Background.Live() != 0 {
		t.Fatalf("returned %v, live %d", ret, r.l.Background.Live())
	}
}

// A background task whose calling run ends while it is being prepared is not
// started either: the call it answers is gone.
func TestCallEndedDuringSpawnStartsNothing(t *testing.T) {
	r := newBGRig(t, WakeNotify, "b")
	block := make(chan struct{})
	inSlow := make(chan struct{})
	r.f.ModelNames = []string{"slow"}
	r.f.Models = func(string) (model.Adapter, error) { close(inSlow); <-block; return r.m, nil }
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := r.f.SpawnBackground(r.l.asParent(ctx), SubagentRequest{Prompt: "b", Description: "b", AgentType: "general", Model: "slow"})
		errc <- err
	}()
	<-inSlow
	cancel()
	close(block)
	if err := <-errc; err == nil || r.l.Background.Live() != 0 {
		t.Fatalf("started after its call ended: err %v, live %d", err, r.l.Background.Live())
	}
}

// A stop while task is making a background task's worktree refuses the task:
// the call is counted from before the worktree, not from its start.
func TestStopWhileTaskPreparesRefusesIt(t *testing.T) {
	r := newBGRig(t, WakeNotify, "a")
	testHookBeforeBackground = func() { r.l.Background.CancelAll(TermUserInterrupt) }
	t.Cleanup(func() { testHookBeforeBackground = nil })
	tk := Task{Spawn: r.f.Spawn, Background: r.f.SpawnBackground, Agents: r.f.Definitions, Workspace: r.f.Workspace}
	res := tk.Run(r.l.asParent(context.Background()), nil, json.RawMessage(`{"prompt":"a","description":"a","background":true}`))
	if !res.IsError || !strings.Contains(res.Content, ErrStopped.Error()) || r.l.Background.Live() != 0 {
		t.Fatalf("started after a stop: %+v, live %d", res, r.l.Background.Live())
	}
}

// The same for tasks: a stop after the call began refuses every task in it.
func TestStopWhileTasksPreparesRefusesThem(t *testing.T) {
	r := newBGRig(t, WakeNotify, "a", "b", "c")
	tk := Tasks{Spawn: r.f.Spawn, Background: r.f.SpawnBackground, Workspace: r.f.Workspace}
	// A stop before the call does not refuse it.
	r.l.Background.CancelAll(TermUserInterrupt)
	if res := tk.Run(r.l.asParent(context.Background()), nil, json.RawMessage(`{"background":true,"tasks":[{"prompt":"c","description":"c"}]}`)); res.IsError {
		t.Fatalf("a stop before the call refused it: %s", res.Content)
	}
	r.m.release("c")
	testHookBeforeBackground = func() { r.l.Background.CancelAll(TermUserInterrupt) }
	t.Cleanup(func() { testHookBeforeBackground = nil })
	res := tk.Run(r.l.asParent(context.Background()), nil, json.RawMessage(`{"background":true,"tasks":[{"prompt":"a","description":"a"},{"prompt":"b","description":"b"}]}`))
	if !res.IsError || strings.Count(res.Content, ErrStopped.Error()) != 2 || r.l.Background.Live() != 0 {
		t.Fatalf("started after a stop: %+v, live %d", res, r.l.Background.Live())
	}
}

// A Stop while idle in auto is not followed by a wake: the stopped tasks'
// results are recorded as skipped:stopped and no model call is made, until
// the next prompted run, after which results wake again.
func TestStopWhileIdleBlocksWakeUntilPrompted(t *testing.T) {
	r := newBGRig(t, WakeAuto, "one", "two")
	r.l.Background.policy.MaxWakesPerHour = 4
	h := hostFor(r, true)
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "children's calls", func() bool { return r.m.childrenInCall() == 2 })
	calls := r.m.calls.Load()
	r.l.Background.CancelAll(TermUserInterrupt) // Stop in the console while idle
	waitFor(t, "notices", func() bool { return len(payloads[Notice](r.events(t), EvSubagentNotice)) == 2 })
	h.wg.Wait()
	time.Sleep(100 * time.Millisecond)
	if w := payloads[SessionWoken](r.events(t), EvSessionWoken); len(w) != 0 || r.m.calls.Load() != calls {
		t.Fatalf("a wake ran after Stop: %+v, %d model calls", w, r.m.calls.Load()-calls)
	}
	for _, n := range payloads[Notice](r.events(t), EvSubagentNotice) {
		if n.Wake != "skipped:stopped" {
			t.Fatalf("a stopped task's result: wake %q, want skipped:stopped", n.Wake)
		}
	}
	// The next prompted run lifts it.
	if _, err := r.l.Run(context.Background(), "thanks"); err != nil {
		t.Fatal(err)
	}
	if ok, why := r.l.Background.canWake(); !ok && why == "stopped" {
		t.Fatal("a prompted run did not lift the stop's hold on wakes")
	}
}

// Two background tasks calls in one turn run at once: each starts all its
// tasks or none, never part, since each holds all its slots before it starts.
func TestConcurrentTasksCallsStartAllOrNone(t *testing.T) {
	for round := range 20 {
		r := newBGRig(t, WakeNotify, "a", "b", "c", "d", "e", "f")
		tk := Tasks{Spawn: r.f.Spawn, Background: r.f.SpawnBackground, Workspace: r.f.Workspace}
		var wg sync.WaitGroup
		outs := make([]string, 2)
		for j, set := range []string{`[{"prompt":"a","description":"a"},{"prompt":"b","description":"b"},{"prompt":"c","description":"c"}]`,
			`[{"prompt":"d","description":"d"},{"prompt":"e","description":"e"},{"prompt":"f","description":"f"}]`} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				outs[j] = tk.Run(r.l.asParent(context.Background()), nil, json.RawMessage(`{"background":true,"tasks":`+set+`}`)).Content
			}()
		}
		wg.Wait()
		for _, o := range outs {
			if strings.Contains(o, "Started in background") && strings.Contains(o, "FAILED") {
				t.Fatalf("round %d: a call started part of its tasks:\n%s", round, o)
			}
		}
		if live := r.l.Background.Live(); live != 3 {
			t.Fatalf("round %d: %d running, want one call's 3:\n%s\n----\n%s", round, live, outs[0], outs[1])
		}
		for _, c := range []string{"a", "b", "c", "d", "e", "f"} {
			r.m.release(c)
		}
	}
}

// The slots a tasks call held and did not use are given back.
func TestTasksCallGivesBackUnusedSlots(t *testing.T) {
	r := newBGRig(t, WakeNotify, "a")
	r.f.ModelNames = []string{"gone"}
	r.f.Models = func(string) (model.Adapter, error) { return nil, errors.New("no such provider") }
	tk := Tasks{Spawn: r.f.Spawn, Background: r.f.SpawnBackground, Workspace: r.f.Workspace, Models: r.f.ModelNames}
	res := tk.Run(r.l.asParent(context.Background()), nil, json.RawMessage(`{"background":true,"tasks":[{"prompt":"a","description":"a"},{"prompt":"x","description":"x","model":"gone"}]}`))
	if !strings.Contains(res.Content, "Started in background") || !strings.Contains(res.Content, "FAILED") {
		t.Fatalf("precondition: one started, one failed:\n%s", res.Content)
	}
	r.l.Background.mu.Lock()
	reserved := r.l.Background.reserved
	r.l.Background.mu.Unlock()
	if reserved != 0 {
		t.Fatalf("%d slot(s) still held after the call", reserved)
	}
	r.m.release("a")
}

// A hold gives out only the slots it holds, and a hold the limit cannot
// cover is not taken at all.
func TestSlotsHoldWhatTheyReserve(t *testing.T) {
	r := newBGRig(t, WakeNotify)
	b := r.l.Background
	if _, err := b.reserveN(5); err == nil {
		t.Fatal("a hold past the limit of 4 was taken")
	}
	s, err := b.reserveN(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.take(); err != nil {
		t.Fatal(err)
	}
	if err := s.take(); err == nil {
		t.Fatal("a hold of one gave out two slots")
	}
}

// A fork is refused while background tasks run, naming them: a task could
// otherwise go on acting on what the fork resets. Nothing is cancelled.
func TestForkRefusedWhileTasksRun(t *testing.T) {
	r := newBGRig(t, WakeNotify, "one")
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	_, err := r.l.ForkTo(r.events(t), 0)
	if err == nil || !strings.Contains(err.Error(), "background tasks are still running: one (") {
		t.Fatalf("fork with a task running: %v", err)
	}
	if r.l.Background.Live() != 1 || len(payloads[map[string]any](r.events(t), EvForked)) != 0 {
		t.Fatal("the refused fork cancelled the task or was recorded")
	}
	r.m.release("one")
	waitFor(t, "the closing end", func() bool { e, _ := LastEnd(r.events(t)); return e.Settled })
	if _, err := r.l.ForkTo(r.events(t), 0); err != nil {
		t.Fatalf("fork once the tasks ended: %v", err)
	}
}

// A Stop after the wake was decided and before its run takes the
// conversation refuses the run: no model call follows, and the results are
// delivered as skipped:stopped.
func TestStopBetweenWakeDecisionAndRun(t *testing.T) {
	r := newBGRig(t, WakeAuto, "one")
	r.l.Background.policy.MaxWakesPerHour = 4
	h := hostFor(r, true)
	gate := make(chan struct{})
	hooks := h.hooks()
	inner := hooks.Wake
	hooks.Wake = func(ids []string) bool {
		go func() { <-gate; inner(ids) }()
		return true
	}
	r.l.Background.SetHooks(hooks)
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the child's call", func() bool { return r.m.childrenInCall() == 1 })
	r.m.release("one")
	waitFor(t, "the wake decided", func() bool { r.l.Background.mu.Lock(); defer r.l.Background.mu.Unlock(); return r.l.Background.waking })
	r.l.Background.CancelAll(TermUserInterrupt) // Stop, with no run live yet
	calls := r.m.calls.Load()
	close(gate)
	waitFor(t, "the result", func() bool { return len(payloads[Notice](r.events(t), EvSubagentNotice)) == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := r.m.calls.Load() - calls; n != 0 || len(payloads[SessionWoken](r.events(t), EvSessionWoken)) != 0 {
		t.Fatalf("a wake ran after Stop: %d model call(s)", n)
	}
	if n := payloads[Notice](r.events(t), EvSubagentNotice)[0]; n.Wake != "skipped:stopped" {
		t.Fatalf("the result: wake %q, want skipped:stopped", n.Wake)
	}
}
