-- Abhed event store schema.
--
-- Implements docs/architecture/10-data-model.md. Two properties matter above
-- all: events are append-only (no UPDATE, no DELETE), and tenant isolation is
-- enforced by row-level security rather than only by query construction. An
-- audit log you can edit is not an audit log, and a boundary that exists in one
-- place is not a boundary.

CREATE TABLE IF NOT EXISTS sessions (
  id              TEXT PRIMARY KEY,
  tenant_id       TEXT        NOT NULL,
  user_id         TEXT        NOT NULL,
  workspace       TEXT        NOT NULL,
  model           TEXT        NOT NULL,
  prompt_hash     TEXT        NOT NULL DEFAULT '',
  harness_version TEXT        NOT NULL DEFAULT '',
  mode            TEXT        NOT NULL DEFAULT 'default',
  -- The opening request, kept so a session list is readable at a glance.
  prompt          TEXT        NOT NULL DEFAULT '',
  parent_id       TEXT        REFERENCES sessions(id),
  started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  ended_at        TIMESTAMPTZ,
  terminal_reason TEXT,
  turns           INT         NOT NULL DEFAULT 0,
  tokens_in       BIGINT      NOT NULL DEFAULT 0,
  tokens_out      BIGINT      NOT NULL DEFAULT 0,
  tokens_cached   BIGINT      NOT NULL DEFAULT 0,
  compactions     INT         NOT NULL DEFAULT 0,
  gpu_seconds     NUMERIC     NOT NULL DEFAULT 0,
  cost_usd        NUMERIC     NOT NULL DEFAULT 0,
  -- How full the window was when the session ended, as distinct from what it
  -- cost. tokens_in above is a running sum across turns and only ever grows;
  -- these two say whether there was room left. Nullable rather than defaulted
  -- to zero: a session recorded before this column existed, or one whose
  -- adapter reports no window, has no measurement — and zero would read as an
  -- empty context rather than an absent reading.
  context_tokens  BIGINT,
  context_window  BIGINT,
  -- Which node holds this session's turn in flight. A turn lives in one
  -- process's memory, so a request about it has to reach that process;
  -- NULL means no node holds it and any node may claim it.
  node_id         TEXT,
  node_seen_at    TIMESTAMPTZ
);

-- Columns added after the first release. CREATE TABLE IF NOT EXISTS leaves an
-- existing table untouched, so a new column in the definition above never
-- reaches a database that already has the table. These statements are what
-- actually migrate one, and they are idempotent for the same reason the rest
-- of this file is: it runs on every start.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS context_tokens BIGINT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS context_window BIGINT;
-- A pending approval, durable so the answer can arrive at any node.
--
-- The request lives in the memory of the node running the turn, and the
-- reviewer's answer may land anywhere behind a load balancer. Keeping the
-- exchange here means a misrouted answer is still delivered, and a node that
-- dies leaves a visible record rather than a turn waiting on a channel
-- nobody will ever write to.
CREATE TABLE IF NOT EXISTS approvals (
  id          TEXT        PRIMARY KEY,
  session_id  TEXT        NOT NULL,
  tenant_id   TEXT        NOT NULL,
  tool        TEXT        NOT NULL,
  args        JSONB       NOT NULL DEFAULT '{}'::jsonb,
  reason      TEXT        NOT NULL DEFAULT '',
  scope       TEXT        NOT NULL DEFAULT '',
  asked_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- NULL until a reviewer answers. Answered rows are kept: who allowed what,
  -- and when, is part of the record the deployment promised.
  answered_at TIMESTAMPTZ,
  approved    BOOLEAN,
  answered_by TEXT
);

-- The "always allow" scope an answer carried, empty for this call only: the
-- waiting node widens the session's allow list only from this.
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS answer_scope TEXT NOT NULL DEFAULT '';
-- Set when the request stopped waiting, answered or not. An ended row takes
-- no answer, so nothing is approved after the turn it was asked in. When the
-- column is first added, the rows already there are from turns that are no
-- longer waiting, so they are closed once; later starts leave rows alone.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                 WHERE table_schema = current_schema() AND table_name = 'approvals' AND column_name = 'ended_at') THEN
    ALTER TABLE approvals ADD COLUMN ended_at TIMESTAMPTZ;
    -- Every tenant's rows: row security would otherwise limit the owner too.
    ALTER TABLE approvals NO FORCE ROW LEVEL SECURITY;
    UPDATE approvals SET ended_at = asked_at WHERE ended_at IS NULL;
    ALTER TABLE approvals FORCE ROW LEVEL SECURITY;
  END IF;
END $$;

CREATE INDEX IF NOT EXISTS approvals_session ON approvals (tenant_id, session_id, asked_at DESC);
CREATE INDEX IF NOT EXISTS approvals_open    ON approvals (tenant_id, session_id) WHERE answered_at IS NULL;

ALTER TABLE approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE approvals FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS approvals_tenant_isolation ON approvals;
CREATE POLICY approvals_tenant_isolation ON approvals
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS node_id TEXT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS node_seen_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS sessions_tenant_started_idx
  ON sessions (tenant_id, started_at DESC);
CREATE INDEX IF NOT EXISTS sessions_user_idx ON sessions (user_id);
CREATE INDEX IF NOT EXISTS sessions_parent_idx ON sessions (parent_id)
  WHERE parent_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS events (
  id         TEXT        PRIMARY KEY,
  session_id TEXT        NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
  tenant_id  TEXT        NOT NULL,
  parent_id  TEXT,
  seq        BIGINT      NOT NULL,
  type       TEXT        NOT NULL,
  payload    JSONB       NOT NULL,
  actor      TEXT        NOT NULL,
  -- Provenance travels with the event: content read from files, tool output,
  -- MCP responses and search results are data, never instructions.
  trust      TEXT        NOT NULL DEFAULT 'trusted',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT events_seq_unique UNIQUE (session_id, seq),
  CONSTRAINT events_trust_valid CHECK (trust IN ('trusted', 'untrusted'))
);

CREATE INDEX IF NOT EXISTS events_session_seq_idx ON events (session_id, seq);
CREATE INDEX IF NOT EXISTS events_type_time_idx   ON events (type, created_at DESC);
CREATE INDEX IF NOT EXISTS events_tenant_time_idx ON events (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS events_payload_idx     ON events USING GIN (payload);

-- Append-only enforcement. Retention is handled by dropping partitions or by a
-- privileged archival role, never by mutating rows in place.
CREATE OR REPLACE FUNCTION abhed_events_immutable() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'events are append-only: % on events is not permitted', TG_OP
    USING HINT = 'Audit integrity depends on immutability. Use retention policy to expire old partitions.';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS events_no_update ON events;
CREATE TRIGGER events_no_update BEFORE UPDATE ON events
  FOR EACH ROW EXECUTE FUNCTION abhed_events_immutable();

DROP TRIGGER IF EXISTS events_no_delete ON events;
CREATE TRIGGER events_no_delete BEFORE DELETE ON events
  FOR EACH ROW EXECUTE FUNCTION abhed_events_immutable();

-- TRUNCATE fires no row trigger, so without this the two above were bypassed
-- by one statement that removes every row.
DROP TRIGGER IF EXISTS events_no_truncate ON events;
CREATE TRIGGER events_no_truncate BEFORE TRUNCATE ON events
  FOR EACH STATEMENT EXECUTE FUNCTION abhed_events_immutable();

-- These triggers stop an UPDATE, DELETE or TRUNCATE. They do not stop the
-- role that OWNS this table: an owner may disable a trigger or drop the table,
-- and no trigger can prevent that. The record is protected against the running
-- application only when the application connects as a role that does not own
-- it and holds INSERT and SELECT alone. `abhed migrate` sets that up, and the
-- store refuses to start otherwise unless storage.single_role says the
-- operator has chosen to go without.

-- Checkpoints back /undo: the content of a file immediately before the agent
-- changed it. NULL `before` means the file did not previously exist.
CREATE TABLE IF NOT EXISTS checkpoints (
  id         TEXT        PRIMARY KEY,
  session_id TEXT        NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
  tenant_id  TEXT        NOT NULL,
  event_seq  BIGINT      NOT NULL,
  path       TEXT        NOT NULL,
  before     BYTEA,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS checkpoints_session_idx
  ON checkpoints (session_id, event_seq DESC);

-- Model registry with the capability profile from the conformance suite
-- (docs/architecture/08-eval.md L2). A model that has not passed conformance
-- should not be enabled.
CREATE TABLE IF NOT EXISTS models (
  name            TEXT PRIMARY KEY,
  provider        TEXT        NOT NULL,
  endpoint        TEXT        NOT NULL,
  context_window  INT         NOT NULL DEFAULT 0,
  profile         JSONB       NOT NULL DEFAULT '{}'::jsonb,
  enabled         BOOLEAN     NOT NULL DEFAULT false,
  registered_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Row-level security. The application connects as a non-superuser role and sets
-- app.tenant_id per connection; even a query that forgets its WHERE clause
-- cannot cross a tenant boundary.
--
-- FORCE is not optional here. A table's OWNER bypasses ordinary RLS, and the
-- application role almost always owns the tables it created — so ENABLE alone
-- leaves the policy silently inert. This was caught by
-- TestRowLevelSecurityIsolatesTenants, which read another tenant's rows until
-- FORCE was added. Superusers still bypass RLS, which is why the application
-- must never connect as one.
ALTER TABLE sessions    ENABLE ROW LEVEL SECURITY;
ALTER TABLE events      ENABLE ROW LEVEL SECURITY;
ALTER TABLE checkpoints ENABLE ROW LEVEL SECURITY;

ALTER TABLE sessions    FORCE ROW LEVEL SECURITY;
ALTER TABLE events      FORCE ROW LEVEL SECURITY;
ALTER TABLE checkpoints FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sessions_tenant_isolation ON sessions;
CREATE POLICY sessions_tenant_isolation ON sessions
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS events_tenant_isolation ON events;
CREATE POLICY events_tenant_isolation ON events
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS checkpoints_tenant_isolation ON checkpoints;
CREATE POLICY checkpoints_tenant_isolation ON checkpoints
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Added after v1: existing deployments get the column without a migration step.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS prompt TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS schema_version (
  version    INT         PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO schema_version (version) VALUES (1) ON CONFLICT DO NOTHING;

-- Schema version 2 was the access tables. They belong to the edition that
-- issues invites to strangers and live in its own migration, with a version
-- table of its own, so two editions never race to write this one. A database
-- that already carries the row keeps it; nothing here depends on it.

-- Schema version 3: a session a user has deleted from their console.
--
-- Events are append-only by trigger, so a delete cannot remove the transcript
-- rows and does not try. It marks the session; every read path treats a marked
-- session as absent. The rows remain for the audit the deployment promised,
-- reachable only by someone with the database, never through the API again.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS deleted_by TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS sessions_live_idx ON sessions (tenant_id, started_at DESC)
  WHERE deleted_at IS NULL;

INSERT INTO schema_version (version) VALUES (3) ON CONFLICT DO NOTHING;
