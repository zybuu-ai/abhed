package agent

import (
	"context"
	"encoding/json"
	"strings"
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
