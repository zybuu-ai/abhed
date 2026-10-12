package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// eventChainColumns are the columns of an event its hash covers, as the
// zz_events_chain trigger names them.
var eventChainColumns = []string{"id", "session_id", "tenant_id", "parent_id", "seq", "type", "payload", "actor", "trust", "created_at"}

// ChainReport is what VerifyChain found for one session's events.
type ChainReport struct {
	Session string
	// Rows is how many events are chained, and Head the hash of the last.
	Rows int64
	Head string
	// FirstBad is the first chain position whose link or hash does not hold;
	// zero when every one does.
	FirstBad int64
	// Unchained counts events written before the chain existed.
	Unchained int64
}

// OK reports whether every chained event checks out.
func (r ChainReport) OK() bool { return r.FirstBad == 0 }

// VerifyChain walks a session's event chain in the database. It shows a row
// edited, removed or put out of order by anyone short of the table's owner;
// an owner who recomputes the whole chain is caught only against a head kept
// elsewhere (CheckWitnessed).
func (p *Postgres) VerifyChain(ctx context.Context, sessionID string) (ChainReport, error) {
	r := ChainReport{Session: sessionID}
	err := p.pool.QueryRow(ctx, `SELECT first_bad, rows_chained, head, unchained
		FROM abhed_chain_verify('events', 'session_id', $1, $2)`, sessionID, eventChainColumns).
		Scan(&r.FirstBad, &r.Rows, &r.Head, &r.Unchained)
	if err != nil {
		return r, fmt.Errorf("verify the chain of %s: %w", sessionID, err)
	}
	return r, nil
}

// ChainSession is a session whose events a chain covers.
type ChainSession struct {
	ID       string
	Deleted  bool // erased by key; its rows and chain stay
	Subagent bool // a subagent's own session, listed under its parent
}

// ChainSessions lists every session of the tenant whose events are chained,
// newest first and at most limit: deleted and subagent sessions too, which
// ListSessions leaves out.
func (p *Postgres) ChainSessions(ctx context.Context, limit int) ([]ChainSession, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, deleted_at IS NOT NULL, parent_id IS NOT NULL
		FROM sessions ORDER BY started_at DESC, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list the sessions to verify: %w", err)
	}
	defer rows.Close()
	var out []ChainSession
	for rows.Next() {
		var s ChainSession
		if err := rows.Scan(&s.ID, &s.Deleted, &s.Subagent); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ChainHashAt is the hash of a session's event at chain position pos, and
// whether there is one, for checking a head a witness holds.
func (p *Postgres) ChainHashAt(ctx context.Context, sessionID string, pos int64) (string, bool, error) {
	var h string
	err := p.pool.QueryRow(ctx, `SELECT COALESCE(hash, '') FROM events WHERE session_id = $1 AND chain_pos = $2`,
		sessionID, pos).Scan(&h)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read the chain of %s: %w", sessionID, err)
	}
	return h, true, nil
}

// chainHead is a session's last chain position and hash.
func (p *Postgres) chainHead(ctx context.Context, sessionID string) (int64, string, error) {
	var pos int64
	var h string
	err := p.pool.QueryRow(ctx, `SELECT COALESCE(max(chain_pos), 0),
		COALESCE((SELECT hash FROM events WHERE session_id = $1 AND chain_pos IS NOT NULL ORDER BY chain_pos DESC LIMIT 1), '')
		FROM events WHERE session_id = $1`, sessionID).Scan(&pos, &h)
	return pos, h, err
}
