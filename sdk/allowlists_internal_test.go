package abhed

import (
	"context"
	"errors"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// The managed allowed models hold in the SDK: only admitted models are
// listed, a switch to another is refused with the reason and recorded, and
// an endpoint outside the list is refused mid-session and at New.
func TestAllowedModelsHoldInTheSDK(t *testing.T) {
	a1, b1 := newModelStub(t, "a", false), newModelStub(t, "b", false)
	modelsEnv(t, `{"model":{"default":"alpha","providers":{"alpha":`+provider(a1.URL, "m-a")+`,"beta":`+provider(b1.URL, "m-b")+`}}}`,
		`{"model":{"allowed":["openai-compatible/m-a"]}}`)
	a := newModelsAgent(t, "")
	if ms := a.Models(); len(ms) != 1 || ms[0].Name != "alpha" {
		t.Fatalf("models: %+v", ms)
	}
	err := a.SwitchModelNamed("beta")
	var refused *config.ManagedError
	if !errors.Is(err, ErrUnknownModel) || !errors.As(err, &refused) || refused.Key != "model.allowed" {
		t.Fatalf("switch: %v", err)
	}
	recorded := false
	for _, ev := range a.Events() {
		if ev.Type == agent.EvConfigRefused {
			recorded = true
		}
		if ev.Type == EvModelSwitched {
			t.Fatal("a refused switch was made")
		}
	}
	if !recorded {
		t.Fatal("the refusal was not recorded")
	}
	if err := a.SetModel(Provider{Type: "openai-compatible", BaseURL: b1.URL, Model: "m-b", ContextWindow: 8192}); !errors.As(err, &refused) {
		t.Fatalf("SetModel outside the list: %v", err)
	}
	if err := a.SetModel(Provider{Type: "openai-compatible", BaseURL: a1.URL, Model: "m-a", ContextWindow: 8192}); err != nil {
		t.Fatalf("SetModel in the list: %v", err)
	}
	ws := t.TempDir()
	if _, err := New(context.Background(), Options{Workspace: ws, ConfigDir: ws,
		Provider: &Provider{Type: "openai-compatible", BaseURL: b1.URL, Model: "m-b", ContextWindow: 8192}}); !errors.As(err, &refused) {
		t.Fatalf("Options.Provider outside the list: %v", err)
	}
}
