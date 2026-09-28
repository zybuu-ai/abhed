package abhed

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// SetModel records the switch, which OnEvent delivers, and the prompt names
// the new model; a switch the record refuses fails and leaves the model as it was.
func TestSetModelIsRecordedAndARefusedOneIsNotMade(t *testing.T) {
	var mu sync.Mutex
	var switched []ModelSwitched
	a := forwardingAgent(t, func(ev Event) {
		if ev.Type == EvModelSwitched {
			var m ModelSwitched
			_ = json.Unmarshal(ev.Payload, &m)
			mu.Lock()
			switched = append(switched, m)
			mu.Unlock()
		}
	})
	if err := a.SetModel(Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m2", ContextWindow: 4096}); err != nil {
		t.Fatal(err)
	}
	if err := flushWithin(t, a, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]ModelSwitched(nil), switched...)
	mu.Unlock()
	if len(got) != 1 || got[0].Model != "m2" || got[0].From != "m" {
		t.Fatalf("OnEvent saw the switches %+v", got)
	}
	if !strings.Contains(a.loop.Config.SystemPrompt, "Model: m2 · Context window: 4096 tokens") {
		t.Fatalf("the prompt does not name the new model:\n%s", a.loop.Config.SystemPrompt)
	}

	a.loop.Recorder.Gate = func() error { return errors.New("the record refused") }
	if err := a.SetModel(Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m3"}); err == nil {
		t.Fatal("a switch the record refused reported success")
	}
	if name := a.loop.Adapter.Profile().Name; name != "m2" {
		t.Fatalf("a refused switch left the agent on %s", name)
	}
}
