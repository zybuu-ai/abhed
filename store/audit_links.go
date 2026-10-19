package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/store/chain"
)

// AuditChainName names the tenant's control-plane audit chain in Postgres.
const AuditChainName = "postgres/sandbox_audit"

// AuditLinks gives the chain links of the tenant's audit rows with these
// seqs; a row from before the chain has none.
func (q *Sandboxes) AuditLinks(ctx context.Context, seqs []int64) (map[int64]chain.Link, error) {
	return auditLinks(ctx, q.pool.Query, seqs)
}

// AuditLinks is Sandboxes.AuditLinks on the store's own connections.
func (p *Postgres) AuditLinks(ctx context.Context, seqs []int64) (map[int64]chain.Link, error) {
	return auditLinks(ctx, p.pool.Query, seqs)
}

// queryFunc is a pool's Query.
type queryFunc func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)

func auditLinks(ctx context.Context, query queryFunc, seqs []int64) (map[int64]chain.Link, error) {
	out := map[int64]chain.Link{}
	if len(seqs) == 0 {
		return out, nil
	}
	rows, err := query(ctx, `SELECT seq, chain_pos, COALESCE(prev_hash, ''), COALESCE(hash, '') FROM sandbox_audit
		WHERE seq = ANY($1) AND chain_pos IS NOT NULL`, seqs)
	if err != nil {
		return nil, fmt.Errorf("read the audit chain's links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var l chain.Link
		if err := rows.Scan(&seq, &l.Pos, &l.Prev, &l.Hash); err != nil {
			return nil, err
		}
		out[seq] = l
	}
	return out, rows.Err()
}

// AuditRows gives the tenant's chained audit rows in chain order, each with
// its link: the sandbox operations and every mutating API request.
func (p *Postgres) AuditRows(ctx context.Context, fn func(controlplane.AuditEvent, chain.Link) error) error {
	rows, err := p.pool.Query(ctx, `
		SELECT seq, at, principal, action, sandbox_id, generation, outcome, code, detail,
		       chain_pos, COALESCE(prev_hash, ''), COALESCE(hash, '')
		FROM sandbox_audit WHERE chain_pos IS NOT NULL ORDER BY chain_pos`)
	if err != nil {
		return fmt.Errorf("read the audit record: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ev controlplane.AuditEvent
		var l chain.Link
		var detail []byte
		if err := rows.Scan(&ev.Seq, &ev.At, &ev.Principal, &ev.Action, &ev.SandboxID, &ev.Generation,
			&ev.Outcome, &ev.Code, &detail, &l.Pos, &l.Prev, &l.Hash); err != nil {
			return err
		}
		if err := json.Unmarshal(detail, &ev.Detail); err != nil {
			return err
		}
		if len(ev.Detail) == 0 {
			ev.Detail = nil
		}
		if err := fn(ev, l); err != nil {
			return err
		}
	}
	return rows.Err()
}

// AuditChainName names the chain AuditLinks reads.
func (q *Sandboxes) AuditChainName() string { return AuditChainName }
