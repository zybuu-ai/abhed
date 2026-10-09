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
