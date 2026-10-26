package sandbox

import (
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// The optional proxy never costs a tier: Select picks the tier it would
// pick without it, holding the proxy only where that tier can.
func TestOptionalEgressNeverTradesATier(t *testing.T) {
	pol, err := egress.Compile(egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy(t.TempDir())
	p.MinTier = TierNone
	plain, err := Select(p)
	if err != nil {
		t.Fatal(err)
	}
	defer Close(plain)
	p.OptionalEgress = pol
	with, err := Select(p)
	if err != nil {
		t.Fatal(err)
	}
	defer Close(with)
	if with.Tier() != plain.Tier() {
		t.Fatalf("the optional proxy moved the tier from %s to %s", plain.Tier(), with.Tier())
	}
	if got := EgressOf(with); got != nil && got != pol {
		t.Fatal("another policy in force")
	}
	if with.Tier() == TierNone && EgressOf(with) != nil {
		t.Fatal("the none tier holds no proxy")
	}
	if EgressOf(plain) != nil {
		t.Fatal("a proxy without one asked for")
	}
}
