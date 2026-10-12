package sandbox

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// The fence holds the allowlist in mount_namespace mode, where each command
// has a network namespace with only the relay in it; in landlock_only only
// the managed fence.egress_port_only lets it, by the proxy's port alone.
func TestFenceEgressNeedsANetworkNamespaceOrConsent(t *testing.T) {
	pol, err := egress.Compile(egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	p := fencePolicy(t)
	p.Egress = pol
	f := NewFence(p)
	if why := f.egressModeRefusal(); !strings.Contains(why, "egress_port_only") {
		t.Fatalf("landlock_only took the allowlist: %q", why)
	}
	f.mounts = true
	if why := f.egressModeRefusal(); why != "" {
		t.Fatalf("mount_namespace refused it: %s", why)
	}
	p.FenceEgressPortOnly = true
	if why := NewFence(p).egressModeRefusal(); why != "" {
		t.Fatalf("port-only consent refused: %s", why)
	}
}
