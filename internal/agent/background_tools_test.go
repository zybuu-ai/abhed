package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// taskRig is a bgRig whose task tools are the real ones, with background.
func taskRig(t *testing.T, mode WakeMode, children ...string) *bgRig {
	t.Helper()
	r := newBGRig(t, mode, children...)
	r.f.Background = true
	r.l.Tools.Add(Task{Spawn: r.f.Spawn, Background: r.f.SpawnBackground})
	r.l.Tools.Add(Tasks{Spawn: r.f.Spawn, Background: r.f.SpawnBackground})
	r.l.Tools.Add(TaskStatus{})
	r.l.Tools.Add(TaskCancel{})
	return r
}

func callTool(t *testing.T, r *bgRig, name, args string) tools.Result {
	t.Helper()
	tool, ok := r.l.Tools.Get(name)
	if !ok {
		t.Fatalf("no %s tool", name)
	}
	return tool.Run(r.l.asParent(context.Background()), r.l.Session, json.RawMessage(args))
}

// The background flag and its note are offered only where a surface runs
// background tasks.
func TestBackgroundParamOffered(t *testing.T) {
	with := Task{Background: func(context.Context, SubagentRequest) (string, error) { return "", nil }}
	for _, c := range []struct {
		schema, desc string
		want         bool
	}{
		{string(Task{}.Schema()), Task{}.Description(), false},
		{string(with.Schema()), with.Description(), true},
	} {
		if strings.Contains(c.schema, `"background"`) != c.want || strings.Contains(c.desc, "not an instruction") != c.want {
			t.Fatalf("background offered = %v, want %v", !c.want, c.want)
		}
	}
	if !strings.Contains(string(Tasks{Background: with.Background}.Schema()), `"background"`) {
		t.Fatal("tasks does not offer background")
	}
}

// task with background returns at once with the task id; task_status lists
// and reports it; task_cancel stops it as cancelled_by_parent.
func TestTaskBackgroundStatusAndCancel(t *testing.T) {
	r := taskRig(t, WakeNotify, "work")
	res := callTool(t, r, "task", `{"prompt":"work","description":"work","background":true}`)
	if res.IsError || !strings.HasPrefix(res.Content, "Started in background: task_id ") || !strings.Contains(res.Content, "do not poll") {
		t.Fatalf("start: %+v", res)
	}
	id := strings.Fields(strings.TrimPrefix(res.Content, "Started in background: task_id "))[0]
	if list := callTool(t, r, "task_status", `{}`); !strings.Contains(list.Content, id+" (work): running") {
		t.Fatalf("status list: %q", list.Content)
	}
	if one := callTool(t, r, "task_status", `{"task_id":"`+id+`"}`); !strings.Contains(one.Content, "running") {
		t.Fatalf("status one: %q", one.Content)
	}
	if res := callTool(t, r, "task_cancel", `{"task_id":"`+id+`"}`); res.IsError {
		t.Fatalf("cancel: %+v", res)
	}
	if res := callTool(t, r, "task_cancel", `{"task_id":"`+id+`"}`); !res.IsError {
		t.Fatal("a finished task was cancelled again")
	}
	ret := payloads[map[string]any](r.events(t), EvSubagentReturn)
	if len(ret) != 1 || ret[0]["reason"] != string(TermCancelledByParent) {
		t.Fatalf("returned: %v", ret)
	}
	waitFor(t, "the notice", func() bool { return len(payloads[Notice](r.events(t), EvSubagentNotice)) == 1 })
	if n := payloads[Notice](r.events(t), EvSubagentNotice)[0]; n.Reason != string(TermCancelledByParent) || n.Status != "cancelled" {
		t.Fatalf("notice: %+v", n)
	}
}

// tasks with background is all or nothing: a call over the session's limit
// starts none.
func TestTasksBackgroundLimitAllOrNothing(t *testing.T) {
	r := taskRig(t, WakeNotify, "a", "b", "c")
	r.l.Background.policy.MaxLive = 2
	res := callTool(t, r, "tasks", `{"background":true,"tasks":[{"prompt":"a","description":"a"},{"prompt":"b","description":"b"},{"prompt":"c","description":"c"}]}`)
	if !res.IsError || !strings.Contains(res.Content, "2 more may run now, and this call asks for 3") || r.f.Budget.spawned.Load() != 0 {
		t.Fatalf("over the limit: %+v, %d spawned", res, r.f.Budget.spawned.Load())
	}
	res = callTool(t, r, "tasks", `{"background":true,"tasks":[{"prompt":"a","description":"a"},{"prompt":"b","description":"b"}]}`)
	if res.IsError || strings.Count(res.Content, "Started in background") != 2 || r.l.Background.Live() != 2 {
		t.Fatalf("within the limit: %+v", res)
	}
	r.m.release("a")
	r.m.release("b")
	r.m.release("c")
}

// task_status reads a finished child from the record when it is not in
// memory, but only a child of this session: another's is "no such task".
func TestTaskStatusOnlyOwnChildren(t *testing.T) {
	r := taskRig(t, WakeOff, "one")
	go r.m.release("one")
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	ret := payloads[map[string]any](r.events(t), EvSubagentReturn)
	id := ret[0]["task_id"].(string)
	// A new manager, as after a restart: nothing in memory.
	NewBackground(r.l, BackgroundPolicy{Wake: WakeNotify, MaxLive: 4})
	if res := callTool(t, r, "task_status", `{"task_id":"`+id+`"}`); res.IsError || !strings.Contains(res.Content, "completed") ||
		!strings.Contains(res.Content, "result of one") {
		t.Fatalf("from the record: %+v", res)
	}
	// Another session in the same store asks about it.
	other := NewLoop(r.m, r.l.Tools, r.l.Policy, AutoApprove{}, r.l.Session, NewRecorder(r.store, "other", ""), DefaultConfig())
	NewBackground(other, BackgroundPolicy{Wake: WakeNotify, MaxLive: 4})
	tool, _ := other.Tools.Get("task_status")
	res := tool.Run(other.asParent(context.Background()), other.Session, json.RawMessage(`{"task_id":"`+id+`"}`))
	if !res.IsError || strings.Contains(res.Content, "one") {
		t.Fatalf("another session read the task: %+v", res)
	}
}

// The session's wake switch is recorded and cannot go above the ceiling.
func TestSetWakeRecordedWithinCeiling(t *testing.T) {
	r := newBGRig(t, WakeNotify)
	r.l.Background.policy.Ceiling = WakeNotify
	if err := r.l.Background.SetWake(WakeAuto, "user"); err == nil {
		t.Fatal("wake went above the surface's ceiling")
	}
	if err := r.l.Background.SetWake(WakeOff, "user"); err != nil {
		t.Fatal(err)
	}
	if r.l.Background.Mode() != WakeOff {
		t.Fatal("the mode did not change")
	}
	w := payloads[WakeSet](r.events(t), EvWakeSet)
	if len(w) != 1 || w[0].Wake != WakeOff || w[0].By != "user" {
		t.Fatalf("wake_set: %+v", w)
	}
}
