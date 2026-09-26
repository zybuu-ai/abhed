package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A shutdown that ends a command on an idle workbench's terminal records the
// command's result before the session's end.
func TestShutdownRecordsATerminalsResultBeforeTheEnd(t *testing.T) {
	wb := shellBench(t, nil)
	start := wb.startPTY("sleep 60")
	time.Sleep(200 * time.Millisecond)
	wb.s.closeIdle()
	obs, end := -1, -1
	for i, e := range wb.events() {
		switch e.Type {
		case agent.EvObservation:
			var o agent.Observation
			if json.Unmarshal(e.Payload, &o) == nil && o.CallID == start.ID {
				obs = i
			}
		case agent.EvSessionEnded:
			end = i
		}
	}
	if obs < 0 || end < 0 || obs > end {
		t.Fatalf("the terminal's result is at %d and the session's end at %d", obs, end)
	}
}

// scoped offers a scope with every request, as policy does for a call that
// has a narrow rule, and puts it to the session's approver.
type scoped struct{ live *liveSession }

func (s scoped) Approve(ctx context.Context, tool string, args json.RawMessage, res policy.Result) (bool, error) {
	res.Scope = "write(*)"
	return s.live.Approve(ctx, tool, args, res)
}

// A call let through by a scope a reviewer allowed earlier is recorded as
// such, with the scope, and not as a reviewer's answer.
func TestRememberedScopeIsRecordedAsTheSessionScope(t *testing.T) {
	ws := tempDirResolved(t)
	sess, err := tools.NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	live := &liveSession{allowed: map[string]bool{"write(*)": true}}
	store := agent.NewMemStore()
	ad := &editingAdapter{calls: [][]model.ToolCall{{
		toolCall("c1", "write", map[string]string{"path": filepath.Join(ws, "x.txt"), "content": "x"}),
	}}}
	l := agent.NewLoop(ad, tools.NewRegistry(tools.Write{}), policy.New(policy.ModeDefault), scoped{live},
		sess, agent.NewRecorder(store, "s1", ""), agent.DefaultConfig())
	if _, err := l.Run(context.Background(), "write"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("s1")
	for _, e := range evs {
		if e.Type != agent.EvActionApproved {
			continue
		}
		var p map[string]string
		_ = json.Unmarshal(e.Payload, &p)
		if p["by"] != agent.BySessionScope || p["scope"] != "write(*)" {
			t.Fatalf("approved %v", p)
		}
		return
	}
	t.Fatal("no approval recorded")
}

// A command on a workbench terminal records the tier it ran under, as the
// agent's commands do.
func TestTerminalObservationCarriesItsTier(t *testing.T) {
	wb := shellBench(t, nil)
	start := wb.startPTY("true")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range wb.events() {
			var o agent.Observation
			if e.Type == agent.EvObservation && json.Unmarshal(e.Payload, &o) == nil && o.CallID == start.ID {
				if o.Sandbox != "none" {
					t.Fatalf("observation sandbox %q, want none", o.Sandbox)
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no observation for the command")
}
