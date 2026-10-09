package controlplane_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	cp "github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type rig struct {
	svc   *cp.Service
	store *cp.Memory
	fake  *drivertest.Fake
	clock *clock
}

var alice = cp.Caller{Principal: "alice"}

func newRig(t *testing.T, values map[string]string, ceilings map[string]bool) *rig {
	t.Helper()
	st, err := cp.NewSettings(values, ceilings)
	if err != nil {
		t.Fatal(err)
	}
	fake := drivertest.New("process", driver.CapAttach)
	reg := driver.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	mem := cp.NewMemory()
	clk := &clock{t: t0}
	ws := t.TempDir()
	svc, err := cp.New(cp.Config{Store: mem, Audit: mem, Drivers: reg, Settings: st,
		Workspace: func(string) (string, error) { return ws, nil }, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{svc: svc, store: mem, fake: fake, clock: clk}
}

func (r *rig) reconcile(t *testing.T, id string) cp.Sandbox {
	t.Helper()
	_ = r.svc.Reconcile(context.Background(), id)
	sb, err := r.store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

func (r *rig) ready(t *testing.T, settings map[string]string) cp.Sandbox {
	t.Helper()
	sb, err := r.svc.Create(context.Background(), alice, cp.CreateRequest{Template: "default", Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != cp.StateProvisioning || sb.Generation != 1 {
		t.Fatalf("created %+v", sb)
	}
	sb = r.reconcile(t, sb.ID)
	if sb.State != cp.StateReady {
		t.Fatalf("not ready after a pass: %+v", sb)
	}
	return sb
}

func TestLifecycleThroughTheReconciler(t *testing.T) {
	r := newRig(t, nil, nil)
	ctx := context.Background()
	sb := r.ready(t, nil)
	if c, _ := sb.Condition(cp.CondIsolationVerified); c.Status != cp.ConditionTrue || sb.Tier != "process" {
		t.Fatalf("isolation not verified: %+v", sb)
	}
	sb, err := r.svc.Stop(ctx, alice, sb.ID, "", time.Second)
	if err != nil || sb.State != cp.StateStopping {
		t.Fatalf("stop: %+v %v", sb, err)
	}
	if sb = r.reconcile(t, sb.ID); sb.State != cp.StateStopped {
		t.Fatalf("after stop: %s", sb.State)
	}
	sb, err = r.svc.Start(ctx, alice, sb.ID, cp.ETag(sb.ResourceVersion))
	if err != nil || sb.State != cp.StateStarting || sb.Generation != 2 {
		t.Fatalf("start: %+v %v", sb, err)
	}
	if sb = r.reconcile(t, sb.ID); sb.State != cp.StateReady || sb.Generation != 2 {
		t.Fatalf("after start: %+v", sb)
	}
	if _, err := r.svc.Delete(ctx, alice, sb.ID, ""); err != nil {
		t.Fatal(err)
	}
	_ = r.svc.Reconcile(ctx, sb.ID)
	if _, err := r.store.Get(ctx, sb.ID); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("still stored after delete: %v", err)
	}
	if r.fake.Calls("delete") != 1 {
		t.Fatalf("driver delete called %d times", r.fake.Calls("delete"))
	}
	evs, _ := r.store.AuditFor(ctx, sb.ID)
	var actions []string
	for _, ev := range evs {
		actions = append(actions, ev.Principal+":"+ev.Action+":"+ev.Outcome)
	}
	got := strings.Join(actions, " ")
	for _, want := range []string{"alice:create:requested", "alice:create:ok", "alice:stop:ok", "alice:start:ok",
		"alice:delete:ok", "system:reconciler:reconcile:ok"} {
		if !strings.Contains(got, want) {
			t.Fatalf("record lacks %s: %s", want, got)
		}
	}
}

func TestDriverErrorsFailClosed(t *testing.T) {
	r := newRig(t, nil, nil)
	r.fake.SetErr("create", driver.Errorf(driver.ErrUnavailable, "no engine"))
	sb, err := r.svc.Create(context.Background(), alice, cp.CreateRequest{Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	sb = r.reconcile(t, sb.ID)
	c, _ := sb.Condition(cp.CondDriverReady)
	if sb.State != cp.StateError || c.Status != cp.ConditionFalse || c.Reason != "unavailable" {
		t.Fatalf("a failed create is %+v", sb)
	}
}

func TestWeakerTierIsRefused(t *testing.T) {
	r := newRig(t, map[string]string{cp.SetMinTier: "container"}, nil)
	sb, err := r.svc.Create(context.Background(), alice, cp.CreateRequest{Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	sb = r.reconcile(t, sb.ID)
	c, _ := sb.Condition(cp.CondIsolationVerified)
	if sb.State != cp.StateError || c.Status != cp.ConditionFalse {
		t.Fatalf("a process-tier instance under a container minimum is %+v", sb)
	}
	if st, _ := r.fake.Status(context.Background(), sb.ID); st.Phase != driver.PhaseStopped {
		t.Fatalf("the weaker instance was left %s", st.Phase)
	}
	// A sandbox may not ask for less than the configured minimum.
	if _, err := r.svc.Create(context.Background(), alice, cp.CreateRequest{Template: "default",
		Settings: map[string]string{cp.SetMinTier: "process"}}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("a looser min tier was accepted: %v", err)
	}
}

func TestReadyThatFailsGoesToErrorAndTierDropIsCaught(t *testing.T) {
	r := newRig(t, nil, nil)
	sb := r.ready(t, nil)
	r.fake.SetPhase(sb.ID, driver.PhaseFailed)
	if sb = r.reconcile(t, sb.ID); sb.State != cp.StateError {
		t.Fatalf("a failed instance stayed %s", sb.State)
	}

	r = newRig(t, map[string]string{cp.SetMinTier: "process"}, nil)
	sb = r.ready(t, nil)
	r.fake.SetTier("none")
	if sb = r.reconcile(t, sb.ID); sb.State != cp.StateError {
		t.Fatalf("a tier that dropped below the minimum stayed %s", sb.State)
	}

	r = newRig(t, nil, nil)
	sb = r.ready(t, nil)
	r.fake.SetErr("status", errors.New("engine hiccup"))
	sb = r.reconcile(t, sb.ID)
	if c, _ := sb.Condition(cp.CondDriverReady); sb.State != cp.StateReady || c.Status != cp.ConditionUnknown {
		t.Fatalf("an unanswered status is %+v", sb)
	}
	rv := sb.ResourceVersion
	if sb = r.reconcile(t, sb.ID); sb.ResourceVersion != rv {
		t.Fatal("a second unanswered status wrote again")
	}
	r.fake.SetErr("status", nil)
	if sb = r.reconcile(t, sb.ID); func() bool { c, _ := sb.Condition(cp.CondDriverReady); return c.Status != cp.ConditionTrue }() {
		t.Fatalf("an answered status did not restore DriverReady: %+v", sb.Conditions)
	}
}

func TestDeleteFailureStaysDeletingAndRetries(t *testing.T) {
	r := newRig(t, nil, nil)
	ctx := context.Background()
	sb := r.ready(t, nil)
	r.fake.SetErr("delete", driver.Errorf(driver.ErrUnavailable, "busy"))
	if _, err := r.svc.Delete(ctx, alice, sb.ID, ""); err != nil {
		t.Fatal(err)
	}
	sb = r.reconcile(t, sb.ID)
	if c, _ := sb.Condition(cp.CondDeleteFailed); sb.State != cp.StateDeleting || c.Status != cp.ConditionTrue {
		t.Fatalf("a failed delete is %+v", sb)
	}
	// Work is refused while it waits.
	if _, _, err := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "true"}, cp.ExecIO{}); !errors.Is(err, cp.ErrInvalidTransition) {
		t.Fatalf("exec in Deleting: %v", err)
	}
	if got, err := r.svc.Delete(ctx, alice, sb.ID, ""); err != nil || got.State != cp.StateDeleting {
		t.Fatalf("deleting again: %+v %v", got, err)
	}
	r.fake.SetErr("delete", nil)
	_ = r.svc.Reconcile(ctx, sb.ID)
	if _, err := r.store.Get(ctx, sb.ID); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("not removed on retry: %v", err)
	}
}

func TestRestartPolicyBackoffAndGenerations(t *testing.T) {
	r := newRig(t, map[string]string{cp.SetRestartPolicy: cp.RestartOnFailure, cp.SetMaxRestarts: "2", cp.SetRestartBackoff: "10"}, nil)
	sb := r.ready(t, nil)
	gen := sb.Generation
	for i := range 2 {
		r.fake.SetPhase(sb.ID, driver.PhaseFailed)
		if sb = r.reconcile(t, sb.ID); sb.State != cp.StateError {
			t.Fatalf("round %d: %s", i, sb.State)
		}
		// The first pass schedules, it does not restart.
		sb = r.reconcile(t, sb.ID)
		want := t0.Add(cp.Backoff(10*time.Second, i))
		if sb.State != cp.StateError || sb.NextRestart == nil {
			t.Fatalf("round %d: no restart scheduled: %+v", i, sb)
		}
		wait := sb.NextRestart.Sub(r.clock.now())
		if wait != cp.Backoff(10*time.Second, i) {
			t.Fatalf("round %d: backoff %s, want %s (next %s)", i, wait, cp.Backoff(10*time.Second, i), want)
		}
		r.clock.add(wait - time.Second)
		if sb = r.reconcile(t, sb.ID); sb.State != cp.StateError {
			t.Fatalf("round %d: restarted before the backoff ended", i)
		}
		r.clock.add(time.Second)
		if sb = r.reconcile(t, sb.ID); sb.State != cp.StateStarting || sb.Generation != gen+1 || sb.Restarts != i+1 {
			t.Fatalf("round %d: after backoff %+v", i, sb)
		}
		gen = sb.Generation
		if sb = r.reconcile(t, sb.ID); sb.State != cp.StateReady {
			t.Fatalf("round %d: restart did not come up: %s", i, sb.State)
		}
	}
	// The budget is spent: it stays in Error and says why.
	r.fake.SetPhase(sb.ID, driver.PhaseFailed)
	r.reconcile(t, sb.ID)
	sb = r.reconcile(t, sb.ID)
	c, _ := sb.Condition(cp.CondRestartScheduled)
	if sb.State != cp.StateError || c.Reason != "LimitReached" || sb.NextRestart != nil {
		t.Fatalf("after the limit: %+v", sb)
	}
	evs, _ := r.store.AuditFor(context.Background(), sb.ID)
	restarts := 0
	for _, ev := range evs {
		if ev.Action == "restart" && ev.Outcome == cp.OutcomeOK && ev.Principal == cp.SystemPrincipal {
			restarts++
		}
	}
	if restarts != 2 {
		t.Fatalf("%d restarts recorded", restarts)
	}
	// A client's start resets the budget.
	sb, err := r.svc.Start(context.Background(), alice, sb.ID, "")
	if err != nil || sb.Restarts != 0 {
		t.Fatalf("client start: %+v %v", sb, err)
	}
}

func TestRestartPolicies(t *testing.T) {
	for _, c := range []struct {
		policy string
		phase  driver.Phase
		want   bool
	}{
		{cp.RestartNever, driver.PhaseFailed, false},
		{cp.RestartOnFailure, driver.PhaseFailed, true},
		{cp.RestartOnFailure, driver.PhaseExited, false},
		{cp.RestartAlways, driver.PhaseExited, true},
	} {
		r := newRig(t, map[string]string{cp.SetRestartPolicy: c.policy, cp.SetRestartBackoff: "1"}, nil)
		sb := r.ready(t, nil)
		r.fake.SetPhase(sb.ID, c.phase)
		r.reconcile(t, sb.ID)
		r.reconcile(t, sb.ID)
		r.clock.add(time.Second)
		sb = r.reconcile(t, sb.ID)
		if got := sb.State == cp.StateStarting; got != c.want {
			t.Errorf("%s after %s: state %s", c.policy, c.phase, sb.State)
		}
	}
	// A sandbox a client stopped is never restarted.
	r := newRig(t, map[string]string{cp.SetRestartPolicy: cp.RestartAlways, cp.SetRestartBackoff: "1"}, nil)
	sb := r.ready(t, nil)
	if _, err := r.svc.Stop(context.Background(), alice, sb.ID, "", 0); err != nil {
		t.Fatal(err)
	}
	r.reconcile(t, sb.ID)
	r.clock.add(time.Hour)
	if sb = r.reconcile(t, sb.ID); sb.State != cp.StateStopped {
		t.Fatalf("a stopped sandbox became %s", sb.State)
	}
}

func TestStableRunResetsRestarts(t *testing.T) {
	r := newRig(t, map[string]string{cp.SetRestartPolicy: cp.RestartOnFailure, cp.SetRestartBackoff: "1"}, nil)
	sb := r.ready(t, nil)
	r.fake.SetPhase(sb.ID, driver.PhaseFailed)
	r.reconcile(t, sb.ID)
	r.reconcile(t, sb.ID)
	r.clock.add(time.Second)
	r.reconcile(t, sb.ID)
	if sb = r.reconcile(t, sb.ID); sb.State != cp.StateReady || sb.Restarts != 1 {
		t.Fatalf("%+v", sb)
	}
	r.clock.add(cp.StableAfter)
	if sb = r.reconcile(t, sb.ID); sb.Restarts != 0 {
		t.Fatalf("restarts not reset after a stable run: %d", sb.Restarts)
	}
}

func TestBackoff(t *testing.T) {
	for n, want := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second} {
		if got := cp.Backoff(10*time.Second, n); got != want {
			t.Errorf("Backoff(10s, %d) = %s", n, got)
		}
	}
	if got := cp.Backoff(10*time.Second, 1000); got != cp.MaxBackoff {
		t.Fatalf("Backoff does not cap: %s", got)
	}
}

func TestRunStopsWithItsContext(t *testing.T) {
	r := newRig(t, map[string]string{cp.SetReconcileInterval: "1"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.svc.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !r.svc.Running() {
		if time.Now().After(deadline) {
			t.Fatal("Run did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := r.svc.Ready(context.Background()); err != nil {
		t.Fatalf("ready: %v", err)
	}
	sb, err := r.svc.Create(context.Background(), alice, cp.CreateRequest{Template: "default"})
	if err != nil {
		t.Fatal(err)
	}
	for {
		got, _ := r.store.Get(context.Background(), sb.ID)
		if got.State == cp.StateReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a kicked create was not reconciled: %s", got.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if r.svc.Running() || r.svc.Ready(context.Background()) == nil {
		t.Fatal("still running after cancel")
	}
}

func TestOwnersAndAdmins(t *testing.T) {
	r := newRig(t, nil, nil)
	ctx := context.Background()
	sb := r.ready(t, nil)
	bob := cp.Caller{Principal: "bob"}
	if _, err := r.svc.Get(ctx, bob, sb.ID); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("another owner read it: %v", err)
	}
	if _, err := r.svc.Stop(ctx, bob, sb.ID, "", 0); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("another owner stopped it: %v", err)
	}
	if items, _, _ := r.svc.List(ctx, bob, cp.ListOptions{}); len(items) != 0 {
		t.Fatalf("another owner listed %d", len(items))
	}
	admin := cp.Caller{Principal: "root", Admin: true}
	if items, _, _ := r.svc.List(ctx, admin, cp.ListOptions{}); len(items) != 1 {
		t.Fatalf("an admin listed %d", len(items))
	}
	if _, err := r.svc.Get(ctx, cp.Caller{}, sb.ID); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("a caller with no principal read it: %v", err)
	}
}

func TestExecCapsTotalOutputAndTime(t *testing.T) {
	r := newRig(t, map[string]string{cp.SetExecOutput: "2048"}, nil)
	ctx := context.Background()
	sb := r.ready(t, nil)
	stopped := make(chan struct{})
	r.fake.ExecFunc = func(ctx context.Context, _ string, req driver.ExecRequest) (driver.ExecResult, error) {
		chunk := bytes.Repeat([]byte("x"), 512)
		for i := 0; ; i++ {
			w := req.Stdout
			if i%2 == 1 {
				w = req.Stderr
			}
			if _, err := w.Write(chunk); err != nil {
				<-ctx.Done()
				close(stopped)
				return driver.ExecResult{}, ctx.Err()
			}
		}
	}
	var out, errOut bytes.Buffer
	_, _, err := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "yes"}, cp.ExecIO{Stdout: &out, Stderr: &errOut})
	if !errors.Is(err, driver.ErrOutputLimit) {
		t.Fatalf("exec past the cap: %v", err)
	}
	if out.Len()+errOut.Len() != 2048 {
		t.Fatalf("%d bytes passed a 2048-byte cap", out.Len()+errOut.Len())
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the command was not cancelled at the cap")
	}

	r = newRig(t, map[string]string{cp.SetExecTimeout: "1"}, nil)
	sb = r.ready(t, nil)
	r.fake.ExecFunc = func(ctx context.Context, _ string, _ driver.ExecRequest) (driver.ExecResult, error) {
		<-ctx.Done()
		return driver.ExecResult{}, ctx.Err()
	}
	if _, _, err := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "sleep 9"}, cp.ExecIO{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exec past the timeout: %v", err)
	}
}

func TestExecRecordsNoSecret(t *testing.T) {
	r := newRig(t, nil, nil)
	ctx := context.Background()
	sb := r.ready(t, nil)
	if _, _, err := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "curl -H 'token: hunter2'"}, cp.ExecIO{}); err != nil {
		t.Fatal(err)
	}
	evs, _ := r.store.AuditFor(ctx, sb.ID)
	for _, ev := range evs {
		if strings.Contains(ev.Detail["command"], "hunter2") {
			t.Fatalf("the record holds the command line: %+v", ev)
		}
	}
}

type failingAudit struct{ cp.Memory }

func (*failingAudit) Append(context.Context, cp.AuditEvent) error { return errors.New("disk full") }

func TestUnrecordableRequestIsRefused(t *testing.T) {
	st, _ := cp.NewSettings(nil, nil)
	reg := driver.NewRegistry()
	_ = reg.Register(drivertest.New("process"))
	mem := cp.NewMemory()
	svc, err := cp.New(cp.Config{Store: mem, Audit: &failingAudit{}, Drivers: reg, Settings: st,
		Workspace: func(string) (string, error) { return "/w", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), alice, cp.CreateRequest{Template: "default"}); !errors.Is(err, cp.ErrUnavailable) {
		t.Fatalf("create with no record: %v", err)
	}
	if items, _, _ := mem.List(context.Background(), cp.ListOptions{}); len(items) != 0 {
		t.Fatal("a sandbox was stored though its request could not be recorded")
	}
}

func TestUpdatePatch(t *testing.T) {
	r := newRig(t, map[string]string{cp.SetExecTimeout: "100"}, map[string]bool{cp.SetExecTimeout: true})
	ctx := context.Background()
	sb, err := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "default", Labels: map[string]string{"a": "1", "b": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	tmpl, v := "other", "3"
	sb, err = r.svc.Update(ctx, alice, sb.ID, cp.ETag(sb.ResourceVersion), cp.Patch{Template: &tmpl,
		Labels: map[string]*string{"a": nil, "c": &v}})
	if err != nil {
		t.Fatal(err)
	}
	if sb.Generation != 2 || sb.Template != "other" || sb.Labels["a"] != "" || sb.Labels["c"] != "3" || sb.Labels["b"] != "2" {
		t.Fatalf("after patch %+v", sb)
	}
	if _, err := r.svc.Update(ctx, alice, sb.ID, cp.ETag(sb.ResourceVersion-1), cp.Patch{}); !errors.Is(err, cp.ErrPreconditionFailed) {
		t.Fatalf("stale If-Match: %v", err)
	}
	reserved := "x"
	if _, err := r.svc.Update(ctx, alice, sb.ID, "", cp.Patch{Labels: map[string]*string{"abhed.dev/session": &reserved}}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("a reserved label was set: %v", err)
	}
	loose, tight := "200", "50"
	if _, err := r.svc.Update(ctx, alice, sb.ID, "", cp.Patch{Settings: map[string]*string{cp.SetExecTimeout: &loose}}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("a setting looser than the managed ceiling was accepted: %v", err)
	}
	if sb, err = r.svc.Update(ctx, alice, sb.ID, "", cp.Patch{Settings: map[string]*string{cp.SetExecTimeout: &tight}}); err != nil || sb.Settings[cp.SetExecTimeout] != "50" {
		t.Fatalf("a tighter setting: %+v %v", sb, err)
	}
	if _, err := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "bad name\n"}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("a template with a newline: %v", err)
	}
	long := strings.Repeat("t", 254)
	if _, err := r.svc.Update(ctx, alice, sb.ID, "", cp.Patch{Template: &long}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("a 254-character template: %v", err)
	}
	global := "5"
	if _, err := r.svc.Update(ctx, alice, sb.ID, "", cp.Patch{Settings: map[string]*string{cp.SetMutationsPerMin: &global}}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("a global setting was set per sandbox: %v", err)
	}
}
