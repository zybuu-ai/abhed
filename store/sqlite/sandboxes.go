package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
)

var (
	_ controlplane.Store       = (*Store)(nil)
	_ controlplane.AuditLog    = (*Store)(nil)
	_ controlplane.AuditReader = (*Store)(nil)
)

type scanner interface{ Scan(dest ...any) error }

func scanSandbox(row scanner) (controlplane.Sandbox, error) {
	var raw string
	var rv int64
	if err := row.Scan(&raw, &rv); err != nil {
		return controlplane.Sandbox{}, err
	}
	var s controlplane.Sandbox
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return controlplane.Sandbox{}, fmt.Errorf("decode sandbox: %w", err)
	}
	s.ResourceVersion = rv
	return s, nil
}

func encodeSandbox(s controlplane.Sandbox) (string, error) {
	s.ResourceVersion = 0
	b, err := json.Marshal(s)
	return string(b), err
}

// write runs fn in one BEGIN IMMEDIATE transaction on the writer, so no
// other writer, in this process or another, comes between its read and its
// write.
func (s *Store) write(ctx context.Context, what string, fn func(tx *sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

func nextVersion(ctx context.Context, tx *sql.Tx) (int64, error) {
	var rv int64
	err := tx.QueryRowContext(ctx,
		`UPDATE counters SET value = value + 1 WHERE name = 'resource_version' RETURNING value`).Scan(&rv)
	return rv, err
}

func readSandbox(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (controlplane.Sandbox, error) {
	s, err := scanSandbox(q.QueryRowContext(ctx, `SELECT object, resource_version FROM sandboxes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return controlplane.Sandbox{}, fmt.Errorf("%w: sandbox %s", controlplane.ErrNotFound, id)
	}
	return s, err
}

func (s *Store) Create(ctx context.Context, sb controlplane.Sandbox) (controlplane.Sandbox, error) {
	if !controlplane.ValidID(sb.ID) {
		return controlplane.Sandbox{}, fmt.Errorf("%w: sandbox id %q", controlplane.ErrInvalid, sb.ID)
	}
	if !sb.State.Valid() {
		return controlplane.Sandbox{}, fmt.Errorf("%w: state %q", controlplane.ErrInvalid, sb.State)
	}
	raw, err := encodeSandbox(sb)
	if err != nil {
		return controlplane.Sandbox{}, err
	}
	var rv int64
	err = s.write(ctx, "create sandbox", func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sandboxes WHERE id = ?`, sb.ID).Scan(&n); err != nil {
			return fmt.Errorf("create sandbox: %w", err)
		}
		if n > 0 {
			return fmt.Errorf("%w: sandbox %s", controlplane.ErrExists, sb.ID)
		}
		if rv, err = nextVersion(ctx, tx); err != nil {
			return fmt.Errorf("create sandbox: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sandboxes (id, owner, state, resource_version, object) VALUES (?, ?, ?, ?, ?)`,
			sb.ID, sb.Owner, string(sb.State), rv, raw); err != nil {
			return fmt.Errorf("create sandbox: %w", err)
		}
		return nil
	})
	if err != nil {
		return controlplane.Sandbox{}, err
	}
	sb = sb.Clone()
	sb.ResourceVersion = rv
	return sb, nil
}

func (s *Store) Get(ctx context.Context, id string) (controlplane.Sandbox, error) {
	return readSandbox(ctx, s.r, id)
}

func (s *Store) List(ctx context.Context, o controlplane.ListOptions) ([]controlplane.Sandbox, string, error) {
	size := o.PageSize()
	// Selectors are applied here rather than in SQL, so the store and the
	// memory store cannot disagree on what one matches.
	batch := max(size+1, 200)
	var out []controlplane.Sandbox
	after := o.After
	for {
		page, err := s.listBatch(ctx, o, after, batch)
		if err != nil {
			return nil, "", err
		}
		for _, sb := range page {
			after = sb.ID
			if !o.Matches(sb) {
				continue
			}
			if len(out) == size {
				return out, out[len(out)-1].ID, nil
			}
			out = append(out, sb)
		}
		if len(page) < batch {
			return out, "", nil
		}
	}
}

func (s *Store) listBatch(ctx context.Context, o controlplane.ListOptions, after string, batch int) ([]controlplane.Sandbox, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT object, resource_version FROM sandboxes
		WHERE id > ? AND (? = '' OR owner = ?) AND (? = '' OR state = ?)
		ORDER BY id LIMIT ?`, after, o.Owner, o.Owner, string(o.State), string(o.State), batch)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var page []controlplane.Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		page = append(page, sb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	return page, nil
}

func (s *Store) Update(ctx context.Context, sb controlplane.Sandbox, expect int64) (controlplane.Sandbox, error) {
	if !sb.State.Valid() {
		return controlplane.Sandbox{}, fmt.Errorf("%w: state %q", controlplane.ErrInvalid, sb.State)
	}
	raw, err := encodeSandbox(sb)
	if err != nil {
		return controlplane.Sandbox{}, err
	}
	var rv int64
	err = s.write(ctx, "update sandbox", func(tx *sql.Tx) error {
		cur, err := readSandbox(ctx, tx, sb.ID)
		if err != nil {
			return err
		}
		if cur.ResourceVersion != expect {
			return fmt.Errorf("%w: sandbox %s is at %d, not %d", controlplane.ErrVersionMismatch, sb.ID, cur.ResourceVersion, expect)
		}
		if err := controlplane.CheckImmutable(cur, sb); err != nil {
			return err
		}
		if rv, err = nextVersion(ctx, tx); err != nil {
			return fmt.Errorf("update sandbox: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sandboxes SET state = ?, object = ?, resource_version = ? WHERE id = ?`,
			string(sb.State), raw, rv, sb.ID); err != nil {
			return fmt.Errorf("update sandbox: %w", err)
		}
		return nil
	})
	if err != nil {
		return controlplane.Sandbox{}, err
	}
	sb = sb.Clone()
	sb.ResourceVersion = rv
	return sb, nil
}

func (s *Store) Delete(ctx context.Context, id string, expect int64) error {
	return s.write(ctx, "delete sandbox", func(tx *sql.Tx) error {
		cur, err := readSandbox(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.ResourceVersion != expect {
			return fmt.Errorf("%w: sandbox %s is at %d, not %d", controlplane.ErrVersionMismatch, id, cur.ResourceVersion, expect)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sandboxes WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete sandbox: %w", err)
		}
		return nil
	})
}

// sweepEvery is how often a claim also removes expired records.
const sweepEvery = time.Minute

func (s *Store) Claim(ctx context.Context, rec controlplane.IdemRecord, now time.Time) (controlplane.IdemRecord, bool, error) {
	s.mu.Lock()
	sweep := now.Sub(s.swept) > sweepEvery
	if sweep {
		s.swept = now
	}
	s.mu.Unlock()
	var cur controlplane.IdemRecord
	claimed := false
	err := s.write(ctx, "claim idempotency key", func(tx *sql.Tx) error {
		if sweep {
			if _, err := tx.ExecContext(ctx, `DELETE FROM sandbox_idempotency WHERE expires_at <= ?`, now.UnixNano()); err != nil {
				return fmt.Errorf("sweep idempotency records: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sandbox_idempotency WHERE scope = ? AND key = ? AND expires_at <= ?`,
			rec.Scope, rec.Key, now.UnixNano()); err != nil {
			return fmt.Errorf("claim idempotency key: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO sandbox_idempotency (scope, key, request_hash, state, status, body, expires_at, claim_token)
			VALUES (?, ?, ?, ?, 0, NULL, ?, ?) ON CONFLICT DO NOTHING`,
			rec.Scope, rec.Key, rec.RequestHash, string(controlplane.IdemPending), rec.ExpiresAt.UnixNano(), rec.Token)
		if err != nil {
			return fmt.Errorf("claim idempotency key: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("claim idempotency key: %w", err)
		} else if n == 1 {
			claimed = true
			return nil
		}
		var state string
		var expires int64
		if err := tx.QueryRowContext(ctx, `
			SELECT scope, key, request_hash, state, status, body, expires_at FROM sandbox_idempotency
			WHERE scope = ? AND key = ?`, rec.Scope, rec.Key).
			Scan(&cur.Scope, &cur.Key, &cur.RequestHash, &state, &cur.Status, &cur.Body, &expires); err != nil {
			return fmt.Errorf("read idempotency key: %w", err)
		}
		cur.State, cur.ExpiresAt = controlplane.IdemState(state), time.Unix(0, expires).UTC()
		return nil
	})
	if err != nil {
		return controlplane.IdemRecord{}, false, err
	}
	if claimed {
		rec.State, rec.Status, rec.Body = controlplane.IdemPending, 0, nil
		return rec, true, nil
	}
	return cur, false, nil
}

func (s *Store) Renew(ctx context.Context, scope, key, token string, expires time.Time) error {
	return s.write(ctx, "renew idempotency key", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE sandbox_idempotency SET expires_at = ?
			WHERE scope = ? AND key = ? AND state = ? AND claim_token = ? AND claim_token <> ''`,
			expires.UnixNano(), scope, key, string(controlplane.IdemPending), token)
		return heldOne(res, err, "renew", key)
	})
}

func (s *Store) Complete(ctx context.Context, scope, key, token string, status int, body []byte, expires time.Time) error {
	if body == nil {
		body = []byte{}
	}
	return s.write(ctx, "complete idempotency key", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE sandbox_idempotency SET state = ?, status = ?, body = ?, expires_at = ?
			WHERE scope = ? AND key = ? AND state = ? AND claim_token = ? AND claim_token <> ''`,
			string(controlplane.IdemDone), status, body, expires.UnixNano(), scope, key, string(controlplane.IdemPending), token)
		return heldOne(res, err, "complete", key)
	})
}

// heldOne is ErrNotFound when an update guarded by a claim token changed
// nothing: the claim expired or another request holds it.
func heldOne(res sql.Result, err error, verb, key string) error {
	if err != nil {
		return fmt.Errorf("%s idempotency key: %w", verb, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("%s idempotency key: %w", verb, err)
	} else if n == 0 {
		return fmt.Errorf("%w: no pending request for idempotency key %q", controlplane.ErrNotFound, key)
	}
	return nil
}

func (s *Store) Release(ctx context.Context, scope, key, token string) error {
	return s.write(ctx, "release idempotency key", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM sandbox_idempotency WHERE scope = ? AND key = ? AND state = ? AND claim_token = ? AND claim_token <> ''`,
			scope, key, string(controlplane.IdemPending), token); err != nil {
			return fmt.Errorf("release idempotency key: %w", err)
		}
		return nil
	})
}

// Append adds one event to the control plane's record.
func (s *Store) Append(ctx context.Context, ev controlplane.AuditEvent) error {
	detail := []byte("{}")
	if ev.Detail != nil {
		var err error
		if detail, err = json.Marshal(ev.Detail); err != nil {
			return err
		}
	}
	return s.write(ctx, "record sandbox event", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sandbox_audit (at, principal, action, sandbox_id, generation, outcome, code, detail)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.At.UTC().Format(time.RFC3339Nano), ev.Principal, ev.Action, ev.SandboxID, ev.Generation,
			ev.Outcome, ev.Code, string(detail)); err != nil {
			return fmt.Errorf("record sandbox event: %w", err)
		}
		return nil
	})
}

// AuditFor lists the record of one sandbox, oldest first.
func (s *Store) AuditFor(ctx context.Context, id string) ([]controlplane.AuditEvent, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT seq, at, principal, action, sandbox_id, generation, outcome, code, detail
		FROM sandbox_audit WHERE sandbox_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, fmt.Errorf("read sandbox record: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []controlplane.AuditEvent
	for rows.Next() {
		var ev controlplane.AuditEvent
		var at, detail string
		if err := rows.Scan(&ev.Seq, &at, &ev.Principal, &ev.Action, &ev.SandboxID, &ev.Generation,
			&ev.Outcome, &ev.Code, &detail); err != nil {
			return nil, err
		}
		if ev.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("read sandbox record: %w", err)
		}
		if err := json.Unmarshal([]byte(detail), &ev.Detail); err != nil {
			return nil, err
		}
		if len(ev.Detail) == 0 {
			ev.Detail = nil
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
