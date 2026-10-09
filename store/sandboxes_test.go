package store

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/controlplane/storetest"
)

var cpTenant atomic.Int64

// sandboxStore opens the control plane's store as the runtime role, in a
// tenant of its own so every store starts empty.
func sandboxStore(t *testing.T) *Sandboxes {
	t.Helper()
	tenant := fmt.Sprintf("cp-%d-%d", time.Now().UnixNano(), cpTenant.Add(1))
	q, err := runtimeStore(t, tenant).Sandboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestSandboxStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) controlplane.Store { return sandboxStore(t) })
}

func TestSandboxesAreTenantScoped(t *testing.T) {
	a, b := sandboxStore(t), sandboxStore(t)
	ctx := context.Background()
	s, err := controlplane.NewSandbox("alice", "default", "process", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s, err = a.Create(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(ctx, s.ID); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("another tenant read the sandbox: %v", err)
	}
	if items, _, err := b.List(ctx, controlplane.ListOptions{}); err != nil || len(items) != 0 {
		t.Fatalf("another tenant listed %d: %v", len(items), err)
	}
	if _, err := b.Update(ctx, s, s.ResourceVersion); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("another tenant updated the sandbox: %v", err)
	}
	if err := b.Delete(ctx, s.ID, s.ResourceVersion); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("another tenant deleted the sandbox: %v", err)
	}
	if err := a.Append(ctx, controlplane.AuditEvent{At: time.Now(), Principal: "alice", Action: "create",
		SandboxID: s.ID, Generation: 1, Outcome: controlplane.OutcomeOK}); err != nil {
		t.Fatal(err)
	}
	if evs, err := b.AuditFor(ctx, s.ID); err != nil || len(evs) != 0 {
		t.Fatalf("another tenant read the record: %d %v", len(evs), err)
	}
}

// The runtime role writes the control plane's record and cannot rewrite it.
func TestSandboxAuditIsAppendOnlyForTheRuntimeRole(t *testing.T) {
	q := sandboxStore(t)
	ctx := context.Background()
	ev := controlplane.AuditEvent{At: time.Now().UTC(), Principal: "alice", Action: "exec", SandboxID: "sbx_00000000000000aa",
		Generation: 2, Outcome: controlplane.OutcomeOK, Detail: map[string]string{"exit_code": "0"}}
	if err := q.Append(ctx, ev); err != nil {
		t.Fatal(err)
	}
	for what, sql := range map[string]string{
		"rewrite an event": `UPDATE sandbox_audit SET principal = 'mallory'`,
		"delete an event":  `DELETE FROM sandbox_audit`,
		"truncate":         `TRUNCATE sandbox_audit`,
		"drop the trigger": `DROP TRIGGER sandbox_audit_no_update ON sandbox_audit`,
	} {
		if _, err := q.pool.Exec(ctx, sql); err == nil {
			t.Errorf("the runtime role could %s", what)
		}
	}
	evs, err := q.AuditFor(ctx, ev.SandboxID)
	if err != nil || len(evs) != 1 || evs[0].Principal != "alice" || evs[0].Detail["exit_code"] != "0" || evs[0].Generation != 2 {
		t.Fatalf("record after the attempts: %+v %v", evs, err)
	}
}
