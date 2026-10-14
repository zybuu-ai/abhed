package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	cp "github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
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

// checkingDriver is a driver that says up front it cannot apply a size.
type checkingDriver struct {
	*drivertest.Fake
}

func (checkingDriver) CheckConfig(cfg map[string]string) error {
	if cfg["memory_mb"] != "" {
		return driver.Errorf(driver.ErrInvalid, "no cgroups here")
	}
	return nil
}

// A size the driver says it cannot apply refuses the create at once, with
// its reason, instead of leaving a sandbox to fail when it is built.
func TestCreateRefusesASizeTheDriverCannotApply(t *testing.T) {
	ctx := context.Background()
	st, err := cp.NewSettings(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := checkingDriver{drivertest.New("process")}
	reg := driver.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	mem := cp.NewMemory()
	ws := t.TempDir()
	svc, err := cp.New(cp.Config{Store: mem, Audit: mem, Drivers: reg, Settings: st,
		Workspace: func(string) (string, error) { return ws, nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Create(ctx, alice, cp.CreateRequest{Template: "t", Settings: map[string]string{cp.SetMemoryMB: "512"}})
	if !errors.Is(err, cp.ErrInvalid) || !strings.Contains(err.Error(), "no cgroups here") {
		t.Fatalf("a size the driver cannot apply: %v", err)
	}
	if items, _, _ := svc.List(ctx, alice, cp.ListOptions{}); len(items) != 0 {
		t.Fatalf("the refused create made %d sandboxes", len(items))
	}
	if _, err := svc.Create(ctx, alice, cp.CreateRequest{Template: "t", Settings: map[string]string{cp.SetCPUs: "1"}}); err != nil {
		t.Fatalf("a size it can apply: %v", err)
	}
}
