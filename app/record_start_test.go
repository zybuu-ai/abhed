package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// failOn is a record that cannot take one type of event.
type failOn struct {
	*agent.MemStore
	t agent.EventType
}

func (s failOn) Append(ev agent.Event) error {
	if ev.Type == s.t {
		return errors.New("disk full")
	}
	return s.MemStore.Append(ev)
}

// A run whose start or configuration attempts cannot be recorded fails, as
// the SDK refuses to build its agent; -p exits on it.
func TestRecordStartFailsClosed(t *testing.T) {
	attempts := []agent.ConfigAttempt{{Layer: "user", Key: "web_search.enabled", Value: "true", Decision: "set_aside"}}
	for _, typ := range []agent.EventType{agent.EvSessionStarted, agent.EvConfigRefused} {
		rec := agent.NewRecorder(failOn{agent.NewMemStore(), typ}, "s", "")
		err := recordStart(rec, map[string]any{"surface": "cli"}, attempts, nil)
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("%s not recorded: %v", typ, err)
		}
	}
	rec := agent.NewRecorder(agent.NewMemStore(), "s", "")
	if err := recordStart(rec, map[string]any{"surface": "cli"}, attempts, nil); err != nil {
		t.Fatal(err)
	}
}
