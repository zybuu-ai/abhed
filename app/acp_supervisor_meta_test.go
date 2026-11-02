package app

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// The supervisor's verdict on a call reaches the editor's approval card in the
// ask's _meta, as the record holds it; a call it gave no verdict on carries none.
func TestACPAskCarriesTheSupervisorVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		note *agent.SupervisorNote
	}{
		{"with a concern", &agent.SupervisorNote{Decision: "ask", Code: "off_task", Advice: "the task is about the docs", Judge: "rules"}},
		{"without a verdict", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var meta map[string]any
			c, _ := answeringConn(t, func(params json.RawMessage) any {
				var p struct {
					ToolCall struct {
						Meta map[string]map[string]any `json:"_meta"`
					} `json:"toolCall"`
				}
				_ = json.Unmarshal(params, &p)
				mu.Lock()
				meta = p.ToolCall.Meta[acpMetaKey]
				mu.Unlock()
				return chosen(params, "reject_once")
			})
			s := &acpSession{id: "s1", cancel: func() {}, always: map[string]string{}}
			payload, _ := json.Marshal(agent.ActionRequested{CallID: "c1", Tool: "bash", Args: json.RawMessage(`{"command":"rm notes.md"}`), Supervisor: tc.note})
			ctx := agent.WithRequested(context.Background(), agent.Event{Type: agent.EvActionRequested, Payload: payload})
			ctx, _ = agent.ExpectAnswer(ctx)
			d := abhed.Decision{Decision: policy.Ask, Step: "default"}
			if _, err := c.askEditor(ctx, s, "bash", json.RawMessage(`{"command":"rm notes.md"}`), d); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			got, has := meta["supervisor"]
			if tc.note == nil {
				if has {
					t.Fatalf("supervisor %v sent without a verdict", got)
				}
				return
			}
			m, _ := got.(map[string]any)
			if m["decision"] != "ask" || m["code"] != "off_task" || m["advice"] != "the task is about the docs" {
				t.Fatalf("meta supervisor = %#v", got)
			}
		})
	}
}
