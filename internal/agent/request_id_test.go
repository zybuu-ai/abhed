package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

type requestIDs struct{ seen []string }

func (r *requestIDs) Approve(ctx context.Context, _ string, _ json.RawMessage, _ policy.Result) (bool, error) {
	r.seen = append(r.seen, RequestIDOf(ctx))
	return true, nil
}

// Models reuse call ids across turns (call_0, name-1), so an answer is bound
// to the recorded request instead: each ask names its own action.requested.
func TestApproverIsToldAUniqueRequestIDPerAsk(t *testing.T) {
	dir := tempDir(t)
	same := func(name string) model.ToolCall {
		b, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, name), "content": "x"})
		return model.ToolCall{ID: "call_0", Name: "write", Args: b}
	}
	l, store := harnessIn(t, dir, []scriptedTurn{{calls: []model.ToolCall{same("a.txt")}}, {calls: []model.ToolCall{same("b.txt")}}},
		policy.ModeDefault, true)
	ids := &requestIDs{}
	l.Approver = ids
	if _, err := l.Run(context.Background(), "write two files"); err != nil {
		t.Fatal(err)
	}
	if len(ids.seen) != 2 || ids.seen[0] == "" || ids.seen[0] == ids.seen[1] {
		t.Fatalf("request ids = %q, want two distinct", ids.seen)
	}
	evs, _ := store.Events("sess1")
	var asked []string
	for _, e := range evs {
		if e.Type == EvActionRequested {
			asked = append(asked, e.ID)
		}
	}
	if len(asked) != 2 || asked[0] != ids.seen[0] || asked[1] != ids.seen[1] {
		t.Fatalf("request ids %q are not the action.requested events %q", ids.seen, asked)
	}
}
