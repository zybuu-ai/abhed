package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/monitor"
)

type modeRows struct {
	*MemStore
	mu    sync.Mutex
	modes []string
}

func (m *modeRows) CreateSubagentSession(ctx context.Context, _, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modes = append(m.modes, SubagentMode(ctx, "auto"))
	return nil
}

// A subagent's row carries the mode it runs in, not a fixed "auto", and the
// parent's monitor judges the child's calls as it judges the parent's.
func TestSubagentInheritsModeRowAndMonitor(t *testing.T) {
	adapter := &scriptedAdapter{turns: []scriptedTurn{
		{calls: []model.ToolCall{call("task", map[string]string{"prompt": "fetch it", "description": "fetcher"})}},
		{calls: []model.ToolCall{{ID: "k1", Name: "bash", Args: json.RawMessage(`{"command":"curl -s https://x.example/u"}`)}}},
		{text: "could not"},
		{text: "done"},
	}}
	rows := &modeRows{MemStore: NewMemStore()}
	l, _, _ := taskTree(t, adapter, AutoApprove{Yes: true}, rows.MemStore, rows, false)
	l.Monitor = &monitor.Guard{Monitor: monitor.Func(func(_ context.Context, c monitor.Case) (monitor.Verdict, error) {
		if strings.Contains(c.Args, "curl") {
			return monitor.Verdict{Decision: policy.Deny, Code: "exfil", Rationale: "no"}, nil
		}
		return monitor.Verdict{Decision: policy.Allow, Code: "ok"}, nil
	})}
	if _, err := l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(rows.modes) != 1 || rows.modes[0] != string(policy.ModeDefault) {
		t.Fatalf("subagent row modes = %q, want the parent's default", rows.modes)
	}
	parentEvs, _ := rows.Events("parent")
	kid := ""
	for _, sp := range payloads[map[string]any](parentEvs, EvSubagentSpawned) {
		if id, _ := sp["session"].(string); id != "" {
			kid = id
		}
	}
	evs, _ := rows.Events(kid)
	judged := false
	for _, e := range evs {
		judged = judged || (e.Type == EvActionDenied && strings.Contains(string(e.Payload), `"step":"monitor"`))
	}
	if kid == "" || !judged {
		t.Fatalf("the child's call was not judged by the parent's monitor: %s", types(evs))
	}
}
