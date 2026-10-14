package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// A key a tool does not take is dropped before policy; the model is told it was,
// and the record names a bounded list of clipped keys.
func TestIgnoredArgumentsAreToldAndBounded(t *testing.T) {
	dir := tempDir(t)
	args := map[string]any{"file_path": "x.txt"}
	long := strings.Repeat("k", 200)
	args[long] = 1
	for i := range 40 {
		args[fmt.Sprintf("zz%02d", i)] = i
	}
	b, _ := json.Marshal(args)
	l, store := harnessIn(t, dir, []scriptedTurn{{calls: []model.ToolCall{{ID: "r1", Name: "read", Args: b}}}, {text: "ok"}},
		policy.ModeDefault, true)
	if _, err := l.Run(context.Background(), "read it"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("sess1")
	var req ActionRequested
	var obs Observation
	for _, e := range evs {
		switch e.Type {
		case EvActionRequested:
			_ = json.Unmarshal(e.Payload, &req)
		case EvObservation:
			_ = json.Unmarshal(e.Payload, &obs)
		}
	}
	if len(req.Dropped) != maxIgnoredKeys+1 || req.Dropped[maxIgnoredKeys] != "+10 more" {
		t.Fatalf("dropped = %d keys, last %q; want %d and a +10 more", len(req.Dropped), req.Dropped[len(req.Dropped)-1], maxIgnoredKeys+1)
	}
	for _, k := range req.Dropped {
		if len([]rune(k)) > 65 {
			t.Fatalf("a dropped key was not clipped: %d runes", len([]rune(k)))
		}
	}
	if !strings.Contains(obs.Content, "[ignored: ") || !strings.Contains(obs.Content, "file_path") {
		t.Fatalf("the result does not tell the model what was ignored: %q", obs.Content)
	}
	sent := l.Adapter.(*scriptedAdapter).gotRequests[1].Messages
	if last := sent[len(sent)-1]; !strings.Contains(last.Content, "ignored: ") {
		t.Fatalf("the model was not told: %q", last.Content)
	}
}
