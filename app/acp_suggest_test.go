package app

import (
	"slices"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A completed turn sends the editor a next prompt after the reply that ends
// the turn, under the engine's _meta key, and initialize lists the feature.
func TestStudioSuggestionAtTurnEnd(t *testing.T) {
	r := newStudioRig(t, "", say("The tests pass."))
	r.model.mu.Lock()
	r.model.suggestion = "Commit\u202e the change"
	r.model.mu.Unlock()
	id := r.open()
	from := r.cl.mark()
	stop, _ := r.prompt(id, "run the tests")
	if stop != "end_turn" {
		t.Fatalf("stop %q", stop)
	}
	var got []string
	for deadline := time.Now().Add(10 * time.Second); len(got) == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, u := range updates(r.cl.since(from)) {
			meta, _ := u["_meta"].(map[string]any)
			ours, _ := meta[acpMetaKey].(map[string]any)
			if s, ok := ours["suggestion"].(string); ok {
				got = append(got, s)
				if u["sessionUpdate"] != "agent_message_chunk" || u["content"].(map[string]any)["text"] != "" {
					t.Fatalf("the suggestion rides an empty message chunk: %v", u)
				}
			}
		}
	}
	if len(got) != 1 || got[0] != "Commit the change" {
		t.Fatalf("suggestions %q, want one, cleaned", got)
	}
	if p, _ := r.recorded(id, agent.EvSuggestionOffered); len(p) != 1 {
		t.Fatalf("recorded %v", p)
	}
	if !slices.Contains(r.cl.conn.acpFeatures(), "suggestions") {
		t.Fatal("features do not list suggestions")
	}
}

// suggest.enabled false in the configuration sends none.
func TestStudioNoSuggestionWhenDisabled(t *testing.T) {
	r := newStudioRig(t, `,"suggest":{"enabled":false}`, say("ok"))
	r.model.mu.Lock()
	r.model.suggestion = "Never sent"
	r.model.mu.Unlock()
	id := r.open()
	_, ups := r.prompt(id, "hi")
	for _, u := range ups {
		if meta, ok := u["_meta"].(map[string]any); ok {
			if ours, ok := meta[acpMetaKey].(map[string]any); ok && ours["suggestion"] != nil {
				t.Fatalf("a suggestion was sent with suggest.enabled false: %v", u)
			}
		}
	}
	if p, _ := r.recorded(id, agent.EvSuggestionOffered); len(p) != 0 {
		t.Fatalf("recorded %v", p)
	}
}
