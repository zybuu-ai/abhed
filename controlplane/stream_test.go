package controlplane_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	cp "github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
)

func TestStreamRecordsScreensAndAdmits(t *testing.T) {
	r := newCapRigWith(t, nil, func(c *cp.Config) {
		c.Redact = func(s string) string { return strings.ReplaceAll(s, "hunter2", "[secret]") }
	},
		driver.CapAttach)
	sb := r.ready(t)
	ctx := context.Background()
	released := false
	g := guarded(t, r, func(*cp.Admission) (func(), error) { return func() { released = true }, nil })

	// The detail fn returns joins the outcome; the guard is held across fn.
	_, err := r.svc.Stream(ctx, alice, sb.ID, cp.StreamRequest{Action: "attach", Capability: driver.CapAttach,
		Detail: map[string]string{"rows": "24"}}, func(d driver.Driver, got cp.Sandbox) (map[string]string, error) {
		if released || got.ID != sb.ID || d == nil {
			t.Fatalf("fn ran outside the admission: released=%v", released)
		}
		return map[string]string{"exit_code": "3"}, nil
	})
	if err != nil || !released {
		t.Fatalf("stream: %v released=%v", err, released)
	}
	if a := g.last(); a.Action != cp.ActionExec || a.Exec == nil || a.Exec.Command != ":attach rows=24" {
		t.Fatalf("guard asked %+v", a)
	}
	evs, _ := r.store.AuditFor(ctx, sb.ID)
	last := evs[len(evs)-1]
	if last.Action != "attach" || last.Outcome != cp.OutcomeOK || last.Principal != "alice" || last.Detail["exit_code"] != "3" || last.Detail["rows"] != "24" {
		t.Fatalf("outcome %+v", last)
	}

	// A screen refuses after the request is recorded and before the guard or fn.
	asked := len(g.seen)
	called := false
	_, err = r.svc.Stream(ctx, alice, sb.ID, cp.StreamRequest{Action: "attach", Capability: driver.CapAttach,
		Screen: func() error { return cp.ErrForbidden }}, func(driver.Driver, cp.Sandbox) (map[string]string, error) {
		called = true
		return nil, nil
	})
	if !errors.Is(err, cp.ErrForbidden) || called || len(g.seen) != asked {
		t.Fatalf("screened: %v called=%v", err, called)
	}
	if got := outcomes(t, r, sb.ID); !strings.HasSuffix(got, "attach:requested:,attach:failed:forbidden") {
		t.Fatalf("record %s", got)
	}

	// Another owner's sandbox is not found, and nothing is recorded for it.
	before := len(actions(t, r, sb.ID))
	if _, err := r.svc.Stream(ctx, cp.Caller{Principal: "bob"}, sb.ID, cp.StreamRequest{Action: "attach"},
		func(driver.Driver, cp.Sandbox) (map[string]string, error) { return nil, nil }); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("bob: %v", err)
	}
	if len(actions(t, r, sb.ID)) != before {
		t.Fatal("a refused caller's stream was recorded")
	}

	// A note is redacted where asked.
	if err := r.svc.Note(ctx, alice, sb, "attach.input", "", map[string]string{"line": "echo hunter2", "terminal": "t1"}, "line"); err != nil {
		t.Fatal(err)
	}
	evs, _ = r.store.AuditFor(ctx, sb.ID)
	last = evs[len(evs)-1]
	if last.Detail["line"] != "echo [secret]" || last.Detail["terminal"] != "t1" || last.Outcome != cp.OutcomeOK {
		t.Fatalf("note %+v", last)
	}
}

func TestExecScreen(t *testing.T) {
	r := newCapRig(t, nil)
	sb := r.ready(t)
	n := countExecs(r)
	screen := func(cmd string) error {
		if strings.Contains(cmd, "rm") {
			return cp.ErrForbidden
		}
		return nil
	}
	ctx := context.Background()
	if _, _, err := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Argv: []string{"rm", "-rf", "x"}}, cp.ExecIO{Screen: screen}); !errors.Is(err, cp.ErrForbidden) {
		t.Fatalf("screened exec: %v", err)
	}
	if n.Load() != 0 {
		t.Fatal("a screened command ran")
	}
	if got := outcomes(t, r, sb.ID); !strings.HasSuffix(got, "exec:requested:,exec:failed:forbidden") {
		t.Fatalf("record %s", got)
	}
	if _, _, err := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "ls"}, cp.ExecIO{Screen: screen}); err != nil || n.Load() != 1 {
		t.Fatalf("allowed exec: %v %d", err, n.Load())
	}
}

// A refusal's reason is recorded, redacted and bounded, so the record says
// why a command did not run.
func TestExecScreenRecordsTheReason(t *testing.T) {
	r := newCapRigWith(t, nil, func(c *cp.Config) {
		c.Redact = func(s string) string { return strings.ReplaceAll(s, "hunter2", "[secret]") }
	})
	sb := r.ready(t)
	ctx := context.Background()
	screen := func(cmd string) error {
		return fmt.Errorf("%w: denied by policy: %s%s", cp.ErrForbidden, cmd, strings.Repeat("x", 2000))
	}
	if _, _, err := r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "echo hunter2"}, cp.ExecIO{Screen: screen}); !errors.Is(err, cp.ErrForbidden) {
		t.Fatalf("screened exec: %v", err)
	}
	evs, _ := r.store.AuditFor(ctx, sb.ID)
	why := evs[len(evs)-1].Detail["reason"]
	if !strings.HasPrefix(why, "denied by policy: echo [secret]x") || len(why) > 512 {
		t.Fatalf("reason %q", why)
	}
	// A bare refusal adds nothing to the code.
	bare := func(string) error { return cp.ErrForbidden }
	_, _, _ = r.svc.Exec(ctx, alice, sb.ID, "", cp.ExecRequest{Command: "ls"}, cp.ExecIO{Screen: bare})
	evs, _ = r.store.AuditFor(ctx, sb.ID)
	if why, ok := evs[len(evs)-1].Detail["reason"]; ok {
		t.Fatalf("bare refusal reason %q", why)
	}
}
