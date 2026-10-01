package abhed

import "testing"

// Once Close has begun, the agent refuses to host a wake run, even before its
// background work is closed.
func TestCanWakeRefusedOnceClosing(t *testing.T) {
	a := &Agent{}
	if ok, why := a.canWake(); !ok || why != "" {
		t.Fatalf("an open agent: %v %q", ok, why)
	}
	a.endWakes()
	if ok, why := a.canWake(); ok || why != "closed" {
		t.Fatalf("a closing agent: %v %q", ok, why)
	}
}
