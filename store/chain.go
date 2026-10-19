package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zybuu-ai/abhed/store/chain"
)

// eventChainColumns are the columns of an event its hash covers, as the
// zz_events_chain trigger names them.
var eventChainColumns = []string{"id", "session_id", "tenant_id", "parent_id", "seq", "type", "payload", "actor", "trust", "created_at"}

// auditChainColumns are the sandbox audit's, as zz_sandbox_audit_chain names them.
var auditChainColumns = []string{"seq", "tenant_id", "at", "principal", "action", "sandbox_id", "generation", "outcome", "code", "detail"}

// A chained table. Events have one chain per session, so sessions never wait
// on each other's lock and a session's chain stays whole through its erasure;
// the sandbox audit has one per tenant, being one stream of the control plane's.
type chainTable struct {
	name, key, kind string
	cols            []string
}

var (
	eventsChain = chainTable{name: "events", key: "session_id", kind: "postgres/events", cols: eventChainColumns}
	auditChain  = chainTable{name: "sandbox_audit", key: "tenant_id", kind: "postgres/sandbox_audit", cols: auditChainColumns}
)

// chainPage is how many links one query reads; a test pins it lower.
var chainPage = 5000

// ChainReport is what VerifyChain found for one session's events.
type ChainReport struct {
	Session string
	// Rows is how many events are chained and hold, and Head the hash of the last.
	Rows int64
	Head string
	// FirstBad is the first chain position whose link or hash does not hold;
	// zero when every one does. Reason says why the check failed.
	FirstBad int64
	Reason   string
	// Unchained counts events with no chain; PreChain how many of them a
	// migration found already there when the chain began.
	Unchained int64
	PreChain  int64
}

// OK reports whether every chained event checks out and every unchained one
// is from before the chain.
func (r ChainReport) OK() bool { return r.FirstBad == 0 && r.Reason == "" }

// VerifyChain walks a session's event chain, recomputing each hash here from
// the rows rather than trusting a function in the database. It shows a row
// edited, removed or put out of order, and a row written with the chain's
// trigger off; an owner who recomputes the whole chain, or cuts its end, is
// caught only against a head kept elsewhere.
func (p *Postgres) VerifyChain(ctx context.Context, sessionID string) (ChainReport, error) {
	r, err := p.verifyChainOf(ctx, eventsChain, sessionID)
	if err != nil {
		return r, fmt.Errorf("verify the chain of %s: %w", sessionID, err)
	}
	return r, nil
}

// verifyChainOf is the whole check of one chain: every link recomputed, and
// every row without one accounted for as from before the chain.
func (p *Postgres) verifyChainOf(ctx context.Context, t chainTable, key string) (ChainReport, error) {
	r := ChainReport{Session: key}
	rg, err := p.verifyRange(ctx, t, key, 1, 0)
	if err != nil {
		return r, err
	}
	r.Rows, r.Head, r.FirstBad, r.Reason = rg.To, rg.Hash, rg.FirstBad, rg.Reason
	if !rg.OK() {
		r.Rows = rg.FirstBad - 1
	}
	var chained int64
	if r.Unchained, r.PreChain, chained, err = p.unchained(ctx, t, key); err != nil {
		return r, err
	}
	// A second row at a position the walk read is in no chain.
	if r.OK() && chained != r.Rows {
		r.Reason = fmt.Sprintf("%d row(s) claim a chain position another holds", chained-r.Rows)
	}
	if r.OK() && r.Unchained > r.PreChain {
		r.Reason = fmt.Sprintf("%d row(s) carry no chain and were not there when it began: they were written with its trigger off",
			r.Unchained-r.PreChain)
	}
	return r, nil
}

// unchained counts a chain's rows with no chain, those marked as from
// before it, and the rows that have one.
func (p *Postgres) unchained(ctx context.Context, t chainTable, key string) (n, pre, chained int64, err error) {
	err = p.pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FILTER (WHERE chain_pos IS NULL), count(*) FILTER (WHERE chain_pos IS NOT NULL),
		COALESCE((SELECT pre_rows FROM chain_pre WHERE tbl = $2 AND key = $1), 0)
		FROM %s WHERE %s = $1`, t.name, t.key), key, t.name).Scan(&n, &chained, &pre) // #nosec G201 -- names from chainTable, not input
	return n, pre, chained, err
}

// EventChain is the per-session chain of events, as a chain.Source whose
// scope is a session id.
func (p *Postgres) EventChain() chain.Source { return pgChain{p, eventsChain} }

// AuditChain is the control plane's sandbox audit, one chain per tenant, as a
// chain.Source whose scope is this store's tenant.
func (p *Postgres) AuditChain() chain.Source { return pgChain{p, auditChain} }

type pgChain struct {
	p *Postgres
	t chainTable
}

func (c pgChain) scoped(scope string) error {
	if c.t.key == "tenant_id" && scope != c.p.tenant {
		return fmt.Errorf("the %s chain of tenant %q is not this store's (%q)", c.t.name, scope, c.p.tenant)
	}
	return nil
}

func (c pgChain) Head(ctx context.Context, scope string) (chain.Head, error) {
	if err := c.scoped(scope); err != nil {
		return chain.Head{}, err
	}
	pos, h, err := c.p.head(ctx, c.t, scope)
	return chain.Head{Chain: c.t.kind, Tenant: c.p.tenant, Scope: scope, Pos: pos, Hash: h}, err
}

func (c pgChain) VerifyRange(ctx context.Context, scope string, from, to int64) (chain.Range, error) {
	if err := c.scoped(scope); err != nil {
		return chain.Range{}, err
	}
	return c.p.verifyRange(ctx, c.t, scope, from, to)
}

// verifyRange recomputes positions from to to (0 is the head) of one chain.
func (p *Postgres) verifyRange(ctx context.Context, t chainTable, key string, from, to int64) (chain.Range, error) {
	if from < 1 {
		from = 1
	}
	before := chain.Genesis
	if from > 1 {
		h, ok, err := p.hashAt(ctx, t, key, from-1)
		if err != nil {
			return chain.Range{}, err
		}
		if !ok {
			return chain.Range{From: from, To: to, FirstBad: from - 1,
				Reason: fmt.Sprintf("position %d, which the range links to, is missing", from-1)}, nil
		}
		before = h
	}
	w := chain.NewWalker(from, before, nil)
	if err := p.walk(ctx, t, key, from, to, w.Next); err != nil {
		return chain.Range{}, err
	}
	return w.Done(to), nil
}

// walk gives fn each link of a chain from position from to to (0 is its
// last), in order, until fn returns false. Each row is rendered as the
// chain's trigger hashed it, with only the database's own JSON functions and
// in UTC as the trigger's were; the hashing is done by the caller. Pages are
// keyed on the last position read, so a forged position far past the others
// costs one page, not a walk to it.
func (p *Postgres) walk(ctx context.Context, t chainTable, key string, from, to int64, fn func(chain.Link) bool) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL timezone = 'UTC'`); err != nil {
		return err
	}
	if to <= 0 {
		to = math.MaxInt64
	}
	q := fmt.Sprintf(`SELECT chain_pos, COALESCE(prev_hash, ''), COALESCE(hash, ''),
		COALESCE((SELECT jsonb_object_agg(e.key, e.value) FROM jsonb_each(to_jsonb(t)) e WHERE e.key = ANY($4::text[])), '{}'::jsonb)::text
		FROM %s t WHERE %s = $1 AND chain_pos >= $2 AND chain_pos <= $3 ORDER BY chain_pos LIMIT $5`, t.name, t.key) // #nosec G201 -- names from chainTable
	for lo := from; ; {
		rows, err := tx.Query(ctx, q, key, lo, to, t.cols, chainPage)
		if err != nil {
			return err
		}
		n, more := 0, true
		for more && rows.Next() {
			var l chain.Link
			if err := rows.Scan(&l.Pos, &l.Prev, &l.Hash, &l.Row); err != nil {
				rows.Close()
				return err
			}
			n++
			lo = l.Pos + 1
			more = fn(l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if !more || n < chainPage || lo > to || lo <= 0 {
			return nil
		}
	}
}

// ChainSession is a session whose events a chain covers.
type ChainSession struct {
	ID       string
	Deleted  bool // erased by key; its rows and chain stay
	Subagent bool // a subagent's own session, listed under its parent
}

// ChainSessions lists the tenant's chained sessions, newest first and at most
// limit, with the deleted and subagent ones ListSessions leaves out.
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
	h, ok, err := p.hashAt(ctx, eventsChain, sessionID, pos)
	if err != nil {
		return "", false, fmt.Errorf("read the chain of %s: %w", sessionID, err)
	}
	return h, ok, nil
}

func (p *Postgres) hashAt(ctx context.Context, t chainTable, key string, pos int64) (string, bool, error) {
	var h string
	err := p.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COALESCE(hash, '') FROM %s WHERE %s = $1 AND chain_pos = $2`, t.name, t.key), // #nosec G201 -- names from chainTable
		key, pos).Scan(&h)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return h, err == nil, err
}

// chainHead is a session's last chain position and hash.
func (p *Postgres) chainHead(ctx context.Context, sessionID string) (int64, string, error) {
	return p.head(ctx, eventsChain, sessionID)
}

func (p *Postgres) head(ctx context.Context, t chainTable, key string) (int64, string, error) {
	var pos int64
	var h string
	err := p.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COALESCE(max(chain_pos), 0),
		COALESCE((SELECT hash FROM %[1]s WHERE %[2]s = $1 AND chain_pos IS NOT NULL ORDER BY chain_pos DESC LIMIT 1), '')
		FROM %[1]s WHERE %[2]s = $1`, t.name, t.key), key).Scan(&pos, &h) // #nosec G201 -- names from chainTable
	return pos, h, err
}

// ExportChain writes a session's event chain for checking with nothing but
// the file (chain.VerifyExport): each row as it was hashed, sealed payloads
// still sealed, then the head the database holds. A chain that does not
// verify is refused unless unverified is set; the file then fails its own check.
func (p *Postgres) ExportChain(ctx context.Context, w io.Writer, sessionID string, unverified bool) (ChainReport, error) {
	r, err := p.VerifyChain(ctx, sessionID)
	if err != nil {
		return r, err
	}
	if !r.OK() && !unverified {
		return r, fmt.Errorf("the chain of %s does not verify: %s", sessionID, r.Reason)
	}
	pos, h, err := p.head(ctx, eventsChain, sessionID)
	if err != nil {
		return r, err
	}
	ew := chain.NewWriter(w, chain.Header{Chain: eventsChain.kind, Tenant: p.tenant, Scope: sessionID, From: 1,
		PreChain: r.Unchained, ExportedAt: time.Now().UTC()})
	var werr error
	if err := p.walk(ctx, eventsChain, sessionID, 1, pos, func(l chain.Link) bool {
		werr = ew.Link(l)
		return werr == nil
	}); err != nil {
		return r, err
	}
	if werr != nil {
		return r, werr
	}
	return r, ew.Close(chain.Head{Chain: eventsChain.kind, Tenant: p.tenant, Scope: sessionID, Pos: pos, Hash: h})
}
