package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/store/chain"
)

// AuditChainName names the control plane's SQLite audit chain.
const AuditChainName = "sqlite/sandbox_audit"

// AuditLinks gives the chain links of the audit rows with these seqs; a row
// from before the chain has none.
func (s *Store) AuditLinks(ctx context.Context, seqs []int64) (map[int64]chain.Link, error) {
	out := map[int64]chain.Link{}
	if len(seqs) == 0 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(seqs)), ",")
	args := make([]any, len(seqs))
	for i, q := range seqs {
		args[i] = q
	}
	rows, err := s.r.QueryContext(ctx, `SELECT seq, chain_pos, coalesce(prev_hash, ''), coalesce(hash, '') FROM sandbox_audit
		WHERE chain_pos IS NOT NULL AND seq IN (`+marks+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read the audit chain's links: %w", err)
	}
	defer func() { _ = rows.Close() }()
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

// AuditRows gives the chained audit rows in chain order, each with its link.
func (s *Store) AuditRows(ctx context.Context, fn func(controlplane.AuditEvent, chain.Link) error) error {
	rows, err := s.r.QueryContext(ctx, `SELECT seq, at, principal, action, sandbox_id, generation, outcome, code, detail,
		chain_pos, coalesce(prev_hash, ''), coalesce(hash, '') FROM sandbox_audit WHERE chain_pos IS NOT NULL ORDER BY chain_pos`)
	if err != nil {
		return fmt.Errorf("read the audit record: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ev controlplane.AuditEvent
		var l chain.Link
		var at, detail string
		if err := rows.Scan(&ev.Seq, &at, &ev.Principal, &ev.Action, &ev.SandboxID, &ev.Generation,
			&ev.Outcome, &ev.Code, &detail, &l.Pos, &l.Prev, &l.Hash); err != nil {
			return err
		}
		if ev.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return fmt.Errorf("read the audit record: %w", err)
		}
		if err := json.Unmarshal([]byte(detail), &ev.Detail); err != nil {
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
func (s *Store) AuditChainName() string { return AuditChainName }
