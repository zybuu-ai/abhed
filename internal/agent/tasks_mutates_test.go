package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
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
