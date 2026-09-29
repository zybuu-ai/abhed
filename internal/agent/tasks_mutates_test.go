package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/monitor"
)

// asker records that it was asked and answers yes.
type asker struct{ asked atomic.Int32 }

func (a *asker) Approve(context.Context, string, json.RawMessage, policy.Result) (bool, error) {
	a.asked.Add(1)
	return true, nil
}

// tasks with worktree isolation makes branches and checkouts on the host, so
// it is a mutating call: it asks in default mode, plan mode refuses it, and
// with nobody to ask it is refused. Without isolation it asks nothing.
func TestWorktreeTasksAskFirst(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      policy.Mode
		isolation string
		approver  Approver
		want      EventType // how the call was settled
		by        string
	}{
		{"default mode asks", policy.ModeDefault, "worktree", &asker{}, EvActionApproved, ByReviewer},
		{"plan mode refuses", policy.ModePlan, "worktree", &asker{}, EvActionDenied, ""},
		{"headless refuses", policy.ModeDefault, "worktree", AutoApprove{}, EvActionDenied, ByHeadless},
		{"no isolation asks nothing", policy.ModeDefault, "none", &asker{}, EvActionApproved, ByPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := tempDir(t)
			reg := tools.NewRegistry(Tasks{Workspace: ws, Profiles: Profiles,
				Spawn: func(context.Context, SubagentRequest) (string, error) { return "ok", nil }})
			sess, err := tools.NewSession(ws)
			if err != nil {
				t.Fatal(err)
			}
			store := NewMemStore()
			args := map[string]any{"tasks": []map[string]string{{"prompt": "p", "description": "d"}}, "isolation": tc.isolation}
			l := NewLoop(&scriptedAdapter{turns: []scriptedTurn{{calls: []model.ToolCall{call("tasks", args)}}}},
				reg, policy.New(tc.mode), tc.approver, sess, NewRecorder(store, "s", ""), DefaultConfig())
			if _, err := l.Run(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			evs, _ := store.Events("s")
			for _, ev := range evs {
				if ev.Type != EvActionApproved && ev.Type != EvActionDenied {
					continue
				}
				var d map[string]string
				_ = json.Unmarshal(ev.Payload, &d)
				if ev.Type != tc.want || (tc.by != "" && d["by"] != tc.by) {
					t.Fatalf("settled as %s %v, want %s by %q", ev.Type, d, tc.want, tc.by)
				}
				if tc.mode == policy.ModePlan && d["step"] != "mode" {
					t.Fatalf("plan mode refused it at step %q", d["step"])
				}
				return
			}
			t.Fatalf("the call was not settled: %v", evs)
		})
	}
}

// The monitor is told a worktree tasks call mutates, so it judges it as the
// change to the host it is rather than as a read.
func TestMonitorIsToldWorktreeTasksMutate(t *testing.T) {
	ws := tempDir(t)
	reg := tools.NewRegistry(Tasks{Workspace: ws, Profiles: Profiles,
		Spawn: func(context.Context, SubagentRequest) (string, error) { return "ok", nil }})
	sess, err := tools.NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	var told atomic.Int32 // 1 read, 2 mutates
	judge := monitor.Func(func(_ context.Context, c monitor.Case) (monitor.Verdict, error) {
		if c.Tool == "tasks" {
			told.Store(1)
			if c.Mutates {
				told.Store(2)
			}
		}
		return monitor.Verdict{Decision: policy.Allow}, nil
	})
	args := map[string]any{"tasks": []map[string]string{{"prompt": "p", "description": "d"}}, "isolation": "worktree"}
	l := NewLoop(&scriptedAdapter{turns: []scriptedTurn{{calls: []model.ToolCall{call("tasks", args)}}}},
		reg, policy.New(policy.ModeBypass), &asker{}, sess, NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
	l.Monitor = &monitor.Guard{Monitor: judge}
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if told.Load() != 2 {
		t.Fatalf("the monitor was not told the worktree call mutates (state %d)", told.Load())
	}
}

// callProbe is read-only unless its arguments say it mutates, and records
// when it ran; slow is read-only and takes a while.
type callProbe struct {
	name    string
	sleep   time.Duration
	started *atomic.Int64
	ended   *atomic.Int64
}

func (p callProbe) Name() string            { return p.name }
func (p callProbe) Description() string     { return "probe" }
func (p callProbe) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p callProbe) Mutates() bool           { return false }
func (p callProbe) MutatesCall(raw json.RawMessage) bool {
	return strings.Contains(string(raw), `"mutate":true`)
}
func (p callProbe) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	p.started.Store(time.Now().UnixNano())
	time.Sleep(p.sleep)
	p.ended.Store(time.Now().UnixNano())
	return tools.Result{Content: "ok"}
}

// A call that mutates by its arguments runs alone, after the turn's read-only
// calls, as a mutating tool's call does.
func TestACallThatMutatesRunsAfterTheReads(t *testing.T) {
	var ms, me, ss, se atomic.Int64
	reg := tools.NewRegistry(
		callProbe{name: "probe", started: &ms, ended: &me},
		callProbe{name: "slow", sleep: 150 * time.Millisecond, started: &ss, ended: &se},
	)
	ws := tempDir(t)
	sess, err := tools.NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLoop(&scriptedAdapter{turns: []scriptedTurn{{calls: []model.ToolCall{
		{ID: "m", Name: "probe", Args: json.RawMessage(`{"mutate":true}`)},
		{ID: "r", Name: "slow", Args: json.RawMessage(`{}`)},
	}}}}, reg, policy.New(policy.ModeBypass), &asker{}, sess, NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if ms.Load() == 0 || ms.Load() < se.Load() {
		t.Fatal("the mutating call ran alongside the read-only one")
	}
}
