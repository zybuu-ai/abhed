package toolset

import (
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// Only an ask a managed ask rule decided is kept from a runbook.
func TestKeepManagedAsks(t *testing.T) {
	keep := KeepManagedAsks(config.Config{})
	if keep(policy.Result{Decision: policy.Ask, Step: "ask", Rule: "ssh(*)"}) {
		t.Fatal("a rule loading never saw was taken as the managed file's")
	}
	if keep(policy.Result{Decision: policy.Ask, Step: "default"}) {
		t.Fatal("a default ask was kept")
	}
}
