package controlplane_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	cp "github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
)

// newCapRig is newRig with a fake declaring caps.
func newCapRig(t *testing.T, values map[string]string, caps ...driver.Capability) *rig {
	t.Helper()
	return newCapRigWith(t, values, func(*cp.Config) {}, caps...)
}

// newCapRigWith is newCapRig with the service's configuration adjusted.
func newCapRigWith(t *testing.T, values map[string]string, adjust func(*cp.Config), caps ...driver.Capability) *rig {
	t.Helper()
	st, err := cp.NewSettings(values, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := drivertest.New("process", caps...)
	reg := driver.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	mem := cp.NewMemory()
	clk := &clock{t: t0}
	ws := t.TempDir()
	c := cp.Config{Store: mem, Audit: mem, Drivers: reg, Settings: st,
		Workspace: func(string) (string, error) { return ws, nil }, Now: clk.now}
	adjust(&c)
	svc, err := cp.New(c)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{svc: svc, store: mem, fake: fake, clock: clk}
}

func actions(t *testing.T, r *rig, id string) []string {
	t.Helper()
	evs, err := r.store.AuditFor(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Action+":"+ev.Outcome)
	}
	return out
}

func TestVerbsWithoutTheCapabilityAreUnsupportedAndUnrecorded(t *testing.T) {
	r := newCapRig(t, nil)
	sb := r.ready(t)
	before := len(actions(t, r, sb.ID))
	ctx := context.Background()
	if _, _, err := r.svc.Snapshot(ctx, alice, sb.ID, "", "x"); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := r.svc.CopyIn(ctx, alice, sb.ID, "f", strings.NewReader("")); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("copy in: %v", err)
	}
	if _, err := r.svc.CopyOut(ctx, alice, sb.ID, "f", &bytes.Buffer{}, nil); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("copy out: %v", err)
	}
	called := false
	if _, err := r.svc.UseDriver(ctx, alice, sb.ID, "expose", driver.CapForward, nil, func(driver.Driver, cp.Sandbox) error {
		called = true
		return nil
	}); !errors.Is(err, driver.ErrUnsupported) || called {
		t.Fatalf("use: %v %v", err, called)
	}
	if got := actions(t, r, sb.ID); len(got) != before {
		t.Fatalf("an unsupported verb was recorded: %v", got[before:])
	}
	// Someone else's sandbox is not found, whatever the driver can do.
	if _, _, err := r.svc.Snapshot(ctx, cp.Caller{Principal: "bob"}, sb.ID, "", "x"); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("bob: %v", err)
	}
}

func TestSnapshotAndRestore(t *testing.T) {
	r := newCapRig(t, nil, driver.CapSnapshot)
	sb := r.ready(t)
	ctx := context.Background()
	ref, _, err := r.svc.Snapshot(ctx, alice, sb.ID, "", "before")
	if err != nil {
		t.Fatal(err)
	}
	ref.Digest = "sha256:00"
	// Only a sandbox whose instance is not running is restored.
	if _, err := r.svc.Restore(ctx, alice, sb.ID, "", ref); !errors.Is(err, cp.ErrInvalidTransition) {
		t.Fatalf("restore while ready: %v", err)
	}
	if _, err := r.svc.Stop(ctx, alice, sb.ID, "", 0); err != nil {
		t.Fatal(err)
	}
	stopped := r.reconcile(t, sb.ID)
	if _, err := r.svc.Restore(ctx, alice, sb.ID, "", driver.SnapshotRef{ID: ref.ID}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("a ref without its digest: %v", err)
	}
	out, err := r.svc.Restore(ctx, alice, sb.ID, cp.ETag(stopped.ResourceVersion), ref)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != cp.StateStarting || out.Generation != stopped.Generation+1 {
		t.Fatalf("after restore: %s generation %d", out.State, out.Generation)
	}
	if r.reconcile(t, sb.ID).State != cp.StateReady {
		t.Fatal("the restored sandbox did not start")
	}
	got := strings.Join(actions(t, r, sb.ID), " ")
	for _, want := range []string{"snapshot:requested snapshot:ok", "restore:requested restore:ok"} {
		if !strings.Contains(got, want) {
			t.Errorf("record lacks %q: %s", want, got)
		}
	}
	// A restore the driver refuses changes nothing.
	if _, err := r.svc.Stop(ctx, alice, sb.ID, "", 0); err != nil {
		t.Fatal(err)
	}
	before := r.reconcile(t, sb.ID)
	if _, err := r.svc.Restore(ctx, alice, sb.ID, "", driver.SnapshotRef{ID: "nope", Digest: "sha256:00"}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("an unknown snapshot: %v", err)
	}
	if after, _ := r.store.Get(ctx, sb.ID); after.State != cp.StateStopped || after.Generation != before.Generation {
		t.Fatalf("a refused restore moved the sandbox: %s %d", after.State, after.Generation)
	}
}

func TestCopyInAndOutAreBounded(t *testing.T) {
	r := newCapRig(t, map[string]string{cp.SetExecOutput: "1024"}, driver.CapCopy)
	sb := r.ready(t)
	ctx := context.Background()
	if _, err := r.svc.CopyIn(ctx, alice, sb.ID, "small", strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	started := false
	if _, err := r.svc.CopyOut(ctx, alice, sb.ID, "small", &out, func(cp.Sandbox) { started = true }); err != nil || out.String() != "abc" || !started {
		t.Fatalf("copy out: %q %v %v", out.String(), err, started)
	}
	if _, err := r.svc.CopyIn(ctx, alice, sb.ID, "big", strings.NewReader(strings.Repeat("x", 2048))); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if _, err := r.svc.CopyOut(ctx, alice, sb.ID, "big", &out, nil); !errors.Is(err, driver.ErrOutputLimit) || out.Len() > 1024 {
		t.Fatalf("an oversized copy: %d bytes, %v", out.Len(), err)
	}
	got := strings.Join(actions(t, r, sb.ID), " ")
	if strings.Count(got, "cp:requested") != 4 || !strings.Contains(got, "cp:failed") {
		t.Fatalf("record: %s", got)
	}
}

func TestDriverRecorderKeepsSandboxAndGeneration(t *testing.T) {
	mem := cp.NewMemory()
	rec := cp.DriverRecorder(mem, nil)
	if err := rec(driver.EventSnapshot, map[string]any{"sandbox_id": "sbx_0123456789abcdef", "generation": int64(3), "bytes": 10}); err != nil {
		t.Fatal(err)
	}
	evs, _ := mem.AuditFor(context.Background(), "sbx_0123456789abcdef")
	if len(evs) != 1 || evs[0].Generation != 3 || evs[0].Action != driver.EventSnapshot || evs[0].Detail["bytes"] != "10" ||
		evs[0].Principal != cp.DriverPrincipal {
		t.Fatalf("%+v", evs)
	}
}

// A guard or interceptor that refuses an exec is not gone round by a copy,
// a snapshot or an exposure, and a restore is admitted as the start it is.
func TestVerbsAreAdmittedAsWhatTheyAmountTo(t *testing.T) {
	all := []driver.Capability{driver.CapCopy, driver.CapSnapshot, driver.CapForward}
	g := &guardFunc{admit: func(a *cp.Admission) (func(), error) {
		if a.Action == cp.ActionExec {
			return nil, cp.ErrForbidden
		}
		return nil, nil
	}}
	r := newCapRig(t, nil, all...)
	sb := r.ready(t)
	if err := r.svc.SetGuard(g); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := r.svc.Snapshot(ctx, alice, sb.ID, "", "x"); !errors.Is(err, cp.ErrForbidden) {
		t.Fatalf("snapshot past a guard refusing exec: %v", err)
	}
	if _, err := r.svc.CopyIn(ctx, alice, sb.ID, "f", strings.NewReader("")); !errors.Is(err, cp.ErrForbidden) {
		t.Fatalf("copy in: %v", err)
	}
	if _, err := r.svc.CopyOut(ctx, alice, sb.ID, "f", &bytes.Buffer{}, nil); !errors.Is(err, cp.ErrForbidden) {
		t.Fatalf("copy out: %v", err)
	}
	called := false
	if _, err := r.svc.UseDriver(ctx, alice, sb.ID, "expose", driver.CapForward, map[string]string{"port": "80"},
		func(driver.Driver, cp.Sandbox) error { called = true; return nil }); !errors.Is(err, cp.ErrForbidden) || called {
		t.Fatalf("expose: %v %v", err, called)
	}
	got := strings.Join(actions(t, r, sb.ID), " ")
	if !strings.Contains(got, "cp:failed") || !strings.Contains(got, "snapshot:failed") {
		t.Fatalf("refusals not recorded: %s", got)
	}

	// The interceptor sees them too, and a restore as a start.
	h := &hook{}
	ri := newCapRigWith(t, nil, func(c *cp.Config) { c.Interceptor = h }, all...)
	sb = ri.ready(t)
	ref, _, err := ri.svc.Snapshot(ctx, alice, sb.ID, "", "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ri.svc.Stop(ctx, alice, sb.ID, "", 0); err != nil {
		t.Fatal(err)
	}
	ri.reconcile(t, sb.ID)
	ref.Digest = "sha256:00"
	if _, err := ri.svc.Restore(ctx, alice, sb.ID, "", ref); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	var seen []string
	for _, req := range h.seen {
		seen = append(seen, req.Operation+" "+req.Command)
	}
	h.mu.Unlock()
	joined := strings.Join(seen, "|")
	// The command is redacted for the record, here to its digest.
	if !strings.Contains(joined, "|exec sha256:") || !strings.Contains(joined, "|start ") {
		t.Fatalf("interceptor saw %q", joined)
	}

	// A replica that does not own the sandbox acts on it in no way.
	o := &owner{}
	ro := newCapRigWith(t, nil, func(c *cp.Config) { c.Ownership = o }, all...)
	o.set(true, nil)
	sb = ro.ready(t)
	o.set(false, nil)
	if _, _, err := ro.svc.Snapshot(ctx, alice, sb.ID, "", "x"); !errors.Is(err, cp.ErrUnavailable) {
		t.Fatalf("snapshot on another replica's sandbox: %v", err)
	}
}
