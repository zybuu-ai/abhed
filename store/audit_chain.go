package store

import (
	"context"
	"fmt"
)

// VerifyAuditChain walks the tenant's control-plane audit chain, which holds
// the sandbox operations and every mutating API request, recomputing each
// hash here as VerifyChain does for a session's events.
func (p *Postgres) VerifyAuditChain(ctx context.Context) (ChainReport, error) {
	r, err := p.verifyChainOf(ctx, auditChain, p.tenant)
	if err != nil {
		return r, fmt.Errorf("verify the audit chain: %w", err)
	}
	r.Session = "sandbox_audit"
	return r, nil
}
