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

// Load reads the cursors of the sessions named, or of every session when
// ids is nil, and whether the sink has any row at all: a sink with none has
// not begun.
func (w *OCSFWriter) Load(ids []string) (map[string]OCSFCursor, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var begun bool
	if err := w.p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ocsf_cursors WHERE sink = $1)`, w.sink).Scan(&begun); err != nil {
		return nil, false, fmt.Errorf("ocsf: read the cursors (has abhed migrate run?): %w", err)
	}
	out := map[string]OCSFCursor{}
	if !begun || (ids != nil && len(ids) == 0) {
		return out, begun, nil
	}
	q, args := `SELECT session_id, seq, provider, primed FROM ocsf_cursors WHERE sink = $1 AND session_id <> ''`, []any{w.sink}
	if ids != nil {
		q += ` AND session_id = ANY($2)`
		args = append(args, ids)
	}
	rows, err := w.p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("ocsf: read the cursors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var c OCSFCursor
		if err := rows.Scan(&id, &c.Seq, &c.Provider, &c.Primed); err != nil {
			return nil, false, err
		}
		out[id] = c
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

// SavePos keeps the place in commit order the writer has read to. It only
// moves forward: a writer that lost the sink to another and saves late
// cannot take the place back, which would export rows a second time. The
// comparison is made in the statement, in the scan's order (transaction,
// time, session, seq), so two writers cannot interleave one.
func (w *OCSFWriter) SavePos(pos string) error {
	c, err := ParseCommitPos(pos)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if pos == "" {
		_, err = w.p.pool.Exec(ctx, `INSERT INTO ocsf_cursors (tenant_id, sink, session_id, seq, commit_pos)
			VALUES ($1, $2, '', 0, '') ON CONFLICT (tenant_id, sink, session_id) DO NOTHING`, w.p.tenant, w.sink)
		return err
	}
	// A stored place that cannot be read counts as older and is replaced, so
	// it cannot stop the writer saving for good; it is logged.
	var stored string
	if err := w.p.pool.QueryRow(ctx, `SELECT commit_pos FROM ocsf_cursors WHERE sink = $1 AND session_id = ''`, w.sink).Scan(&stored); err == nil && stored != "" {
		if _, perr := ParseCommitPos(stored); perr != nil {
			w.p.logger().Warn("OCSF cursor: the saved place cannot be read and is replaced", "sink", w.sink, "error", perr)
		}
	}
	_, err = w.p.pool.Exec(ctx, `INSERT INTO ocsf_cursors (tenant_id, sink, session_id, seq, commit_pos)
		VALUES ($1, $2, '', 0, $3)
		ON CONFLICT (tenant_id, sink, session_id) DO UPDATE SET commit_pos = EXCLUDED.commit_pos, updated_at = now()
		WHERE CASE WHEN ocsf_cursors.commit_pos !~ '^[0-9]{1,20}/-?[0-9]{1,18}/-?[0-9]{1,18}/' THEN true
		      ELSE (split_part(ocsf_cursors.commit_pos, '/', 1)::numeric, split_part(ocsf_cursors.commit_pos, '/', 2)::bigint,
		            regexp_replace(ocsf_cursors.commit_pos, '^([^/]*/){3}', ''), split_part(ocsf_cursors.commit_pos, '/', 3)::bigint)
		           < ($4::numeric, $5::bigint, $6::text, $7::bigint) END`,
		w.p.tenant, w.sink, pos, strconv.FormatUint(c.Tx, 10), c.At.UnixMicro(), c.Session, c.Seq)
	return err
}

// CommitEnd is the place in commit order of the tenant's last committed
// event, where a reader that starts now begins; with no event, a place
// before every one that String does not write as "".
func (p *Postgres) CommitEnd(ctx context.Context) (CommitPos, error) {
	var pos CommitPos
	var tx string
	// The last row with a transaction, by the same index ScanCommitted reads;
	// with none, the last from before schema version 12.
	err := p.pool.QueryRow(ctx, `
		SELECT commit_tx::text AS tx, created_at, session_id, seq FROM events
		WHERE commit_tx IS NOT NULL AND commit_tx < pg_snapshot_xmin(pg_current_snapshot())
		ORDER BY commit_tx DESC, created_at DESC, session_id DESC, seq DESC
		LIMIT 1`).Scan(&tx, &pos.At, &pos.Session, &pos.Seq)
	if errors.Is(err, pgx.ErrNoRows) {
		tx = "0"
		err = p.pool.QueryRow(ctx, `SELECT created_at, session_id, seq FROM events WHERE commit_tx IS NULL
			ORDER BY created_at DESC, session_id DESC, seq DESC LIMIT 1`).Scan(&pos.At, &pos.Session, &pos.Seq)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return CommitPos{At: time.Unix(0, 0).UTC()}, nil
	}
	if err != nil {
		return pos, err
	}
	pos.Tx, err = strconv.ParseUint(tx, 10, 64)
	return pos, err
}
