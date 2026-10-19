package store

import (
	"context"
	"fmt"
)

// auditChainColumns are the columns of a sandbox_audit row its hash covers,
// as the zz_sandbox_audit_chain trigger names them.
var auditChainColumns = []string{"seq", "tenant_id", "at", "principal", "action", "sandbox_id", "generation", "outcome", "code", "detail"}

// VerifyAuditChain walks the tenant's control-plane audit chain, which holds
// the sandbox operations and every mutating API request, in the database.
func (p *Postgres) VerifyAuditChain(ctx context.Context) (ChainReport, error) {
	r := ChainReport{Session: "sandbox_audit"}
	err := p.pool.QueryRow(ctx, `SELECT first_bad, rows_chained, head, unchained
		FROM abhed_chain_verify('sandbox_audit', 'tenant_id', $1, $2)`, p.tenant, auditChainColumns).
		Scan(&r.FirstBad, &r.Rows, &r.Head, &r.Unchained)
	if err != nil {
		return r, fmt.Errorf("verify the audit chain: %w", err)
	}
	return r, nil
}
