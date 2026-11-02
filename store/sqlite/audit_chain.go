package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/zybuu-ai/abhed/store/chain"
)

// genesis is the prev of the chain's first row.
var genesis = chain.Genesis

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

// sum is the row's hash, chain.LinkHash of the previous hash and the row's
// columns as JSON in the order above.
func (r auditRow) sum() (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return chain.LinkHash(r.Prev, string(b)), nil
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
	// hold, and Reason why; zero and empty when every one does.
	FirstBad int64
	Reason   string
	// Unchained counts rows written before the chain existed; one with no
	// chain written after it began fails the check instead.
	Unchained int64
}

// OK reports whether every chained row checks out.
func (r ChainReport) OK() bool { return r.FirstBad == 0 && r.Reason == "" }

// VerifyAuditChain walks the audit record's chain with chain.Walker. It shows
// a row edited, removed or put out of order; whoever can write the file can
// recompute the whole chain.
func (s *Store) VerifyAuditChain(ctx context.Context) (ChainReport, error) {
	var rep ChainReport
	var chained, late int64
	// A row with no chain after the chain's first was written with it bypassed.
	if err := s.r.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE chain_pos IS NULL), count(*) FILTER (WHERE chain_pos IS NOT NULL),
		count(*) FILTER (WHERE chain_pos IS NULL AND seq > (SELECT min(seq) FROM sandbox_audit WHERE chain_pos IS NOT NULL))
		FROM sandbox_audit`).Scan(&rep.Unchained, &chained, &late); err != nil {
		return rep, fmt.Errorf("verify the audit chain: %w", err)
	}
	rg, err := s.verifyRange(ctx, 1, 0)
	if err != nil {
		return rep, fmt.Errorf("verify the audit chain: %w", err)
	}
	rep.Rows, rep.Head, rep.FirstBad, rep.Reason = rg.To, rg.Hash, rg.FirstBad, rg.Reason
	if !rg.OK() {
		rep.Rows = rg.FirstBad - 1
	}
	if rep.OK() && chained != rep.Rows {
		rep.Reason = fmt.Sprintf("%d row(s) claim a chain position another holds", chained-rep.Rows)
	}
	if rep.OK() && late > 0 {
		rep.Reason = fmt.Sprintf("%d row(s) carry no chain though written after it began", late)
	}
	rep.Unchained -= late
	return rep, nil
}

// AuditChain is the audit record as a chain.Source. The file holds one
// chain, so the scope is not read; heads name the file as their scope.
func (s *Store) AuditChain() chain.Source { return auditSource{s} }

type auditSource struct{ s *Store }

func (a auditSource) Head(ctx context.Context, _ string) (chain.Head, error) {
	h := chain.Head{Chain: "sqlite/sandbox_audit", Scope: a.s.path}
	var hash sql.NullString
	err := a.s.r.QueryRowContext(ctx, `SELECT chain_pos, hash FROM sandbox_audit WHERE chain_pos IS NOT NULL
		ORDER BY chain_pos DESC LIMIT 1`).Scan(&h.Pos, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return h, nil
	}
	h.Hash = hash.String
	return h, err
}

// Links hands out the audit's links as stored (chain.Linker).
func (a auditSource) Links(ctx context.Context, _ string, from, to int64, fn func(chain.Link) bool) error {
	if from < 1 {
		from = 1
	}
	if to <= 0 {
		to = math.MaxInt64
	}
	return a.s.links(ctx, from, to, fn)
}

// links reads positions from to to, each row as its hash covers it.
func (s *Store) links(ctx context.Context, from, to int64, fn func(chain.Link) bool) error {
	rows, err := s.r.QueryContext(ctx, `SELECT chain_pos, at, principal, action, sandbox_id, generation, outcome, code, detail,
		coalesce(prev_hash, ''), coalesce(hash, '') FROM sandbox_audit WHERE chain_pos >= ? AND chain_pos <= ? ORDER BY chain_pos`, from, to)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r auditRow
		var l chain.Link
		if err := rows.Scan(&r.Pos, &r.At, &r.Principal, &r.Action, &r.SandboxID, &r.Generation, &r.Outcome,
			&r.Code, &r.Detail, &l.Prev, &l.Hash); err != nil {
			return err
		}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		l.Pos, l.Row = r.Pos, string(b)
		if !fn(l) {
			break
		}
	}
	return rows.Err()
}

func (a auditSource) VerifyRange(ctx context.Context, _ string, from, to int64) (chain.Range, error) {
	return a.s.verifyRange(ctx, from, to)
}

// verifyRange recomputes positions from to to (0 is the last).
func (s *Store) verifyRange(ctx context.Context, from, to int64) (chain.Range, error) {
	if from < 1 {
		from = 1
	}
	before := genesis
	if from > 1 {
		err := s.r.QueryRowContext(ctx, `SELECT coalesce(hash, '') FROM sandbox_audit WHERE chain_pos = ?`, from-1).Scan(&before)
		if errors.Is(err, sql.ErrNoRows) {
			return chain.Range{From: from, To: to, FirstBad: from - 1,
				Reason: fmt.Sprintf("position %d, which the range links to, is missing", from-1)}, nil
		}
		if err != nil {
			return chain.Range{}, err
		}
	}
	hi := to
	if hi <= 0 {
		hi = math.MaxInt64
	}
	w := chain.NewWalker(from, before, nil)
	if err := s.links(ctx, from, hi, w.Next); err != nil {
		return chain.Range{}, err
	}
	return w.Done(to), nil
}
