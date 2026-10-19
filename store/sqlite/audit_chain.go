package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// genesis is the prev of the chain's first row.
var genesis = strings.Repeat("0", 64)

// auditRow is a sandbox_audit row as its hash covers it.
type auditRow struct {
	Pos        int64  `json:"chain_pos"`
	At         string `json:"at"`
	Principal  string `json:"principal"`
	Action     string `json:"action"`
	SandboxID  string `json:"sandbox_id"`
	Generation int64  `json:"generation"`
	Outcome    string `json:"outcome"`
	Code       string `json:"code"`
	Detail     string `json:"detail"`
	Prev       string `json:"-"`
	Hash       string `json:"-"`
}

// sum is the row's hash: sha256 of the previous hash, a newline, and the
// row's columns as JSON in the order above.
func (r auditRow) sum() (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(append([]byte(r.Prev+"\n"), b...))
	return hex.EncodeToString(h[:]), nil
}

// link sets the row's place in the chain, after the last chained row; the
// write transaction is immediate, so no other writer links between.
func (r *auditRow) link(ctx context.Context, tx *sql.Tx) error {
	var pos sql.NullInt64
	var hash sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT chain_pos, hash FROM sandbox_audit
		WHERE chain_pos IS NOT NULL ORDER BY chain_pos DESC LIMIT 1`).Scan(&pos, &hash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	r.Pos, r.Prev = 1, genesis
	if pos.Valid {
		r.Pos, r.Prev = pos.Int64+1, hash.String
	}
	r.Hash, err = r.sum()
	return err
}

// ChainReport is what VerifyAuditChain found.
type ChainReport struct {
	Rows int64
	Head string
	// FirstBad is the first chain position whose link or hash does not
	// hold; zero when every one does.
	FirstBad int64
	// Unchained counts rows written before the chain existed.
	Unchained int64
}

// OK reports whether every chained row checks out.
func (r ChainReport) OK() bool { return r.FirstBad == 0 }

// VerifyAuditChain walks the audit record's chain. It shows a row edited,
// removed or put out of order; whoever can write the file can recompute the
// whole chain.
func (s *Store) VerifyAuditChain(ctx context.Context) (ChainReport, error) {
	var rep ChainReport
	if err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM sandbox_audit WHERE chain_pos IS NULL`).Scan(&rep.Unchained); err != nil {
		return rep, fmt.Errorf("verify the audit chain: %w", err)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT chain_pos, at, principal, action, sandbox_id, generation, outcome, code, detail,
		coalesce(prev_hash, ''), coalesce(hash, '') FROM sandbox_audit WHERE chain_pos IS NOT NULL ORDER BY chain_pos`)
	if err != nil {
		return rep, fmt.Errorf("verify the audit chain: %w", err)
	}
	defer func() { _ = rows.Close() }()
	prev, want := genesis, int64(1)
	for rows.Next() {
		var r auditRow
		var stored string
		if err := rows.Scan(&r.Pos, &r.At, &r.Principal, &r.Action, &r.SandboxID, &r.Generation, &r.Outcome,
			&r.Code, &r.Detail, &r.Prev, &stored); err != nil {
			return rep, err
		}
		linked := r.Prev == prev
		r.Prev = prev
		got, err := r.sum()
		if err != nil {
			return rep, err
		}
		if rep.FirstBad == 0 && (r.Pos != want || !linked || got != stored) {
			rep.FirstBad = want
		}
		prev, want = stored, r.Pos+1
		rep.Rows++
		rep.Head = stored
	}
	return rep, rows.Err()
}
