package sqlite

import (
	"context"
	"fmt"
	"time"
)

// migrations are applied in order, each once, recorded in schema_version as
// the Postgres schema records its own. Never edit one that has shipped; add
// the next.
var migrations = []string{
	// 1: the control plane. A sandbox is kept whole as JSON beside the
	// columns its queries filter on. resource_version comes from one counter,
	// so it rises across every write. The audit table is append-only.
	`
CREATE TABLE counters (
  name  TEXT    PRIMARY KEY,
  value INTEGER NOT NULL
) STRICT;
INSERT INTO counters (name, value) VALUES ('resource_version', 0);

CREATE TABLE sandboxes (
  id               TEXT    PRIMARY KEY,
  owner            TEXT    NOT NULL,
  state            TEXT    NOT NULL,
  resource_version INTEGER NOT NULL,
  object           TEXT    NOT NULL
) STRICT;
CREATE INDEX sandboxes_owner_idx ON sandboxes (owner, id);

CREATE TABLE sandbox_idempotency (
  scope        TEXT    NOT NULL,
  key          TEXT    NOT NULL,
  request_hash TEXT    NOT NULL,
  state        TEXT    NOT NULL,
  status       INTEGER NOT NULL DEFAULT 0,
  body         BLOB,
  expires_at   INTEGER NOT NULL,
  PRIMARY KEY (scope, key)
) STRICT;
CREATE INDEX sandbox_idempotency_expiry_idx ON sandbox_idempotency (expires_at);

CREATE TABLE sandbox_audit (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  at          TEXT    NOT NULL,
  principal   TEXT    NOT NULL,
  action      TEXT    NOT NULL,
  sandbox_id  TEXT    NOT NULL,
  generation  INTEGER NOT NULL,
  outcome     TEXT    NOT NULL,
  code        TEXT    NOT NULL DEFAULT '',
  detail      TEXT    NOT NULL DEFAULT '{}',
  inserted_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;
CREATE INDEX sandbox_audit_sandbox_idx ON sandbox_audit (sandbox_id, seq);

CREATE TRIGGER sandbox_audit_no_update BEFORE UPDATE ON sandbox_audit
BEGIN SELECT RAISE(ABORT, 'sandbox_audit is append-only'); END;
CREATE TRIGGER sandbox_audit_no_delete BEFORE DELETE ON sandbox_audit
BEGIN SELECT RAISE(ABORT, 'sandbox_audit is append-only'); END;
-- INSERT OR REPLACE deletes the old row without firing a delete trigger.
CREATE TRIGGER sandbox_audit_no_replace BEFORE INSERT ON sandbox_audit
WHEN EXISTS (SELECT 1 FROM sandbox_audit WHERE seq = NEW.seq)
BEGIN SELECT RAISE(ABORT, 'sandbox_audit is append-only'); END;
`,
}

// SchemaVersion is the newest version this build knows.
var SchemaVersion = len(migrations)

// migrate brings the file to SchemaVersion in one transaction, and refuses a
// file a newer build has changed.
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate the control plane's database: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_version (
		  version    INTEGER PRIMARY KEY,
		  applied_at TEXT    NOT NULL
		) STRICT`); err != nil {
		return fmt.Errorf("migrate the control plane's database: %w", err)
	}
	var have int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_version`).Scan(&have); err != nil {
		return fmt.Errorf("read the control plane's schema version: %w", err)
	}
	if have > SchemaVersion {
		return fmt.Errorf("the control plane's database %s is at schema version %d, newer than this build's %d; "+
			"run the Abhed that wrote it", s.path, have, SchemaVersion)
	}
	for v := have + 1; v <= SchemaVersion; v++ {
		if _, err := tx.ExecContext(ctx, migrations[v-1]); err != nil {
			return fmt.Errorf("apply the control plane's schema version %d: %w", v, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`,
			v, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("record the control plane's schema version %d: %w", v, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate the control plane's database: %w", err)
	}
	return nil
}
