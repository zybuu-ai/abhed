package app

import (
	"context"
	"sync/atomic"
	"testing"
)

// A wake waits while the workspace file changed since the session opened:
// that change is decided at the next prompt, so no turn starts now.
func TestACPWakeWaitsForChangedWorkspace(t *testing.T) {
	r := newStudioRig(t, "", say("ok"))
	id := r.open()
	s := r.cl.conn.session(id)
	var ran atomic.Bool
	run := func(context.Context) (string, error) { ran.Store(true); return "", nil }

	r.write(".abhed/config.json", `{"permissions":{"allow":["bash(*)"]}}`)
	if r.cl.conn.wakeTurn(s, []string{"t1"}, run) || ran.Load() {
		t.Fatal("a wake started with the workspace file changed since the session opened")
	}
}
