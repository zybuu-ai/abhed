package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// A turn that spent the whole output budget without a tool call is not an
// answer, even when a sentence came out before the cut: the model is nudged,
// the next call asks for low effort, and the record marks the turn.
func TestOutputCappedTurnIsAStallNotAnAnswer(t *testing.T) {
	capped := &model.Usage{InputTokens: 20000, OutputTokens: DefaultConfig().MaxTokens}
	l, store, _ := harness(t, []scriptedTurn{
		{text: "I will examine the code in similar.py to identify", usage: capped},
		{text: "The fix is one line; done.", usage: &model.Usage{InputTokens: 100, OutputTokens: 40}},
	}, policy.ModeDefault, true)

	reason, _ := l.Run(context.Background(), "fix the duplicate-code check")
	if reason != TermCompleted {
		t.Fatalf("ended with %s", reason)
	}
	adapter := l.Adapter.(*scriptedAdapter)
	if len(adapter.gotRequests) != 2 {
		t.Fatalf("%d model calls; the capped turn must be retried once", len(adapter.gotRequests))
	}
	second := adapter.gotRequests[1]
	if second.Effort != model.EffortLow {
		t.Fatalf("effort after the cut-off is %q, want low", second.Effort)
	}
	last := second.Messages[len(second.Messages)-1]
	if last.Role != model.RoleUser || !strings.Contains(last.Content, "cut off") {
		t.Fatalf("the model was not told its reply was cut off: %+v", last)
	}
	evs, _ := store.Events("sess1")
	marked := false
	for _, e := range evs {
		if e.Type == EvModelCall {
			var mc ModelCall
			_ = json.Unmarshal(e.Payload, &mc)
			marked = marked || (mc.CutOff && mc.ToolCalls == 0)
		}
	}
	if !marked {
		t.Fatal("no model.call in the record is marked cut_off")
	}
}

// Three capped turns in a row end the session as stalled, not completed.
func TestRepeatedlyCappedTurnsStall(t *testing.T) {
	capped := &model.Usage{InputTokens: 20000, OutputTokens: DefaultConfig().MaxTokens}
	l, _, _ := harness(t, []scriptedTurn{
		{text: "Looking at", usage: capped}, {text: "Looking at", usage: capped}, {text: "Looking at", usage: capped},
	}, policy.ModeDefault, true)
	if reason, _ := l.Run(context.Background(), "fix it"); reason != TermStalled {
		t.Fatalf("ended with %s, want stalled", reason)
	}
}
