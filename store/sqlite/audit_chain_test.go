package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
)

// The control plane's own record, the reconciler's events among them, is
// chained; an edit by whoever can write the file breaks it at that row.
func TestAuditChainShowsAnEditedRow(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for i, o := range []string{"requested", "ok", "ok", "failed"} {
		if err := s.Append(ctx, controlplane.AuditEvent{At: time.Now(), Principal: controlplane.SystemPrincipal,
			Action: "reconcile", SandboxID: "sb1", Generation: int64(i), Outcome: o, Detail: map[string]string{"k": "v"}}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.VerifyAuditChain(ctx)
	if err != nil || !r.OK() || r.Rows != 4 || len(r.Head) != 64 || r.Unchained != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	// What only the file's owner can do: the append-only trigger dropped.
	if _, err := s.w.Exec(`DROP TRIGGER sandbox_audit_no_update;
		UPDATE sandbox_audit SET principal = 'mallory' WHERE chain_pos = 3`); err != nil {
		t.Fatal(err)
	}
	if r, err := s.VerifyAuditChain(ctx); err != nil || r.FirstBad != 3 {
		t.Fatalf("edit not found at 3: %+v %v", r, err)
	}
	if _, err := s.w.Exec(`UPDATE sandbox_audit SET principal = 'system:reconciler' WHERE chain_pos = 3;
		UPDATE sandbox_audit SET chain_pos = chain_pos + 10 WHERE chain_pos = 2`); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.VerifyAuditChain(ctx); r.OK() {
		t.Fatal("a row out of place verified")
	}
}

// Rows from before the chain are counted, not failed.
func TestAuditChainCountsUnchainedRows(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	if _, err := s.w.Exec(`INSERT INTO sandbox_audit (at, principal, action, sandbox_id, generation, outcome)
		VALUES ('2026-01-01T00:00:00Z', 'a', 'create', 'sb', 1, 'ok')`); err != nil {
		t.Fatal(err)
	}
	_ = s.Append(ctx, controlplane.AuditEvent{At: time.Now(), Principal: "a", Action: "delete", SandboxID: "sb", Outcome: "ok"})
	if r, err := s.VerifyAuditChain(ctx); err != nil || !r.OK() || r.Unchained != 1 || r.Rows != 1 {
		t.Fatalf("%+v %v", r, err)
	}
}
