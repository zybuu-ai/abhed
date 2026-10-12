package controlplane_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cp "github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
)

// specDriver keeps the spec each instance was created with.
type specDriver struct {
	*drivertest.Fake
	mu    sync.Mutex
	specs map[string]driver.Spec
}

func (d *specDriver) Create(ctx context.Context, spec driver.Spec) (driver.Status, error) {
	d.mu.Lock()
	d.specs[spec.ID] = spec
	d.mu.Unlock()
	return d.Fake.Create(ctx, spec)
}

// fakeEgress is an egress proxy for the "vm" driver only.
type fakeEgress struct {
	mu           sync.Mutex
	open, closed []string
	calls, ended int
	openErr      error
}

func (e *fakeEgress) For(name string) bool { return name == "vm" }

func (e *fakeEgress) Open(id string) (driver.Egress, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.openErr != nil {
		return driver.Egress{}, e.openErr
	}
	e.open = append(e.open, id)
	return driver.Egress{Socket: "/run/egress/" + id + ".sock", Port: 3128}, nil
}

func (e *fakeEgress) Call(string) ([]string, func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	return []string{"HTTP_PROXY=http://call:token@127.0.0.1:3128"}, func() {
		e.mu.Lock()
		e.ended++
		e.mu.Unlock()
	}, nil
}

func (e *fakeEgress) Close(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = append(e.closed, id)
	return nil
}

func newEgressRig(t *testing.T, eg *fakeEgress) (*rig, *specDriver, *specDriver) {
	t.Helper()
	st, _ := cp.NewSettings(nil, nil)
	proc := &specDriver{Fake: drivertest.New("process"), specs: map[string]driver.Spec{}}
	vm := &specDriver{Fake: drivertest.New("vm"), specs: map[string]driver.Spec{}}
	reg := driver.NewRegistry()
	for _, d := range []driver.Driver{proc, vm} {
		if err := reg.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	mem := cp.NewMemory()
	ws := t.TempDir()
	svc, err := cp.New(cp.Config{Store: mem, Audit: mem, Drivers: reg, Settings: st,
		Workspace: func(string) (string, error) { return ws, nil }, Now: (&clock{t: t0}).now,
		WriteProtected: []string{"protected.txt"}, Egress: eg})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{svc: svc, store: mem, fake: proc.Fake}, proc, vm
}

func TestSpecCarriesWriteProtectedAndEgressForItsDrivers(t *testing.T) {
	eg := &fakeEgress{}
	r, proc, vm := newEgressRig(t, eg)
	ctx := context.Background()
	var gotEnv []string
	vm.ExecFunc = func(_ context.Context, _ string, req driver.ExecRequest) (driver.ExecResult, error) {
		gotEnv = req.Env
		return driver.ExecResult{}, nil
	}

	a, _ := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "default"})
	b, _ := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "default", Driver: "vm"})
	r.reconcile(t, a.ID)
	if got := r.reconcile(t, b.ID); got.State != cp.StateReady {
		t.Fatalf("vm sandbox: %s", got.State)
	}
	if s := proc.specs[a.ID]; s.Egress != nil || !slices.Equal(s.WriteProtected, []string{"protected.txt"}) {
		t.Fatalf("process spec: %+v", s)
	}
	if s := vm.specs[b.ID]; s.Egress == nil || s.Egress.Port != 3128 || !slices.Equal(s.WriteProtected, []string{"protected.txt"}) {
		t.Fatalf("vm spec: %+v", s)
	}

	// Each of its commands carries a credential of its own, ended after it.
	if _, _, err := r.svc.Exec(ctx, alice, b.ID, "", cp.ExecRequest{Command: "curl x", Env: []string{"A=1"}}, cp.ExecIO{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gotEnv, []string{"A=1", "HTTP_PROXY=http://call:token@127.0.0.1:3128"}) || eg.calls != 1 || eg.ended != 1 {
		t.Fatalf("env %v, calls %d, ended %d", gotEnv, eg.calls, eg.ended)
	}
	// A client cannot point the command at another proxy.
	if _, _, err := r.svc.Exec(ctx, alice, b.ID, "", cp.ExecRequest{Command: "curl x", Env: []string{"https_proxy=http://evil"}}, cp.ExecIO{}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("a client's proxy variable: %v", err)
	}
	// The process driver's commands are left to its own proxy.
	if _, _, err := r.svc.Exec(ctx, alice, a.ID, "", cp.ExecRequest{Command: "true"}, cp.ExecIO{}); err != nil || eg.calls != 1 {
		t.Fatalf("process exec: %v, calls %d", err, eg.calls)
	}

	if _, err := r.svc.Delete(ctx, alice, b.ID, ""); err != nil {
		t.Fatal(err)
	}
	_ = r.svc.Reconcile(ctx, b.ID)
	if !slices.Equal(eg.closed, []string{b.ID}) {
		t.Fatalf("closed %v", eg.closed)
	}
}

func TestNoEgressProxyNoSandbox(t *testing.T) {
	eg := &fakeEgress{openErr: errors.New("no socket")}
	r, _, vm := newEgressRig(t, eg)
	sb, _ := r.svc.Create(context.Background(), alice, cp.CreateRequest{Template: "default", Driver: "vm"})
	got := r.reconcile(t, sb.ID)
	if got.State != cp.StateError || vm.Calls("create") != 0 {
		t.Fatalf("built without its proxy: %s, creates %d", got.State, vm.Calls("create"))
	}
	if c, _ := got.Condition(cp.CondDriverReady); !strings.Contains(c.Message, "egress proxy") {
		t.Fatalf("condition %+v", c)
	}
}

// A microvm sandbox under the default sandbox.min_tier (process) is Ready,
// not stopped as weaker for reporting a tier the control plane did not know.
func TestMicroVMTierMeetsTheFloor(t *testing.T) {
	st, err := cp.NewSettings(map[string]string{cp.SetMinTier: "process"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	vm := drivertest.New("microvm")
	vm.Tier = cp.TierMicroVM
	reg := driver.NewRegistry()
	_ = reg.Register(drivertest.New("process"))
	if err := reg.Register(vm); err != nil {
		t.Fatal(err)
	}
	mem := cp.NewMemory()
	ws := t.TempDir()
	svc, err := cp.New(cp.Config{Store: mem, Audit: mem, Drivers: reg, Settings: st,
		Workspace: func(string) (string, error) { return ws, nil }, Now: (&clock{t: t0}).now})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{svc: svc, store: mem}
	ctx := context.Background()
	sb, err := svc.Create(ctx, alice, cp.CreateRequest{Template: "default", Driver: "microvm",
		Settings: map[string]string{cp.SetMinTier: "microvm"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.reconcile(t, sb.ID); got.State != cp.StateReady || got.Tier != cp.TierMicroVM {
		t.Fatalf("microvm sandbox: %s %s", got.State, got.Tier)
	}
	p, _ := svc.Create(ctx, alice, cp.CreateRequest{Template: "default", Settings: map[string]string{cp.SetMinTier: "microvm"}})
	if got := r.reconcile(t, p.ID); got.State != cp.StateError {
		t.Fatalf("a process sandbox asking for microvm: %s", got.State)
	}
}

// A session's command on a driver that takes its egress from the control
// plane carries a credential of its own, forwarded by name and ended when
// the command's context ends.
func TestCommandCarriesItsEgressCredential(t *testing.T) {
	eg := &fakeEgress{}
	ctx := context.Background()
	st, _ := cp.NewSettings(nil, nil)
	vm := &specDriver{Fake: drivertest.New("vm", driver.CapCommand), specs: map[string]driver.Spec{}}
	reg := driver.NewRegistry()
	if err := reg.Register(vm); err != nil {
		t.Fatal(err)
	}
	mem := cp.NewMemory()
	ws := t.TempDir()
	svc, err := cp.New(cp.Config{Store: mem, Audit: mem, Drivers: reg, Settings: st,
		Workspace: func(string) (string, error) { return ws, nil }, Egress: eg})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := svc.Create(ctx, alice, cp.CreateRequest{Template: "default", Driver: "vm"})
	if err != nil {
		t.Fatal(err)
	}
	if sb, err = svc.WaitReady(ctx, alice, sb.ID); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cmd, _, err := svc.Command(cctx, alice, sb.ID, driver.CommandRequest{Command: "curl x"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Env, "HTTP_PROXY=http://call:token@127.0.0.1:3128") || eg.calls != 1 {
		t.Fatalf("env %v, calls %d", cmd.Env, eg.calls)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		eg.mu.Lock()
		ended := eg.ended
		eg.mu.Unlock()
		if ended == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the credential outlived the command")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
