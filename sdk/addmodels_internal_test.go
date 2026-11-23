package abhed

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// An open conversation takes only the providers added, as their configuration
// admits them: one a managed pin refuses is not added, and none it has is
// replaced.
func TestAddModels(t *testing.T) {
	a := &Agent{cfg: config.Default()}
	a.cfg.Model.Default = "a"
	a.cfg.Model.Providers = map[string]config.ProviderConfig{"a": {Type: "openai-compatible", Model: "m"}}
	before := a.cfg.Model.Providers
	from := config.Default()
	from.Model.Default = "a"
	from.Model.Providers = map[string]config.ProviderConfig{
		"a": {Type: "openai-compatible", Model: "changed"},
		"b": {Type: "openai-compatible", Model: "n"},
	}
	if got := strings.Join(a.AddModels(from), ","); got != "b" {
		t.Fatalf("added %q", got)
	}
	if a.cfg.Model.Providers["a"].Model != "m" || len(before) != 1 {
		t.Fatal("an existing provider was replaced, or the old map written under")
	}
	pinned := from
	pinned.Model.Providers = map[string]config.ProviderConfig{"c": {Type: "openai-compatible", Model: "o"}}
	pinned.ManagedKeys = []string{"model.default"}
	pinned.Managed = true
	if got := a.AddModels(pinned); len(got) != 0 {
		t.Fatalf("a model the managed pin refuses was added: %v", got)
	}
}
