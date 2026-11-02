package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// CommitPos is a place in the record's commit order: the writing
// transaction, then time, session and sequence. The zero value is the start.
// Rows from before schema version 12 have transaction 0 and come first.
type CommitPos struct {
	Tx      uint64
	At      time.Time
	Session string
	Seq     int64
}

// String encodes the position for a cursor; "" is the start.
func (c CommitPos) String() string {
	if c.Tx == 0 && c.At.IsZero() && c.Session == "" && c.Seq == 0 {
		return ""
	}
	return strconv.FormatUint(c.Tx, 10) + "/" + strconv.FormatInt(c.At.UnixMicro(), 10) + "/" +
		strconv.FormatInt(c.Seq, 10) + "/" + c.Session
}

// ParseCommitPos reads a position String wrote.
func ParseCommitPos(s string) (CommitPos, error) {
	if s == "" {
		return CommitPos{}, nil
	}
	p := strings.SplitN(s, "/", 4)
	if len(p) != 4 {
		return CommitPos{}, fmt.Errorf("store: bad commit position %q", s)
	}
	tx, e1 := strconv.ParseUint(p[0], 10, 64)
	us, e2 := strconv.ParseInt(p[1], 10, 64)
	seq, e3 := strconv.ParseInt(p[2], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return CommitPos{}, fmt.Errorf("store: bad commit position %q", s)
	}
	return CommitPos{Tx: tx, At: time.UnixMicro(us).UTC(), Session: p[3], Seq: seq}, nil
}

// ScanCommitted streams up to limit of this tenant's events after pos, in
// commit order, to fn with each event's own position, opening sealed
// payloads as Scan does. Only rows whose transaction is older than every
// transaction still running are read, so a position, once passed, is never
// behind a row that commits later. A long-running transaction anywhere in
// the database holds the reader back; it never makes it skip.
func (p *Postgres) ScanCommitted(ctx context.Context, pos CommitPos, limit int, fn func(agent.Event, CommitPos) error) error {
	if limit <= 0 {
		limit = 500
	}
	// Rows from before schema version 12 have no transaction and come first,
	// by time; they are all committed.
	if pos.Tx == 0 {
		n, err := p.scanCommitted(ctx, `commit_tx IS NULL AND (created_at, session_id, seq) > ($1, $2, $3)
			ORDER BY created_at, session_id, seq LIMIT $4`, []any{pos.At, pos.Session, pos.Seq, limit}, fn)
		if err != nil || n == limit {
			return err
		}
		limit -= n
		pos = CommitPos{}
	}
	// The bare column, not a COALESCE of it, so under row security the bound
	// is an index condition (events_commit_tx_idx) rather than a filter over
	// every row of the tenant.
	_, err := p.scanCommitted(ctx, `commit_tx IS NOT NULL AND commit_tx < public.abhed_commit_horizon()
		AND commit_tx >= $1::text::xid8 AND (commit_tx, created_at, session_id, seq) > ($1::text::xid8, $2, $3, $4)
		ORDER BY commit_tx, created_at, session_id, seq LIMIT $5`,
		[]any{strconv.FormatUint(pos.Tx, 10), pos.At, pos.Session, pos.Seq, limit}, fn)
	return err
}

// scanCommitted gives fn the events where and its args select, in their
// order, and returns how many it gave.
func (p *Postgres) scanCommitted(ctx context.Context, where string, args []any, fn func(agent.Event, CommitPos) error) (int, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, session_id, COALESCE(parent_id,''), seq, type, payload, actor, trust, created_at,
		       COALESCE(commit_tx, '0'::xid8)::text
		FROM events WHERE `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("scan events in commit order: %w", err)
	}
	defer rows.Close()
	n := 0
	keys := p.readKeys()
	for rows.Next() {
		var ev agent.Event
		var payload []byte
		var typ, actor, trust, tx string
		if err := rows.Scan(&ev.ID, &ev.SessionID, &ev.ParentID, &ev.Seq,
			&typ, &payload, &actor, &trust, &ev.CreatedAt, &tx); err != nil {
			return n, err
		}
		ev.Type, ev.Actor, ev.Trust = agent.EventType(typ), agent.Actor(actor), agent.Trust(trust)
		if payload, err = keys.openPayload(ctx, ev.SessionID, ev.Seq, payload); err != nil {
			return n, err
		}
		ev.Payload = payload
		txn, err := strconv.ParseUint(tx, 10, 64)
		if err != nil {
			return n, fmt.Errorf("scan events in commit order: transaction id %q: %w", tx, err)
		}
		if err := fn(ev, CommitPos{Tx: txn, At: ev.CreatedAt, Session: ev.SessionID, Seq: ev.Seq}); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// PostgresOf is the Postgres store behind s: s itself, a Held store, or a
// store that wraps one and says so with PostgresStore. An edition's feature
// that needs Postgres finds it through any wrapper this way.
func PostgresOf(s any) (*Postgres, bool) {
	switch v := s.(type) {
	case *Postgres:
		return v, v != nil
	case *Held:
		return v.Postgres, v != nil && v.Postgres != nil
	case interface{ PostgresStore() *Postgres }:
		pg := v.PostgresStore()
		return pg, pg != nil
	}
	return nil, false
}

// errCommitOrderAhead is a restored record whose commit order is ahead of
// this cluster: abhed migrate has not run since the restore.
var errCommitOrderAhead = errors.New("the record's commit order is ahead of this database cluster, as after a " +
	"restore from another cluster: new events would sort before ones already exported. " +
	"Run `abhed migrate` as the owner before starting (see docs/guide/02-configuration.md)")

// checkCommitOrder refuses a database where some event's commit_tx is at or
// past where the next transaction would sort: a restore that skipped abhed
// migrate. It only reads, and assigns no transaction id; row security holds
// it to the server's tenant.
func checkCommitOrder(ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) error {
	var ahead bool
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(max(commit_tx)::text::numeric >=
		       pg_snapshot_xmax(pg_current_snapshot())::text::numeric + (SELECT shift FROM public.commit_shift), false)
		  FROM public.events WHERE commit_tx IS NOT NULL`).Scan(&ahead)
	if err != nil {
		return fmt.Errorf("check the record's commit order: %w", err)
	}
	if ahead {
		return errCommitOrderAhead
	}
	return nil
}
