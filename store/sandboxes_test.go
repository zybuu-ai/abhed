package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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

// A fence that refuses leaves every write as if it had not been asked, and
// runs in the write's own transaction.
func TestSandboxFenceRefusesInTheWritesTransaction(t *testing.T) {
	base := sandboxStore(t)
	ctx := context.Background()
	refuse := map[FenceOp]bool{}
	var seen []string
	q := base.WithFence(func(ctx context.Context, tx pgx.Tx, op FenceOp, id string) error {
		// The row being written is visible only inside its transaction.
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sandboxes WHERE id = $1`, id).Scan(&n); err != nil {
			return err
		}
		seen = append(seen, fmt.Sprintf("%s:%d", op, n))
		if refuse[op] {
			return fmt.Errorf("%w: fenced", controlplane.ErrUnavailable)
		}
		return nil
	})
	if q.Pool() != base.Pool() || q.Tenant() != base.Tenant() {
		t.Fatal("the fenced store is not on the same pool and tenant")
	}
	s, err := controlplane.NewSandbox("alice", "default", "process", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	refuse[FenceCreate] = true
	if _, err := q.Create(ctx, s); !errors.Is(err, controlplane.ErrUnavailable) {
		t.Fatalf("fenced create: %v", err)
	}
	if _, err := base.Get(ctx, s.ID); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("a fenced create was kept: %v", err)
	}
	refuse[FenceCreate] = false
	if s, err = q.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	refuse[FenceUpdate], refuse[FenceDelete] = true, true
	next := s.Clone()
	next.Template = "other"
	if _, err := q.Update(ctx, next, s.ResourceVersion); !errors.Is(err, controlplane.ErrUnavailable) {
		t.Fatalf("fenced update: %v", err)
	}
	if err := q.Delete(ctx, s.ID, s.ResourceVersion); !errors.Is(err, controlplane.ErrUnavailable) {
		t.Fatalf("fenced delete: %v", err)
	}
	if got, err := base.Get(ctx, s.ID); err != nil || got.ResourceVersion != s.ResourceVersion || got.Template != "default" {
		t.Fatalf("a fenced write changed the sandbox: %+v %v", got, err)
	}
	refuse[FenceUpdate], refuse[FenceDelete] = false, false
	if next, err = q.Update(ctx, next, s.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	if err := q.Delete(ctx, s.ID, next.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(seen, " "); got != "create:1 create:1 update:1 delete:0 update:1 delete:0" {
		t.Fatalf("fence calls %s", got)
	}
}
