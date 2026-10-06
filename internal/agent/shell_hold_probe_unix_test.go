//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// End to end: a shell given no secret prints a guess at a stored value's
// start and waits; what shell_output shows does not depend on the guess.
func TestShellOutputProbeSeesNoDifference(t *testing.T) {
	probe := func(guess string) string {
		a := &stepAdapter{steps: []func(model.Request) scriptedTurn{
			func(model.Request) scriptedTurn {
				return scriptedTurn{calls: []model.ToolCall{{ID: "c1", Name: "bash", Args: json.RawMessage(
					`{"command":"printf '` + guess + `'; sleep 5","description":"probe","run_in_background":true}`)}}}
			},
			func(req model.Request) scriptedTurn {
				time.Sleep(2 * shellQuietRelease)
				return scriptedTurn{calls: []model.ToolCall{{ID: "c2", Name: "shell_output", Args: json.RawMessage(
					`{"shell_id":"` + lastShellID(req) + `"}`)}}}
			},
			func(req model.Request) scriptedTurn {
				return scriptedTurn{calls: []model.ToolCall{{ID: "c3", Name: "shell_kill", Args: json.RawMessage(
					`{"shell_id":"` + lastShellID(req) + `"}`)}}}
			},
		}}
		r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, a)
		if _, err := r.l.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, req := range a.reqs {
			for _, m := range req.Messages {
				if m.Role == model.RoleTool && strings.Contains(m.Content, "running ·") {
					return m.Content
				}
			}
		}
		t.Fatalf("no read of the running shell for %q", guess)
		return ""
	}
	// tok-9f8e starts the rig's stored value; tok-0f8e does not.
	for _, guess := range []string{"tok-0f8e", "tok-9f8e"} {
		if got := probe(guess); !strings.Contains(got, "\n"+guess+"\n") {
			t.Errorf("probe %q: the read held it back, which tells the model it starts a value:\n%s", guess, got)
		}
	}
}
