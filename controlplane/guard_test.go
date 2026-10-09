package controlplane_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	cp "github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
)

// countExecs makes the rig's driver count the commands it runs.
func countExecs(r *rig) *atomic.Int32 {
	n := new(atomic.Int32)
	r.fake.ExecFunc = func(context.Context, string, driver.ExecRequest) (driver.ExecResult, error) {
		n.Add(1)
		return driver.ExecResult{}, nil
	}
	return n
}

// guardFunc is a Guard from a function, recording what it was asked.
type guardFunc struct {
	mu    sync.Mutex
	seen  []cp.Admission
	admit func(a *cp.Admission) (func(), error)
}

func (g *guardFunc) Admit(_ context.Context, a *cp.Admission) (func(), error) {
	g.mu.Lock()
	g.seen = append(g.seen, *a)
	g.mu.Unlock()
	if g.admit == nil {
		return nil, nil
	}
	return g.admit(a)
}

func (g *guardFunc) last() cp.Admission {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.seen[len(g.seen)-1]
}

func guarded(t *testing.T, r *rig, admit func(a *cp.Admission) (func(), error)) *guardFunc {
	t.Helper()
	g := &guardFunc{admit: admit}
	if err := r.svc.SetGuard(g); err != nil {
		t.Fatal(err)
	}
	return g
}

func outcomes(t *testing.T, r *rig, id string) string {
	t.Helper()
	evs, err := r.store.AuditFor(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Action+":"+ev.Outcome+":"+ev.Code)
	}
	return strings.Join(out, ",")
}

// A refusal is recorded and leaves the store and the driver untouched.
func TestGuardRefusesBeforeTheStoreAndDriver(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil, nil)
	sb := r.ready(t)
	execs := countExecs(r)
	refuse := error(nil)
	guarded(t, r, func(a *cp.Admission) (func(), error) { return nil, refuse })

	refuse = cp.ErrQuotaExceeded
	if _, err := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "default"}); cp.ErrorCode(err) != "quota_exceeded" {
		t.Fatalf("create past a quota: %v", err)
	}
	if items, _, _ := r.store.List(ctx, cp.ListOptions{}); len(items) != 1 {
		t.Fatalf("a refused create was stored: %d sandboxes", len(items))
	}

	refuse = &cp.ApprovalNeeded{ID: "apr_1"}
	_, err := r.svc.Delete(ctx, alice, sb.ID, "")
	var need *cp.ApprovalNeeded
	if !errors.As(err, &need) || need.ID != "apr_1" || cp.ErrorCode(err) != "approval_required" {
		t.Fatalf("delete needing approval: %v", err)
	}
	if got, _ := r.store.Get(ctx, sb.ID); got.State != cp.StateReady || got.ResourceVersion != sb.ResourceVersion {
		t.Fatalf("a refused delete changed the sandbox: %+v", got)
	}
	if !strings.HasSuffix(outcomes(t, r, sb.ID), "delete:requested:,delete:failed:approval_required") {
		t.Fatalf("the refusal is not on the record: %s", outcomes(t, r, sb.ID))
	}

	refuse = cp.ErrForbidden
	if _, _, err := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "true"}, cp.ExecIO{}); cp.ErrorCode(err) != "forbidden" {
		t.Fatalf("refused exec: %v", err)
	}
	if execs.Load() != 0 {
		t.Fatal("the driver ran a refused exec")
	}
	for _, op := range []func() error{
		func() error { _, err := r.svc.Stop(ctx, alice, sb.ID, "", 0); return err },
		func() error { _, err := r.svc.Start(ctx, alice, sb.ID, ""); return err },
		func() error {
			v := "x"
			_, err := r.svc.Update(ctx, alice, sb.ID, "*", cp.Patch{Labels: map[string]*string{"a": &v}})
			return err
		},
	} {
		if err := op(); !errors.Is(err, cp.ErrForbidden) {
			t.Fatalf("a refused mutation went through: %v", err)
		}
	}
	if got, _ := r.store.Get(ctx, sb.ID); got.ResourceVersion != sb.ResourceVersion {
		t.Fatal("a refused mutation wrote the sandbox")
	}

	// A guard that cannot decide fails closed, as unavailable.
	refuse = errors.New("usage could not be counted")
	if _, err := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "default"}); !errors.Is(err, cp.ErrUnavailable) {
		t.Fatalf("an undecided guard admitted: %v", err)
	}
}

// The guard sees the request's approval, the object now and what an update
// would make of it; its release runs once the operation is done.
func TestGuardSeesTheRequestAndReleasesAfter(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil, nil)
	sb := r.ready(t)
	execs := countExecs(r)
	var mu sync.Mutex
	var released []string
	g := guarded(t, r, func(a *cp.Admission) (func(), error) {
		action := a.Action
		return func() {
			mu.Lock()
			defer mu.Unlock()
			switch action {
			case cp.ActionExec:
				if execs.Load() != 1 {
					t.Error("exec released before the driver ran")
				}
			case cp.ActionCreate:
				if items, _, _ := r.store.List(ctx, cp.ListOptions{}); len(items) != 2 {
					t.Error("create released before the store write")
				}
			}
			released = append(released, action)
		}, nil
	})

	actx := cp.WithApproval(ctx, " apr_9 ")
	five := "5"
	if _, err := r.svc.Update(actx, alice, sb.ID, "*", cp.Patch{Settings: map[string]*string{cp.SetMaxRestarts: &five}}); err != nil {
		t.Fatal(err)
	}
	a := g.last()
	if a.Action != cp.ActionUpdate || a.Approval != "apr_9" || a.Sandbox.Settings[cp.SetMaxRestarts] != "" ||
		a.Next == nil || a.Next.Settings[cp.SetMaxRestarts] != "5" || a.Caller.Principal != "alice" {
		t.Fatalf("update admission: %+v", a)
	}
	var out bytes.Buffer
	if _, _, err := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "echo hi"}, cp.ExecIO{Stdout: &out}); err != nil {
		t.Fatal(err)
	}
	if a := g.last(); a.Exec == nil || a.Exec.Command != "echo hi" || a.Approval != "" {
		t.Fatalf("exec admission: %+v", a)
	}
	if _, err := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "default"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Stop(ctx, alice, sb.ID, "", 0); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(released, ",") != "update,exec,create,stop" {
		t.Fatalf("released %v", released)
	}
}

// A guard may tighten a create's settings; anything it makes looser than a
// ceiling, or invalid, is refused and nothing is stored.
func TestGuardMayTightenOnly(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, map[string]string{cp.SetMemoryMB: "1024"}, map[string]bool{cp.SetMemoryMB: true})
	size := "256"
	released := 0
	guarded(t, r, func(a *cp.Admission) (func(), error) {
		if a.Action == cp.ActionCreate {
			if a.Next.Settings == nil {
				a.Next.Settings = map[string]string{}
			}
			a.Next.Settings[cp.SetMemoryMB] = size
		}
		return func() { released++ }, nil
	})
	sb, err := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "default"})
	if err != nil || sb.Settings[cp.SetMemoryMB] != "256" {
		t.Fatalf("tightened create: %+v %v", sb, err)
	}
	size = "4096"
	if _, err := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "default"}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("a guard loosened past the ceiling: %v", err)
	}
	if items, _, _ := r.store.List(ctx, cp.ListOptions{}); len(items) != 1 || released != 2 {
		t.Fatalf("%d stored, %d released", len(items), released)
	}
}

func TestSetGuardOnce(t *testing.T) {
	r := newRig(t, nil, nil)
	if err := r.svc.SetGuard(nil); err == nil {
		t.Fatal("a nil guard was installed")
	}
	guarded(t, r, nil)
	if err := r.svc.SetGuard(&guardFunc{}); err == nil {
		t.Fatal("a second guard replaced the first")
	}
}

// A scoped caller sees, changes and runs only sandboxes in its scope, and
// stamps the scope on what it creates; no client can move one out of it.
func TestCallerScope(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil, nil)
	inA := cp.Caller{Principal: "alice", Scope: map[string]string{"abhed.dev/workspace": "a"}}
	adminB := cp.Caller{Principal: "root", Admin: true, Scope: map[string]string{"abhed.dev/workspace": "b"}}
	adminAll := cp.Caller{Principal: "root", Admin: true}

	sb, err := r.svc.Create(ctx, inA, cp.CreateRequest{Template: "default", Labels: map[string]string{"team": "x"}})
	if err != nil || sb.Labels["abhed.dev/workspace"] != "a" || sb.Labels["team"] != "x" {
		t.Fatalf("scoped create: %+v %v", sb, err)
	}
	r.reconcile(t, sb.ID)
	other, err := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "default"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.svc.Get(ctx, adminB, sb.ID); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("an admin of another scope saw it: %v", err)
	}
	if _, err := r.svc.Get(ctx, inA, other.ID); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("a scoped caller saw its own unscoped sandbox: %v", err)
	}
	for name, op := range map[string]func() error{
		"update": func() error {
			v := "y"
			_, err := r.svc.Update(ctx, adminB, sb.ID, "*", cp.Patch{Labels: map[string]*string{"team": &v}})
			return err
		},
		"stop":   func() error { _, err := r.svc.Stop(ctx, adminB, sb.ID, "", 0); return err },
		"delete": func() error { _, err := r.svc.Delete(ctx, adminB, sb.ID, ""); return err },
		"exec": func() error {
			_, _, err := r.svc.Exec(ctx, adminB, sb.ID, "", cp.ExecRequest{Command: "true"}, cp.ExecIO{})
			return err
		},
	} {
		if err := op(); !errors.Is(err, cp.ErrNotFound) {
			t.Fatalf("%s across scopes: %v", name, err)
		}
	}
	items, _, err := r.svc.List(ctx, adminB, cp.ListOptions{})
	if err != nil || len(items) != 0 {
		t.Fatalf("another scope's list: %v %v", items, err)
	}
	items, _, _ = r.svc.List(ctx, inA, cp.ListOptions{})
	if len(items) != 1 || items[0].ID != sb.ID {
		t.Fatalf("scoped list: %v", items)
	}
	if items, _, _ = r.svc.List(ctx, adminAll, cp.ListOptions{}); len(items) != 2 {
		t.Fatalf("an unscoped admin lists %d", len(items))
	}

	// The scope label cannot be patched away or over.
	v := "b"
	if _, err := r.svc.Update(ctx, inA, sb.ID, "*", cp.Patch{Labels: map[string]*string{"abhed.dev/workspace": &v}}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("the scope label was changed: %v", err)
	}
	if got, err := r.svc.Update(ctx, inA, sb.ID, "*", cp.Patch{ClearLabels: true}); err != nil || got.Labels["abhed.dev/workspace"] != "a" {
		t.Fatalf("clearing labels dropped the scope: %+v %v", got, err)
	}
	if _, err := r.svc.Create(ctx, alice, cp.CreateRequest{Template: "default",
		Labels: map[string]string{"abhed.dev/workspace": "a"}}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("a client set the scope label itself: %v", err)
	}

	// A scope that is not a reserved label, or has no value, is refused.
	for _, scope := range []map[string]string{{"workspace": "a"}, {"abhed.dev/workspace": ""}} {
		bad := cp.Caller{Principal: "alice", Scope: scope}
		if _, err := r.svc.Create(ctx, bad, cp.CreateRequest{Template: "default"}); !errors.Is(err, cp.ErrForbidden) {
			t.Fatalf("scope %v on create: %v", scope, err)
		}
		if _, _, err := r.svc.List(ctx, bad, cp.ListOptions{}); !errors.Is(err, cp.ErrForbidden) {
			t.Fatalf("scope %v on list: %v", scope, err)
		}
		if _, err := r.svc.Get(ctx, bad, other.ID); !errors.Is(err, cp.ErrNotFound) {
			t.Fatalf("scope %v on get: %v", scope, err)
		}
	}
}

// A guard ends an exec and a stream it admitted, as a quota guard does when
// a slot lapses; the record names its cause and the call fails with it.
func TestGuardEndsWhatItAdmitted(t *testing.T) {
	ctx := context.Background()
	r := newRigWith(t, nil, func(c *cp.Config) { c.Redact = func(s string) string { return s } })
	sb := r.ready(t)
	started := make(chan struct{}, 1)
	r.fake.ExecFunc = func(ctx context.Context, _ string, _ driver.ExecRequest) (driver.ExecResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return driver.ExecResult{}, ctx.Err()
	}
	lapsed := fmt.Errorf("%w: the workspace's exec slot lapsed", cp.ErrQuotaExceeded)
	cancels := make(chan func(error), 1)
	guarded(t, r, func(a *cp.Admission) (func(), error) {
		cancels <- a.Cancel
		return nil, nil
	})
	done := make(chan error, 1)
	go func() {
		_, _, err := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "sleep 600"}, cp.ExecIO{})
		done <- err
	}()
	cancel := <-cancels
	<-started
	cancel(lapsed)
	err := <-done
	if !errors.Is(err, cp.ErrEndedByGuard) || !errors.Is(err, cp.ErrQuotaExceeded) {
		t.Fatalf("exec ended with %v", err)
	}
	evs, _ := r.store.AuditFor(ctx, sb.ID)
	last := evs[len(evs)-1]
	if last.Action != "exec" || last.Outcome != cp.OutcomeFailed || last.Code != "quota_exceeded" ||
		!strings.Contains(last.Detail["reason"], "slot lapsed") {
		t.Fatalf("exec recorded as %+v", last)
	}
	cancel(lapsed) // once the exec has ended, nothing

	// A stream the guard ends, though fn itself returns no error.
	sctx, end := context.WithCancelCause(ctx)
	defer end(nil)
	_, err = r.svc.Stream(sctx, alice, sb.ID, cp.StreamRequest{Action: "attach", Capability: driver.CapAttach, End: end},
		func(driver.Driver, cp.Sandbox) (map[string]string, error) {
			(<-cancels)(nil)
			<-sctx.Done()
			return nil, nil
		})
	if !errors.Is(err, cp.ErrEndedByGuard) {
		t.Fatalf("stream ended with %v", err)
	}
	evs, _ = r.store.AuditFor(ctx, sb.ID)
	if last := evs[len(evs)-1]; last.Action != "attach" || last.Code != "forbidden" || last.Detail["reason"] != cp.ErrEndedByGuard.Error() {
		t.Fatalf("stream recorded as %+v", last)
	}

	// Verbs that end as they are admitted get a Cancel that does nothing.
	if _, err := r.svc.Stop(ctx, alice, sb.ID, "", 0); err != nil {
		t.Fatal(err)
	}
	(<-cancels)(lapsed)
}
