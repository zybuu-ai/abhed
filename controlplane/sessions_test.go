package controlplane_test

import (
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

// commandRig is a rig whose driver hands out commands, each sandbox with a
// folder of its own.
func commandRig(t *testing.T) *rig {
	t.Helper()
	st, err := cp.NewSettings(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := drivertest.New("process", driver.CapCommand)
	reg := driver.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	mem := cp.NewMemory()
	root := t.TempDir()
	svc, err := cp.New(cp.Config{Store: mem, Audit: mem, Drivers: reg, Settings: st,
		Workspace: func(id string) (string, error) { return root + "/" + id, nil }, Now: (&clock{t: t0}).now})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{svc: svc, store: mem, fake: fake}
}

func sessionScope(id string) map[string]string { return map[string]string{cp.LabelSession: id} }

func TestCommandIsRecordedGuardedAndNeedsReady(t *testing.T) {
	ctx := context.Background()
	r := commandRig(t)
	maker := cp.Caller{Principal: cp.SessionsPrincipal, Scope: sessionScope("s1")}
	sb, err := r.svc.Create(ctx, maker, cp.CreateRequest{Template: "session"})
	if err != nil {
		t.Fatal(err)
	}
	user := cp.Caller{Principal: "alice", Admin: true, Scope: sessionScope("s1")}
	if _, _, err := r.svc.Command(ctx, user, sb.ID, driver.CommandRequest{Command: "true"}); !errors.Is(err, cp.ErrInvalidTransition) {
		t.Fatalf("a command before Ready: %v", err)
	}
	if sb, err = r.svc.WaitReady(ctx, user, sb.ID); err != nil || sb.State != cp.StateReady {
		t.Fatalf("wait ready: %v %v", sb.State, err)
	}
	g := guarded(t, r, nil)
	cmd, got, err := r.svc.Command(ctx, user, sb.ID, driver.CommandRequest{Command: "echo hi"})
	if err != nil || cmd == nil || got.ID != sb.ID {
		t.Fatal(err)
	}
	if a := g.last(); a.Action != cp.ActionExec || a.Caller.Principal != "alice" || a.Exec.Command != "echo hi" {
		t.Fatalf("guard saw %+v", a)
	}
	if got := outcomes(t, r, sb.ID); !strings.HasSuffix(got, "exec:requested:,exec:ok:") {
		t.Fatalf("record %s", got)
	}
	evs, _ := r.store.AuditFor(ctx, sb.ID)
	if evs[0].Action != "create" || evs[0].Detail["session"] != "s1" {
		t.Fatalf("the create does not name the session: %+v", evs[0])
	}
	// Another session's caller sees no such sandbox.
	other := cp.Caller{Principal: "bob", Admin: true, Scope: sessionScope("s2")}
	if _, _, err := r.svc.Command(ctx, other, sb.ID, driver.CommandRequest{Command: "true"}); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("another session's command: %v", err)
	}
}

func TestCommandRefusesADriverThatCannot(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil, nil) // its driver declares attach only
	sb := r.ready(t)
	if _, _, err := r.svc.Command(ctx, alice, sb.ID, driver.CommandRequest{Command: "true"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("a driver without command: %v", err)
	}
}

func TestWaitReadyNamesWhyItFailed(t *testing.T) {
	ctx := context.Background()
	r := commandRig(t)
	r.fake.SetErr("create", driver.Errorf(driver.ErrUnavailable, "engine gone"))
	sb, err := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.WaitReady(ctx, alice, sb.ID); !errors.Is(err, cp.ErrUnavailable) || !strings.Contains(err.Error(), "engine gone") {
		t.Fatalf("wait ready: %v", err)
	}
	if _, err := r.svc.WaitReady(ctx, cp.Caller{Principal: "bob"}, sb.ID); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("another caller waited on it: %v", err)
	}
}

func TestStopNowAndDeleteNow(t *testing.T) {
	ctx := context.Background()
	r := commandRig(t)
	sb, _ := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "t"})
	if _, err := r.svc.WaitReady(ctx, alice, sb.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.StopNow(ctx, alice, sb.ID, time.Second); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.store.Get(ctx, sb.ID); got.State != cp.StateStopped {
		t.Fatalf("after StopNow: %s", got.State)
	}
	if err := r.svc.DeleteNow(ctx, alice, sb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Get(ctx, sb.ID); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("after DeleteNow: %v", err)
	}
	if err := r.svc.DeleteNow(ctx, alice, sb.ID); err != nil {
		t.Fatalf("deleting it again: %v", err)
	}
}

func newPool(t *testing.T, r *rig, size int) *cp.Pool {
	t.Helper()
	p, err := r.svc.NewPool(cp.PoolConfig{Key: "session", Size: size, Create: cp.CreateRequest{Template: "session"}, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// readyAll makes every pool member Ready, as the reconciler would.
func readyAll(t *testing.T, r *rig, p *cp.Pool) []cp.Sandbox {
	t.Helper()
	ms, err := p.Members(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []cp.Sandbox
	for _, m := range ms {
		sb, err := r.svc.WaitReady(context.Background(), cp.Caller{Principal: cp.SessionsPrincipal, Scope: map[string]string{cp.LabelPool: "session"}}, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, sb)
	}
	return out
}

func TestPoolConfigIsBounded(t *testing.T) {
	r := commandRig(t)
	for _, c := range []cp.PoolConfig{
		{Key: "session", Size: 0}, {Key: "session", Size: cp.MaxPoolSize + 1}, {Key: "", Size: 1}, {Key: "bad key", Size: 1},
		{Key: "session", Size: 1, Create: cp.CreateRequest{Template: "t", Labels: map[string]string{"a": "b"}}},
	} {
		if _, err := r.svc.NewPool(c); err == nil {
			t.Errorf("%+v was accepted", c)
		}
	}
}

func TestPoolFillsClaimsAndRefills(t *testing.T) {
	ctx := context.Background()
	r := commandRig(t)
	p := newPool(t, r, 2)
	if err := p.Fill(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Fill(ctx); err != nil {
		t.Fatal(err)
	}
	ms := readyAll(t, r, p)
	if len(ms) != 2 {
		t.Fatalf("filled to %d, want 2 and never more", len(ms))
	}
	for _, m := range ms {
		if m.Owner != cp.SessionsPrincipal {
			t.Fatalf("a pool member is owned by %q", m.Owner)
		}
	}
	user := cp.Caller{Principal: "alice", Admin: true, Scope: sessionScope("s1")}
	sb, ok, err := p.Claim(ctx, user, sessionScope("s1"))
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if _, pooled := sb.Labels[cp.LabelPool]; pooled || sb.Labels[cp.LabelSession] != "s1" || sb.Owner != cp.SessionsPrincipal {
		t.Fatalf("claimed %+v", sb)
	}
	var before cp.Sandbox
	for _, m := range ms {
		if m.ID == sb.ID {
			before = m
		}
	}
	if sb.Generation != before.Generation+1 {
		t.Fatalf("generation %d after claim, was %d", sb.Generation, before.Generation)
	}
	evs, _ := r.store.AuditFor(ctx, sb.ID)
	last := evs[len(evs)-1]
	if last.Action != "claim" || last.Outcome != cp.OutcomeOK || last.Principal != "alice" || last.Detail["session"] != "s1" {
		t.Fatalf("claim record %+v", last)
	}
	// A claimed sandbox never goes back to the pool: one left, refilled to two.
	if left, _ := p.Members(ctx); len(left) != 1 {
		t.Fatalf("%d left in the pool", len(left))
	}
	if err := p.Fill(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := p.Members(ctx); len(again) != 2 || again[0].ID == sb.ID || again[1].ID == sb.ID {
		t.Fatalf("refilled %+v", again)
	}
}

func TestPoolReplacesFailedMembersAndDrains(t *testing.T) {
	ctx := context.Background()
	r := commandRig(t)
	p := newPool(t, r, 2)
	if err := p.Fill(ctx); err != nil {
		t.Fatal(err)
	}
	ms := readyAll(t, r, p)
	r.fake.SetPhase(ms[0].ID, driver.PhaseFailed)
	if got := r.reconcile(t, ms[0].ID); got.State != cp.StateError {
		t.Fatalf("failed member is %s", got.State)
	}
	// None Ready claims nothing; the failed one is never handed out.
	if err := p.Fill(ctx); err != nil {
		t.Fatal(err)
	}
	members, _ := p.Members(ctx)
	live := 0
	for _, m := range members {
		if m.ID == ms[0].ID && m.State != cp.StateDeleting {
			t.Fatalf("the failed member was kept: %s", m.State)
		}
		if m.State != cp.StateDeleting {
			live++
		}
	}
	if live != 2 {
		t.Fatalf("%d live members, want 2", live)
	}
	if err := p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if left, _ := p.Members(ctx); len(left) != 0 {
		t.Fatalf("%d members after drain", len(left))
	}
	if _, ok, err := p.Claim(ctx, alice, sessionScope("s1")); ok || err != nil {
		t.Fatalf("claimed from a drained pool: %v %v", ok, err)
	}
}

// Many sessions claiming at once each get a different member, and no
// member is claimed twice.
func TestPoolClaimRace(t *testing.T) {
	ctx := context.Background()
	r := commandRig(t)
	p := newPool(t, r, 4)
	if err := p.Fill(ctx); err != nil {
		t.Fatal(err)
	}
	readyAll(t, r, p)
	// A second pool over the same store, as another process would hold.
	p2 := newPool(t, r, 4)
	const claimers = 12
	var wg sync.WaitGroup
	got := make(chan string, claimers)
	for i := range claimers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool := p
			if i%2 == 1 {
				pool = p2
			}
			id := "s" + string(rune('a'+i))
			sb, ok, err := pool.Claim(ctx, cp.Caller{Principal: "u", Admin: true, Scope: sessionScope(id)}, sessionScope(id))
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				got <- sb.ID + "=" + id
			}
		}()
	}
	wg.Wait()
	close(got)
	seen := map[string]string{}
	for g := range got {
		sid, sess, _ := strings.Cut(g, "=")
		if prev, dup := seen[sid]; dup {
			t.Fatalf("sandbox %s claimed by %s and %s", sid, prev, sess)
		}
		seen[sid] = sess
		sb, _ := r.store.Get(ctx, sid)
		if sb.Labels[cp.LabelSession] != sess {
			t.Fatalf("sandbox %s is labelled for %s, claimed by %s", sid, sb.Labels[cp.LabelSession], sess)
		}
	}
	if len(seen) != 4 {
		t.Fatalf("%d claims succeeded, want the 4 members", len(seen))
	}
}

// The guard is asked about a claim as an update by the claiming user, and
// its refusal leaves the member in the pool.
func TestPoolClaimIsGuarded(t *testing.T) {
	ctx := context.Background()
	r := commandRig(t)
	p := newPool(t, r, 1)
	if err := p.Fill(ctx); err != nil {
		t.Fatal(err)
	}
	ms := readyAll(t, r, p)
	refuse := cp.ErrQuotaExceeded
	g := guarded(t, r, func(a *cp.Admission) (func(), error) {
		if a.Action == cp.ActionUpdate {
			return nil, refuse
		}
		return nil, nil
	})
	user := cp.Caller{Principal: "alice", Admin: true, Scope: sessionScope("s1")}
	if _, _, err := p.Claim(ctx, user, sessionScope("s1")); !errors.Is(err, cp.ErrQuotaExceeded) {
		t.Fatalf("a refused claim: %v", err)
	}
	if a := g.last(); a.Caller.Principal != "alice" || a.Next == nil || a.Next.Labels[cp.LabelSession] != "s1" {
		t.Fatalf("guard saw %+v", a)
	}
	if left, _ := p.Members(ctx); len(left) != 1 || left[0].ID != ms[0].ID || left[0].Generation != ms[0].Generation {
		t.Fatalf("a refused claim changed the pool: %+v", left)
	}
	if _, _, err := p.Claim(ctx, user, map[string]string{"team": "x"}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("a claim setting a client label: %v", err)
	}
}

func TestUserLabelIsATenantsUser(t *testing.T) {
	a, b, c := cp.UserLabel("t1", "alice@example.com"), cp.UserLabel("t2", "alice@example.com"), cp.UserLabel("t1", "bob")
	if a == b || a == c || cp.ValidateLabels(map[string]string{cp.LabelSessionUser: a}, true) != nil {
		t.Fatalf("labels %s %s %s", a, b, c)
	}
}

// A scoped pool's fills are made in its scope, so the guard admits and
// counts each as that scope's; pools of two scopes never share a member, and
// a claim cannot move one out of its scope.
func TestScopedPoolFillsAreGuardedInTheirScope(t *testing.T) {
	ctx := context.Background()
	r := commandRig(t)
	ws := cp.ReservedPrefix + "workspace"
	var mu sync.Mutex
	created := map[string]int{}
	guarded(t, r, func(a *cp.Admission) (func(), error) {
		if a.Action == cp.ActionCreate {
			mu.Lock()
			defer mu.Unlock()
			if created[a.Next.Labels[ws]] >= 2 {
				return nil, cp.ErrQuotaExceeded
			}
			created[a.Next.Labels[ws]]++
		}
		return nil, nil
	})
	pool := func(scope string) *cp.Pool {
		p, err := r.svc.NewPool(cp.PoolConfig{Key: "session", Size: 3, Create: cp.CreateRequest{Template: "session"},
			Scope: map[string]string{ws: scope}, Interval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	pay, ops := pool("pay"), pool("ops")
	if err := pay.Fill(ctx); !errors.Is(err, cp.ErrQuotaExceeded) {
		t.Fatalf("a fill past the scope's quota: %v", err)
	}
	if err := ops.Fill(ctx); !errors.Is(err, cp.ErrQuotaExceeded) {
		t.Fatalf("a fill past the scope's quota: %v", err)
	}
	pm, _ := pay.Members(ctx)
	om, _ := ops.Members(ctx)
	if len(pm) != 2 || len(om) != 2 || created["pay"] != 2 || created["ops"] != 2 || created[""] != 0 {
		t.Fatalf("members pay %d ops %d, admitted %v", len(pm), len(om), created)
	}
	for _, m := range pm {
		if m.Labels[ws] != "pay" {
			t.Fatalf("a pay member carries %v", m.Labels)
		}
		if _, err := r.svc.WaitReady(ctx, cp.Caller{Principal: cp.SessionsPrincipal, Scope: map[string]string{cp.LabelPool: "session"}}, m.ID); err != nil {
			t.Fatal(err)
		}
	}
	user := cp.Caller{Principal: "alice", Admin: true, Scope: map[string]string{ws: "pay", cp.LabelSession: "s1"}}
	if _, _, err := pay.Claim(ctx, user, map[string]string{cp.LabelSession: "s1", ws: "ops"}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("a claim moving a member to another scope: %v", err)
	}
	sb, ok, err := pay.Claim(ctx, user, sessionScope("s1"))
	if err != nil || !ok || sb.Labels[ws] != "pay" || sb.Labels[cp.LabelSession] != "s1" {
		t.Fatalf("claim %+v %v %v", sb.Labels, ok, err)
	}
	if _, err := r.svc.NewPool(cp.PoolConfig{Key: "session", Size: 1, Scope: map[string]string{"team": "x"}}); err == nil {
		t.Fatal("a pool scoped by a client label was accepted")
	}
	if _, err := r.svc.NewPool(cp.PoolConfig{Key: "session", Size: 1, Scope: map[string]string{cp.LabelPool: "other"}}); err == nil {
		t.Fatal("a pool scope naming the pool label was accepted")
	}
}

// Fills that keep failing back off, from the interval doubling to the
// bound, rather than retrying at every interval; a claim's kick does not
// cut the wait short, and the pool fills once creates succeed again.
func TestPoolBacksOffWhileFillsFail(t *testing.T) {
	r := commandRig(t)
	var mu sync.Mutex
	attempts, refuse := 0, true
	guarded(t, r, func(a *cp.Admission) (func(), error) {
		if a.Action != "create" {
			return nil, nil
		}
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if refuse {
			return nil, errors.New("no capacity")
		}
		return nil, nil
	})
	p, err := r.svc.NewPool(cp.PoolConfig{Key: "session", Size: 1, Create: cp.CreateRequest{Template: "session"},
		Interval: 10 * time.Millisecond, MaxBackoff: 80 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	for end := time.Now().Add(600 * time.Millisecond); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		p.Kick()
	}
	mu.Lock()
	n := attempts
	refuse = false
	mu.Unlock()
	// 10+20+40+80+80... ms is about 10 fills in 600ms; every interval or
	// kick would be 60 or more.
	if n < 3 || n > 14 {
		t.Fatalf("%d fills in 600ms while every one failed", n)
	}
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if ms, _ := p.Members(context.Background()); len(ms) == 1 {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the pool did not fill once creates succeeded")
		}
	}
}
