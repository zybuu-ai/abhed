package store

import (
	"context"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
)

// API audit events go to the tenant's sandbox_audit chain, which verifies
// as a whole and breaks where an owner edits a row.
func TestAuditChainVerifies(t *testing.T) {
	tenant := testID(t, "audit-chain-")
	p := openStore(t, tenant)
	ctx := context.Background()
	q, err := p.Sandboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []string{controlplane.OutcomeRequested, controlplane.OutcomeOK, controlplane.OutcomeRequested} {
		if err := q.Append(ctx, controlplane.AuditEvent{At: time.Now(), Principal: "alice", Action: "api:POST /v1/sessions",
			Outcome: o, Detail: map[string]string{"route": "POST /v1/sessions"}}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := p.VerifyAuditChain(ctx)
	if err != nil || !r.OK() || r.Rows != 3 || len(r.Head) != 64 {
		t.Fatalf("verify = %+v, %v; want 3 chained rows", r, err)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, s := range []string{
		`ALTER TABLE sandbox_audit DISABLE TRIGGER sandbox_audit_no_update`,
		`UPDATE sandbox_audit SET principal = 'mallory' WHERE tenant_id = '` + tenant + `' AND chain_pos = 2`,
		`ALTER TABLE sandbox_audit ENABLE TRIGGER sandbox_audit_no_update`,
	} {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	var bad int64
	if err := tx.QueryRow(ctx, `SELECT first_bad FROM abhed_chain_verify('sandbox_audit', 'tenant_id', $1, $2)`,
		tenant, auditChainColumns).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 2 {
		t.Fatalf("first bad position %d, want 2", bad)
	}
}
