package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// Two parallel reads the model sent without ids (or with one id twice) each get
// an id of their own, so a result is paired with the call that made it.
func TestCallsWithoutDistinctIDsGetTheirOwn(t *testing.T) {
	for name, ids := range map[string][2]string{"missing": {"", ""}, "repeated": {"call_0", "call_0"}} {
		t.Run(name, func(t *testing.T) {
			dir := tempDir(t)
			for _, f := range []string{"a", "b"} {
				if err := os.WriteFile(filepath.Join(dir, f+".txt"), []byte("content of "+f+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			read := func(id, f string) model.ToolCall {
				b, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, f+".txt")})
				return model.ToolCall{ID: id, Name: "read", Args: b}
			}
			l, store := harnessIn(t, dir, []scriptedTurn{{calls: []model.ToolCall{read(ids[0], "a"), read(ids[1], "b")}}, {text: "ok"}},
				policy.ModeDefault, true)
			if _, err := l.Run(context.Background(), "read both"); err != nil {
				t.Fatal(err)
			}
			evs, _ := store.Events("sess1")
			asked := map[string]string{} // call id -> file it asked for
			for _, e := range evs {
				if e.Type == EvActionRequested {
					var r ActionRequested
					_ = json.Unmarshal(e.Payload, &r)
					asked[r.CallID] = string(r.Args)
				}
			}
			if len(asked) != 2 {
				t.Fatalf("requested call ids = %v, want two distinct", asked)
			}
			for _, e := range evs {
				if e.Type != EvObservation {
					continue
				}
				var o Observation
				_ = json.Unmarshal(e.Payload, &o)
				args, ok := asked[o.CallID]
				if !ok || o.CallID == "" {
					t.Fatalf("observation call id %q names no request", o.CallID)
				}
				f := "a"
				if strings.Contains(args, "b.txt") {
					f = "b"
				}
				if !strings.Contains(o.Content, "content of "+f) {
					t.Fatalf("result for %s carries %q", args, o.Content)
				}
			}
			// The model sees the same pairing: each tool message answers its own call.
			req := l.Adapter.(*scriptedAdapter).gotRequests[1]
			sent := map[string]string{}
			for _, m := range req.Messages {
				for _, c := range m.ToolCalls {
					sent[c.ID] = string(c.Args)
				}
			}
			for _, m := range req.Messages {
				if m.Role != model.RoleTool {
					continue
				}
				args := sent[m.ToolCallID]
				f := "a"
				if strings.Contains(args, "b.txt") {
					f = "b"
				}
				if args == "" || !strings.Contains(m.Content, "content of "+f) {
					t.Fatalf("tool message %q for call %q (%s)", m.Content, m.ToolCallID, args)
				}
			}
		})
	}
}
