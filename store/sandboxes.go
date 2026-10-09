package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zybuu-ai/abhed/controlplane"
)

// Sandboxes is the control plane's store and audit record in Postgres, for
// the tenant its Postgres was opened for. It passes controlplane/storetest.
type Sandboxes struct {
	pool   *pgxpool.Pool
	tenant string
	fence  Fence

	mu    sync.Mutex
	swept time.Time
}

var (
	_ controlplane.Store       = (*Sandboxes)(nil)
	_ controlplane.AuditLog    = (*Sandboxes)(nil)
	_ controlplane.AuditReader = (*Sandboxes)(nil)
)

// FenceOp is the write a Fence is asked about.
type FenceOp string

const (
	FenceCreate FenceOp = "create"
	FenceUpdate FenceOp = "update"
	FenceDelete FenceOp = "delete"
)

// Fence runs inside the transaction of every sandbox write, before it
// commits, and refuses it by returning an error; the write then changes
// nothing. Enterprise HA uses it to hold a write to the replica whose lease
// on the sandbox is current, so a replica that lost it writes nothing.
type Fence func(ctx context.Context, tx pgx.Tx, op FenceOp, id string) error

// WithFence returns the store with every Create, Update and Delete checked
// by f in its own transaction.
func (q *Sandboxes) WithFence(f Fence) *Sandboxes {
	return &Sandboxes{pool: q.pool, tenant: q.tenant, fence: f}
}

// Pool is the connection pool, for an edition's tables beside these.
func (q *Sandboxes) Pool() *pgxpool.Pool { return q.pool }

// Tenant is the tenant the store was opened for.
func (q *Sandboxes) Tenant() string { return q.tenant }

func (q *Sandboxes) fenced(ctx context.Context, tx pgx.Tx, op FenceOp, id string) error {
	if q.fence == nil {
		return nil
	}
	return q.fence(ctx, tx, op, id)
}

// sandboxColumns are what this build reads and writes; the runtime role
// cannot add them, so their absence is found before the first request.
var sandboxColumns = [][2]string{
	{"sandboxes", "object"}, {"sandboxes", "resource_version"},
	{"sandbox_idempotency", "request_hash"}, {"sandbox_audit", "principal"},
}

// Sandboxes returns the control plane's store, refusing a database whose
// schema predates it.
func (p *Postgres) Sandboxes(ctx context.Context) (*Sandboxes, error) {
	var missing []string
	for _, c := range sandboxColumns {
		var n int
		if err := p.pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`, c[0], c[1]).Scan(&n); err != nil {
			return nil, fmt.Errorf("check the control plane's schema: %w", err)
		}
		if n == 0 {
			missing = append(missing, c[0]+"."+c[1])
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the database has no control-plane tables yet (%s missing). "+
			"Run `abhed migrate` as the owner (see docs/guide/02-configuration.md)", strings.Join(missing, ", "))
	}
	return &Sandboxes{pool: p.pool, tenant: p.tenant}, nil
}

func scanSandbox(row pgx.Row) (controlplane.Sandbox, error) {
	var raw []byte
	var rv int64
	if err := row.Scan(&raw, &rv); err != nil {
		return controlplane.Sandbox{}, err
	}
	var s controlplane.Sandbox
	if err := json.Unmarshal(raw, &s); err != nil {
		return controlplane.Sandbox{}, fmt.Errorf("decode sandbox: %w", err)
	}
	s.ResourceVersion = rv
	return s, nil
}

func encodeSandbox(s controlplane.Sandbox) ([]byte, error) {
	s.ResourceVersion = 0
	return json.Marshal(s)
}

func (q *Sandboxes) Create(ctx context.Context, s controlplane.Sandbox) (controlplane.Sandbox, error) {
	if !controlplane.ValidID(s.ID) {
		return controlplane.Sandbox{}, fmt.Errorf("%w: sandbox id %q", controlplane.ErrInvalid, s.ID)
	}
	if !s.State.Valid() {
		return controlplane.Sandbox{}, fmt.Errorf("%w: state %q", controlplane.ErrInvalid, s.State)
	}
	raw, err := encodeSandbox(s)
	if err != nil {
		return controlplane.Sandbox{}, err
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return controlplane.Sandbox{}, fmt.Errorf("create sandbox: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var rv int64
	err = tx.QueryRow(ctx, `
		INSERT INTO sandboxes (tenant_id, id, owner, state, resource_version, object)
		VALUES ($1, $2, $3, $4, nextval('sandbox_resource_version'), $5)
		RETURNING resource_version`, q.tenant, s.ID, s.Owner, string(s.State), raw).Scan(&rv)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return controlplane.Sandbox{}, fmt.Errorf("%w: sandbox %s", controlplane.ErrExists, s.ID)
		}
		return controlplane.Sandbox{}, fmt.Errorf("create sandbox: %w", err)
	}
	if err := q.fenced(ctx, tx, FenceCreate, s.ID); err != nil {
		return controlplane.Sandbox{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return controlplane.Sandbox{}, fmt.Errorf("create sandbox: %w", err)
	}
	s = s.Clone()
	s.ResourceVersion = rv
	return s, nil
}

func (q *Sandboxes) Get(ctx context.Context, id string) (controlplane.Sandbox, error) {
	s, err := scanSandbox(q.pool.QueryRow(ctx,
		`SELECT object, resource_version FROM sandboxes WHERE tenant_id = $1 AND id = $2`, q.tenant, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return controlplane.Sandbox{}, fmt.Errorf("%w: sandbox %s", controlplane.ErrNotFound, id)
	}
	return s, err
}

func (q *Sandboxes) List(ctx context.Context, o controlplane.ListOptions) ([]controlplane.Sandbox, string, error) {
	size := o.PageSize()
	// Selectors are applied here rather than in SQL, so the store and the
	// memory store cannot disagree on what one matches.
	batch := max(size+1, 200)
	var out []controlplane.Sandbox
	after := o.After
	for {
		rows, err := q.pool.Query(ctx, `
			SELECT object, resource_version FROM sandboxes
			WHERE tenant_id = $1 AND id > $2 AND ($3 = '' OR owner = $3) AND ($4 = '' OR state = $4)
			ORDER BY id LIMIT $5`, q.tenant, after, o.Owner, string(o.State), batch)
		if err != nil {
			return nil, "", fmt.Errorf("list sandboxes: %w", err)
		}
		n := 0
		var page []controlplane.Sandbox
		for rows.Next() {
			s, err := scanSandbox(rows)
			if err != nil {
				rows.Close()
				return nil, "", err
			}
			n++
			after = s.ID
			page = append(page, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, "", fmt.Errorf("list sandboxes: %w", err)
		}
		for _, s := range page {
			if !o.Matches(s) {
				continue
			}
			if len(out) == size {
				return out, out[len(out)-1].ID, nil
			}
			out = append(out, s)
		}
		if n < batch {
			return out, "", nil
		}
	}
}

// locked reads id for update inside tx, so a guarded write sees the row as
// no other writer can change it until tx ends.
func (q *Sandboxes) locked(ctx context.Context, tx pgx.Tx, id string) (controlplane.Sandbox, error) {
	s, err := scanSandbox(tx.QueryRow(ctx,
		`SELECT object, resource_version FROM sandboxes WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, q.tenant, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return controlplane.Sandbox{}, fmt.Errorf("%w: sandbox %s", controlplane.ErrNotFound, id)
	}
	return s, err
}

func (q *Sandboxes) Update(ctx context.Context, s controlplane.Sandbox, expect int64) (controlplane.Sandbox, error) {
	if !s.State.Valid() {
		return controlplane.Sandbox{}, fmt.Errorf("%w: state %q", controlplane.ErrInvalid, s.State)
	}
	raw, err := encodeSandbox(s)
	if err != nil {
		return controlplane.Sandbox{}, err
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return controlplane.Sandbox{}, fmt.Errorf("update sandbox: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	cur, err := q.locked(ctx, tx, s.ID)
	if err != nil {
		return controlplane.Sandbox{}, err
	}
	if cur.ResourceVersion != expect {
		return controlplane.Sandbox{}, fmt.Errorf("%w: sandbox %s is at %d, not %d", controlplane.ErrVersionMismatch, s.ID, cur.ResourceVersion, expect)
	}
	if err := controlplane.CheckImmutable(cur, s); err != nil {
		return controlplane.Sandbox{}, err
	}
	if err := q.fenced(ctx, tx, FenceUpdate, s.ID); err != nil {
		return controlplane.Sandbox{}, err
	}
	var rv int64
	if err := tx.QueryRow(ctx, `
		UPDATE sandboxes SET state = $3, object = $4, resource_version = nextval('sandbox_resource_version')
		WHERE tenant_id = $1 AND id = $2 RETURNING resource_version`, q.tenant, s.ID, string(s.State), raw).Scan(&rv); err != nil {
		return controlplane.Sandbox{}, fmt.Errorf("update sandbox: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return controlplane.Sandbox{}, fmt.Errorf("update sandbox: %w", err)
	}
	s = s.Clone()
	s.ResourceVersion = rv
	return s, nil
}

func (q *Sandboxes) Delete(ctx context.Context, id string, expect int64) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete sandbox: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	tag, err := tx.Exec(ctx, `DELETE FROM sandboxes WHERE tenant_id = $1 AND id = $2 AND resource_version = $3`,
		q.tenant, id, expect)
	if err != nil {
		return fmt.Errorf("delete sandbox: %w", err)
	}
	if tag.RowsAffected() == 1 {
		if err := q.fenced(ctx, tx, FenceDelete, id); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("delete sandbox: %w", err)
		}
		return nil
	}
	_ = tx.Rollback(ctx)
	cur, err := q.Get(ctx, id)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: sandbox %s is at %d, not %d", controlplane.ErrVersionMismatch, id, cur.ResourceVersion, expect)
}

// sweepEvery is how often a claim also removes the tenant's expired records.
const sweepEvery = time.Minute

func (q *Sandboxes) Claim(ctx context.Context, rec controlplane.IdemRecord, now time.Time) (controlplane.IdemRecord, bool, error) {
	q.mu.Lock()
	sweep := now.Sub(q.swept) > sweepEvery
	if sweep {
		q.swept = now
	}
	q.mu.Unlock()
	if sweep {
		if _, err := q.pool.Exec(ctx, `DELETE FROM sandbox_idempotency WHERE tenant_id = $1 AND expires_at <= $2`, q.tenant, now); err != nil {
			return controlplane.IdemRecord{}, false, fmt.Errorf("sweep idempotency records: %w", err)
		}
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return controlplane.IdemRecord{}, false, fmt.Errorf("claim idempotency key: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `DELETE FROM sandbox_idempotency WHERE tenant_id = $1 AND scope = $2 AND key = $3 AND expires_at <= $4`,
		q.tenant, rec.Scope, rec.Key, now); err != nil {
		return controlplane.IdemRecord{}, false, fmt.Errorf("claim idempotency key: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO sandbox_idempotency (tenant_id, scope, key, request_hash, state, status, body, expires_at)
		VALUES ($1, $2, $3, $4, $5, 0, NULL, $6) ON CONFLICT DO NOTHING`,
		q.tenant, rec.Scope, rec.Key, rec.RequestHash, string(controlplane.IdemPending), rec.ExpiresAt)
	if err != nil {
		return controlplane.IdemRecord{}, false, fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		if err := tx.Commit(ctx); err != nil {
			return controlplane.IdemRecord{}, false, fmt.Errorf("claim idempotency key: %w", err)
		}
		rec.State, rec.Status, rec.Body = controlplane.IdemPending, 0, nil
		return rec, true, nil
	}
	var cur controlplane.IdemRecord
	var state string
	err = tx.QueryRow(ctx, `
		SELECT scope, key, request_hash, state, status, body, expires_at FROM sandbox_idempotency
		WHERE tenant_id = $1 AND scope = $2 AND key = $3`, q.tenant, rec.Scope, rec.Key).
		Scan(&cur.Scope, &cur.Key, &cur.RequestHash, &state, &cur.Status, &cur.Body, &cur.ExpiresAt)
	if err != nil {
		return controlplane.IdemRecord{}, false, fmt.Errorf("read idempotency key: %w", err)
	}
	cur.State = controlplane.IdemState(state)
	return cur, false, tx.Commit(ctx)
}

func (q *Sandboxes) Complete(ctx context.Context, scope, key string, status int, body []byte, expires time.Time) error {
	tag, err := q.pool.Exec(ctx, `
		UPDATE sandbox_idempotency SET state = $4, status = $5, body = $6, expires_at = $7
		WHERE tenant_id = $1 AND scope = $2 AND key = $3 AND state = $8`,
		q.tenant, scope, key, string(controlplane.IdemDone), status, body, expires, string(controlplane.IdemPending))
	if err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no pending request for idempotency key %q", controlplane.ErrNotFound, key)
	}
	return nil
}

func (q *Sandboxes) Release(ctx context.Context, scope, key string) error {
	if _, err := q.pool.Exec(ctx, `DELETE FROM sandbox_idempotency WHERE tenant_id = $1 AND scope = $2 AND key = $3 AND state = $4`,
		q.tenant, scope, key, string(controlplane.IdemPending)); err != nil {
		return fmt.Errorf("release idempotency key: %w", err)
	}
	return nil
}

// Append adds one event to the control plane's record.
func (q *Sandboxes) Append(ctx context.Context, ev controlplane.AuditEvent) error {
	detail, err := json.Marshal(ev.Detail)
	if err != nil {
		return err
	}
	if ev.Detail == nil {
		detail = []byte("{}")
	}
	if _, err := q.pool.Exec(ctx, `
		INSERT INTO sandbox_audit (tenant_id, at, principal, action, sandbox_id, generation, outcome, code, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		q.tenant, ev.At, ev.Principal, ev.Action, ev.SandboxID, ev.Generation, ev.Outcome, ev.Code, detail); err != nil {
		return fmt.Errorf("record sandbox event: %w", err)
	}
	return nil
}

// AuditFor lists the record of one sandbox, oldest first.
func (q *Sandboxes) AuditFor(ctx context.Context, id string) ([]controlplane.AuditEvent, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT seq, at, principal, action, sandbox_id, generation, outcome, code, detail
		FROM sandbox_audit WHERE tenant_id = $1 AND sandbox_id = $2 ORDER BY seq`, q.tenant, id)
	if err != nil {
		return nil, fmt.Errorf("read sandbox record: %w", err)
	}
	defer rows.Close()
	var out []controlplane.AuditEvent
	for rows.Next() {
		var ev controlplane.AuditEvent
		var detail []byte
		if err := rows.Scan(&ev.Seq, &ev.At, &ev.Principal, &ev.Action, &ev.SandboxID, &ev.Generation,
			&ev.Outcome, &ev.Code, &detail); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(detail, &ev.Detail); err != nil {
			return nil, err
		}
		if len(ev.Detail) == 0 {
			ev.Detail = nil
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
