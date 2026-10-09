package controlplane_test

import (
	"context"
	"errors"
	"testing"

	cp "github.com/zybuu-ai/abhed/controlplane"
)

// A sandbox's size reaches the driver as the keys drivers read; an unset
// size passes nothing, and a malformed or looser one is refused.
func TestResourceSettings(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, map[string]string{cp.SetCPUs: "2"}, map[string]bool{cp.SetCPUs: true})
	sb, err := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "default", Settings: map[string]string{
		cp.SetCPUs: "0.5", cp.SetMemoryMB: "512"}})
	if err != nil {
		t.Fatal(err)
	}
	r.reconcile(t, sb.ID)
	spec, _ := r.fake.Spec(sb.ID)
	if spec.Config["cpus"] != "0.5" || spec.Config["memory_mb"] != "512" || len(spec.Config) != 2 {
		t.Fatalf("spec config %v", spec.Config)
	}
	for _, bad := range []map[string]string{{cp.SetCPUs: "4"}, {cp.SetCPUs: "0.001"}, {cp.SetCPUs: "1e2"},
		{cp.SetMemoryMB: "8"}, {cp.SetMemoryMB: "lots"}} {
		if _, err := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "default", Settings: bad}); !errors.Is(err, cp.ErrInvalid) {
			t.Fatalf("%v accepted: %v", bad, err)
		}
	}

	free := newRig(t, nil, nil)
	sb = free.ready(t)
	if spec, _ := free.fake.Spec(sb.ID); spec.Config != nil {
		t.Fatalf("an unsized sandbox passed %v", spec.Config)
	}
	if m, ok := cp.MilliCPUs("1.25"); !ok || m != 1250 {
		t.Fatalf("MilliCPUs(1.25) = %d %v", m, ok)
	}
}
