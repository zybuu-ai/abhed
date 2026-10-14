package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A replayed fork shows as a divider naming the step it went on from.
func TestRenderMarksAFork(t *testing.T) {
	var out bytes.Buffer
	raw, _ := json.Marshal(agent.Forked{ThroughSeq: 7})
	NewRenderer(&out, false).Event(agent.Event{Seq: 9, Type: agent.EvForked, Payload: raw})
	if !strings.Contains(out.String(), "forked at step 7") {
		t.Fatalf("no fork divider: %q", out.String())
	}
}

// A fork through step 0, as resending the first message makes, went on from
// before the first message, and says so rather than naming a step 0.
func TestRenderMarksAForkBeforeTheFirstMessage(t *testing.T) {
	var out bytes.Buffer
	raw, _ := json.Marshal(agent.Forked{ThroughSeq: 0})
	NewRenderer(&out, false).Event(agent.Event{Seq: 3, Type: agent.EvForked, Payload: raw})
	if !strings.Contains(out.String(), "forked before the first message") || strings.Contains(out.String(), "step 0") {
		t.Fatalf("fork divider: %q", out.String())
	}
}
