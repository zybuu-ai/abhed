package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OCSFCursor is how far an OCSF sink has written one session.
type OCSFCursor struct {
	Seq      int64
	Provider string
	Primed   bool
}

// ErrOCSFStandby is OCSFWriter.Lock's answer while another server holds the
// writer's lock.
var ErrOCSFStandby = errors.New("another server writes the OCSF file")

// OCSFWriter is the right to write one OCSF sink: a session-level advisory
// lock on a connection of its own, which Postgres lets go of when that
// connection ends, so a server that dies hands the file to the next. Its
// cursors are rows of ocsf_cursors, read by whichever server takes over.
type OCSFWriter struct {
	p    *Postgres
	sink string
	key  string

	mu   sync.Mutex
	conn *pgxpool.Conn
}

// OCSFWriter returns the writer of sink, not yet holding its lock.
func (p *Postgres) OCSFWriter(sink string) *OCSFWriter {
	return &OCSFWriter{p: p, sink: sink, key: "abhed.ocsf:" + p.tenant + ":" + sink}
}

// Lock holds the writer's lock, taking it when free; ErrOCSFStandby while
// another server holds it. The lock stays held until Close.
func (w *OCSFWriter) Lock() (func(), error) {
	w.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if w.conn != nil {
		// Still ours only while the connection lives.
		if _, err := w.conn.Exec(ctx, "SELECT 1"); err == nil {
			return w.mu.Unlock, nil
		}
		w.conn.Release()
		w.conn = nil
	}
	conn, err := w.p.pool.Acquire(ctx)
	if err != nil {
		w.mu.Unlock()
		return nil, fmt.Errorf("ocsf: acquire a connection for the writer's lock: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", w.key).Scan(&got); err != nil {
		conn.Release()
		w.mu.Unlock()
		return nil, fmt.Errorf("ocsf: take the writer's lock: %w", err)
	}
	if !got {
		conn.Release()
		w.mu.Unlock()
		return nil, ErrOCSFStandby
	}
	w.conn = conn
	return w.mu.Unlock, nil
}

// Close lets go of the writer's lock.
func (w *OCSFWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = w.conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", w.key)
	w.conn.Release()
	w.conn = nil
}

// Load reads the sink's cursors, and whether it has any: a sink with none
// has not begun.
func (w *OCSFWriter) Load() (map[string]OCSFCursor, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := w.p.pool.Query(ctx, `SELECT session_id, seq, provider, primed FROM ocsf_cursors WHERE sink = $1`, w.sink)
	if err != nil {
		return nil, false, fmt.Errorf("ocsf: read the cursors (has abhed migrate run?): %w", err)
	}
	defer rows.Close()
	out := map[string]OCSFCursor{}
	begun := false
	for rows.Next() {
		var id string
		var c OCSFCursor
		if err := rows.Scan(&id, &c.Seq, &c.Provider, &c.Primed); err != nil {
			return nil, false, err
		}
		begun = true
		if id != "" {
			out[id] = c
		}
	}
	return out, begun, rows.Err()
}

// Save moves the cursors it names forward; one never moves back.
func (w *OCSFWriter) Save(moved map[string]OCSFCursor) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := w.p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if len(moved) == 0 {
		// The row with no session marks a sink that has begun, so one that
		// began on an empty record does not start again at the present end.
		moved = map[string]OCSFCursor{"": {}}
	}
	for id, c := range moved {
		if _, err := tx.Exec(ctx, `INSERT INTO ocsf_cursors (tenant_id, sink, session_id, seq, provider, primed)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant_id, sink, session_id) DO UPDATE
			SET seq = GREATEST(ocsf_cursors.seq, EXCLUDED.seq), provider = EXCLUDED.provider,
			    primed = EXCLUDED.primed, updated_at = now()`,
			w.p.tenant, w.sink, id, c.Seq, c.Provider, c.Primed); err != nil {
			return fmt.Errorf("ocsf: save the cursor of %s: %w", id, err)
		}
	}
	return tx.Commit(ctx)
}

// EventEnds is the last seq of each session with events, erased sessions
// left out: what a new OCSF writer compares the cursors with, once. With
// upTo set, only events at or before that place in commit order count.
func (p *Postgres) EventEnds(ctx context.Context, upTo *CommitPos) (map[string]int64, error) {
	q := `SELECT e.session_id, max(e.seq) FROM events e
		WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = e.session_id AND s.deleted_at IS NOT NULL)`
	var args []any
	if upTo != nil {
		q += ` AND (COALESCE(e.commit_tx, '0'::xid8), e.created_at, e.session_id, e.seq) <= ($1::text::xid8, $2, $3, $4)`
		args = append(args, strconv.FormatUint(upTo.Tx, 10), upTo.At, upTo.Session, upTo.Seq)
	}
	rows, err := p.pool.Query(ctx, q+` GROUP BY e.session_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list the sessions' last events: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var seq int64
		if err := rows.Scan(&id, &seq); err != nil {
			return nil, err
		}
		out[id] = seq
	}
	return out, rows.Err()
}

// LoadPos is the place in commit order the sink's writer reads on from, ""
// before one was saved.
func (w *OCSFWriter) LoadPos() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var pos string
	err := w.p.pool.QueryRow(ctx, `SELECT commit_pos FROM ocsf_cursors WHERE sink = $1 AND session_id = ''`, w.sink).Scan(&pos)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return pos, err
}

// SavePos keeps the place in commit order the writer has read to.
func (w *OCSFWriter) SavePos(pos string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := w.p.pool.Exec(ctx, `INSERT INTO ocsf_cursors (tenant_id, sink, session_id, seq, commit_pos)
		VALUES ($1, $2, '', 0, $3)
		ON CONFLICT (tenant_id, sink, session_id) DO UPDATE SET commit_pos = EXCLUDED.commit_pos, updated_at = now()`,
		w.p.tenant, w.sink, pos)
	return err
}

// CommitEnd is the place in commit order of the tenant's last committed
// event, where a reader that starts now begins; with no event, a place
// before every one that String does not write as "".
func (p *Postgres) CommitEnd(ctx context.Context) (CommitPos, error) {
	var pos CommitPos
	var tx string
	err := p.pool.QueryRow(ctx, `
		SELECT COALESCE(commit_tx, '0'::xid8)::text, created_at, session_id, seq FROM events
		WHERE COALESCE(commit_tx, '0'::xid8) < pg_snapshot_xmin(pg_current_snapshot())
		ORDER BY COALESCE(commit_tx, '0'::xid8) DESC, created_at DESC, session_id DESC, seq DESC
		LIMIT 1`).Scan(&tx, &pos.At, &pos.Session, &pos.Seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return CommitPos{At: time.Unix(0, 0).UTC()}, nil
	}
	if err != nil {
		return pos, err
	}
	pos.Tx, err = strconv.ParseUint(tx, 10, 64)
	return pos, err
}
