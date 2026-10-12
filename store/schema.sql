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
  -- Its parent in the same tenant only: see sessions_parent_same_tenant.
  parent_id       TEXT,
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
-- The action.requested event the row asks about, so another node can check an
-- answer bound to it. Empty on rows from before the column.
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS request_id TEXT NOT NULL DEFAULT '';
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

-- A parent is found in the session's own tenant. A key on parent_id alone was
-- checked outside row security, so a write naming another tenant's session as
-- parent succeeded and confirmed that the id existed there. Rows already
-- stored are not re-checked (NOT VALID); every write from now on is.
CREATE UNIQUE INDEX IF NOT EXISTS sessions_tenant_id_key ON sessions (tenant_id, id);
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint
                 WHERE conname = 'sessions_parent_same_tenant'
                   AND conrelid = to_regclass(current_schema() || '.sessions')) THEN
    ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_parent_id_fkey;
    ALTER TABLE sessions ADD CONSTRAINT sessions_parent_same_tenant
      FOREIGN KEY (tenant_id, parent_id) REFERENCES sessions (tenant_id, id) NOT VALID;
  END IF;
END $$;

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

-- A title a person gave the session, copied from its latest session.renamed
-- event so the list can show it; the event is the record of the rename.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS title TEXT NOT NULL DEFAULT '';

-- Schema version 6: when the database stored each event, on its own clock.
-- created_at is the writer's; a session with no holder is an orphan only
-- once its last event is old on the database's clock, which a writer whose
-- clock runs fast cannot make look stale. Rows already there read the time
-- the column was added, which makes them younger, never older.
ALTER TABLE events ADD COLUMN IF NOT EXISTS inserted_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- The writer may insert every column, so the database sets this one itself.
CREATE OR REPLACE FUNCTION abhed_events_inserted_at() RETURNS trigger AS $$
BEGIN
  NEW.inserted_at := now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS events_inserted_at ON events;
CREATE TRIGGER events_inserted_at BEFORE INSERT ON events
  FOR EACH ROW EXECUTE FUNCTION abhed_events_inserted_at();

INSERT INTO schema_version (version) VALUES (6) ON CONFLICT DO NOTHING;

-- Schema version 7: the sandbox control plane (docs/architecture/control-plane-contract.md).
--
-- A sandbox is kept whole as JSON beside the columns its queries filter on.
-- resource_version comes from one sequence, so it rises across every write
-- and a list can be resumed. The audit table is append-only like events:
-- who asked for what is part of the record.
CREATE SEQUENCE IF NOT EXISTS sandbox_resource_version;

CREATE TABLE IF NOT EXISTS sandboxes (
  tenant_id        TEXT   NOT NULL,
  id               TEXT   NOT NULL,
  owner            TEXT   NOT NULL,
  state            TEXT   NOT NULL,
  resource_version BIGINT NOT NULL,
  object           JSONB  NOT NULL,
  PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS sandboxes_owner_idx ON sandboxes (tenant_id, owner, id);

CREATE TABLE IF NOT EXISTS sandbox_idempotency (
  tenant_id    TEXT        NOT NULL,
  scope        TEXT        NOT NULL,
  key          TEXT        NOT NULL,
  request_hash TEXT        NOT NULL,
  state        TEXT        NOT NULL,
  status       INT         NOT NULL DEFAULT 0,
  body         BYTEA,
  expires_at   TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (tenant_id, scope, key)
);
ALTER TABLE sandbox_idempotency ADD COLUMN IF NOT EXISTS claim_token TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS sandbox_audit (
  seq         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id   TEXT        NOT NULL,
  at          TIMESTAMPTZ NOT NULL,
  principal   TEXT        NOT NULL,
  action      TEXT        NOT NULL,
  sandbox_id  TEXT        NOT NULL,
  generation  BIGINT      NOT NULL,
  outcome     TEXT        NOT NULL,
  code        TEXT        NOT NULL DEFAULT '',
  detail      JSONB       NOT NULL DEFAULT '{}'::jsonb,
  inserted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sandbox_audit_sandbox_idx ON sandbox_audit (tenant_id, sandbox_id, seq);

DROP TRIGGER IF EXISTS sandbox_audit_no_update ON sandbox_audit;
CREATE TRIGGER sandbox_audit_no_update BEFORE UPDATE ON sandbox_audit
  FOR EACH ROW EXECUTE FUNCTION abhed_events_immutable();
DROP TRIGGER IF EXISTS sandbox_audit_no_delete ON sandbox_audit;
CREATE TRIGGER sandbox_audit_no_delete BEFORE DELETE ON sandbox_audit
  FOR EACH ROW EXECUTE FUNCTION abhed_events_immutable();
DROP TRIGGER IF EXISTS sandbox_audit_no_truncate ON sandbox_audit;
CREATE TRIGGER sandbox_audit_no_truncate BEFORE TRUNCATE ON sandbox_audit
  FOR EACH STATEMENT EXECUTE FUNCTION abhed_events_immutable();

ALTER TABLE sandboxes           ENABLE ROW LEVEL SECURITY;
ALTER TABLE sandbox_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE sandbox_audit       ENABLE ROW LEVEL SECURITY;
ALTER TABLE sandboxes           FORCE ROW LEVEL SECURITY;
ALTER TABLE sandbox_idempotency FORCE ROW LEVEL SECURITY;
ALTER TABLE sandbox_audit       FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sandboxes_tenant_isolation ON sandboxes;
CREATE POLICY sandboxes_tenant_isolation ON sandboxes
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
DROP POLICY IF EXISTS sandbox_idempotency_tenant_isolation ON sandbox_idempotency;
CREATE POLICY sandbox_idempotency_tenant_isolation ON sandbox_idempotency
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
DROP POLICY IF EXISTS sandbox_audit_tenant_isolation ON sandbox_audit;
CREATE POLICY sandbox_audit_tenant_isolation ON sandbox_audit
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Sandbox templates: named defaults a sandbox is created from, versioned
-- from the same sequence as sandboxes.
CREATE TABLE IF NOT EXISTS sandbox_templates (
  tenant_id        TEXT   NOT NULL,
  name             TEXT   NOT NULL,
  resource_version BIGINT NOT NULL,
  object           JSONB  NOT NULL,
  PRIMARY KEY (tenant_id, name)
);
ALTER TABLE sandbox_templates ENABLE ROW LEVEL SECURITY;
ALTER TABLE sandbox_templates FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS sandbox_templates_tenant_isolation ON sandbox_templates;
CREATE POLICY sandbox_templates_tenant_isolation ON sandbox_templates
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

INSERT INTO schema_version (version) VALUES (7) ON CONFLICT DO NOTHING;

-- Schema version 8: a hash chain over each append-only record.
--
-- Every row is linked to the one written before it under the same key (a
-- session's events, a tenant's sandbox audit): chain_pos counts them, prev_hash
-- is the previous row's hash, and hash covers prev_hash and the row itself.
-- The trigger sets all three, whatever the writer sent, and takes a lock on the
-- key so two writers cannot both link to the same row. A row removed or changed
-- by someone who owns the table breaks the chain at that point; one who
-- recomputes the whole chain is caught only by a head kept outside the
-- database (record.witness). Rows written before this version have none and
-- are reported as unchained, never as broken.
ALTER TABLE events ADD COLUMN IF NOT EXISTS chain_pos BIGINT;
ALTER TABLE events ADD COLUMN IF NOT EXISTS prev_hash TEXT;
ALTER TABLE events ADD COLUMN IF NOT EXISTS hash TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS events_chain_idx ON events (session_id, chain_pos) WHERE chain_pos IS NOT NULL;
ALTER TABLE sandbox_audit ADD COLUMN IF NOT EXISTS chain_pos BIGINT;
ALTER TABLE sandbox_audit ADD COLUMN IF NOT EXISTS prev_hash TEXT;
ALTER TABLE sandbox_audit ADD COLUMN IF NOT EXISTS hash TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS sandbox_audit_chain_idx ON sandbox_audit (tenant_id, chain_pos) WHERE chain_pos IS NOT NULL;

-- abhed_chain_hash is a row's hash: the named columns of the row as JSON,
-- after the previous hash. Only named columns, so a column added later does
-- not change the hash of a row written before it; the time zone is fixed so
-- every session renders a timestamp the same way.
CREATE OR REPLACE FUNCTION abhed_chain_hash(prev text, rec jsonb, cols text[]) RETURNS text AS $$
  SELECT encode(sha256(convert_to(prev || E'\n' ||
    COALESCE((SELECT jsonb_object_agg(e.key, e.value) FROM jsonb_each(rec) e WHERE e.key = ANY(cols)), '{}'::jsonb)::text,
    'UTF8')), 'hex')
$$ LANGUAGE sql IMMUTABLE SET timezone TO 'UTC';

-- abhed_chain_insert links a new row. Its first argument names the key
-- column; the rest name the columns the hash covers.
CREATE OR REPLACE FUNCTION abhed_chain_insert() RETURNS trigger AS $$
DECLARE
  k    text := to_jsonb(NEW) ->> TG_ARGV[0];
  last record;
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended(TG_TABLE_NAME || '/' || k, 0));
  EXECUTE format('SELECT chain_pos, hash FROM %I WHERE %I = $1 AND chain_pos IS NOT NULL
                  ORDER BY chain_pos DESC LIMIT 1', TG_TABLE_NAME, TG_ARGV[0])
    INTO last USING k;
  NEW.chain_pos := COALESCE(last.chain_pos, 0) + 1;
  NEW.prev_hash := COALESCE(last.hash, repeat('0', 64));
  NEW.hash := abhed_chain_hash(NEW.prev_hash, to_jsonb(NEW), TG_ARGV[1:]);
  RETURN NEW;
END;
$$ LANGUAGE plpgsql SET timezone TO 'UTC';

-- Named to sort last: BEFORE triggers fire in name order, and the hash must
-- cover what the others set.
DROP TRIGGER IF EXISTS zz_events_chain ON events;
CREATE TRIGGER zz_events_chain BEFORE INSERT ON events
  FOR EACH ROW EXECUTE FUNCTION abhed_chain_insert('session_id',
    'id', 'session_id', 'tenant_id', 'parent_id', 'seq', 'type', 'payload', 'actor', 'trust', 'created_at');
DROP TRIGGER IF EXISTS zz_sandbox_audit_chain ON sandbox_audit;
CREATE TRIGGER zz_sandbox_audit_chain BEFORE INSERT ON sandbox_audit
  FOR EACH ROW EXECUTE FUNCTION abhed_chain_insert('tenant_id',
    'seq', 'tenant_id', 'at', 'principal', 'action', 'sandbox_id', 'generation', 'outcome', 'code', 'detail');

-- abhed_chain_verify walks one key's chain and returns the first position
-- whose link or hash does not hold (0 when all do), with the chain's length,
-- its head hash and how many rows carry no chain. cols are the trigger's.
CREATE OR REPLACE FUNCTION abhed_chain_verify(tbl regclass, keycol text, k text, cols text[],
  OUT first_bad bigint, OUT rows_chained bigint, OUT head text, OUT unchained bigint) AS $$
DECLARE
  r    record;
  prev text := repeat('0', 64);
  want bigint := 1;
BEGIN
  first_bad := 0; rows_chained := 0; head := '';
  EXECUTE format('SELECT count(*) FROM %s WHERE %I = $1 AND chain_pos IS NULL', tbl, keycol)
    INTO unchained USING k;
  FOR r IN EXECUTE format('SELECT chain_pos, prev_hash, hash, to_jsonb(t) AS rec FROM %s t
                           WHERE %I = $1 AND chain_pos IS NOT NULL ORDER BY chain_pos', tbl, keycol) USING k LOOP
    IF first_bad = 0 AND (r.chain_pos <> want OR r.prev_hash IS DISTINCT FROM prev
                          OR r.hash IS DISTINCT FROM abhed_chain_hash(prev, r.rec, cols)) THEN
      first_bad := want;
    END IF;
    prev := r.hash; want := r.chain_pos + 1;
    rows_chained := rows_chained + 1; head := r.hash;
  END LOOP;
END;
$$ LANGUAGE plpgsql STABLE SET timezone TO 'UTC';

INSERT INTO schema_version (version) VALUES (8) ON CONFLICT DO NOTHING;

-- Schema version 9: who owns a session changes only by the paths that may
-- change it, and every change is recorded.
--
-- The running server may move a removed account's sessions to its unclaimed
-- form, and nothing else; the owner role (a migration) may do more. A
-- session's id, tenant and parent never change. Each owner change is written
-- to owner_changes by a trigger that runs as the table's owner, so the server
-- can neither skip the row nor write one of its own.
CREATE TABLE IF NOT EXISTS owner_changes (
  seq        BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id  TEXT        NOT NULL,
  session_id TEXT        NOT NULL,
  from_user  TEXT        NOT NULL,
  to_user    TEXT        NOT NULL,
  by_role    TEXT        NOT NULL,
  at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS owner_changes_no_update ON owner_changes;
CREATE TRIGGER owner_changes_no_update BEFORE UPDATE ON owner_changes
  FOR EACH ROW EXECUTE FUNCTION abhed_events_immutable();
DROP TRIGGER IF EXISTS owner_changes_no_delete ON owner_changes;
CREATE TRIGGER owner_changes_no_delete BEFORE DELETE ON owner_changes
  FOR EACH ROW EXECUTE FUNCTION abhed_events_immutable();
DROP TRIGGER IF EXISTS owner_changes_no_truncate ON owner_changes;
CREATE TRIGGER owner_changes_no_truncate BEFORE TRUNCATE ON owner_changes
  FOR EACH STATEMENT EXECUTE FUNCTION abhed_events_immutable();
ALTER TABLE owner_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE owner_changes FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_changes_tenant_isolation ON owner_changes;
CREATE POLICY owner_changes_tenant_isolation ON owner_changes
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Runs as the writer, so it can tell the table's owner from the server.
CREATE OR REPLACE FUNCTION abhed_sessions_guard() RETURNS trigger AS $$
BEGIN
  IF NEW.id IS DISTINCT FROM OLD.id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
     OR NEW.parent_id IS DISTINCT FROM OLD.parent_id THEN
    RAISE EXCEPTION 'a session''s id, tenant and parent do not change';
  END IF;
  IF NEW.user_id IS DISTINCT FROM OLD.user_id
     AND NEW.user_id IS DISTINCT FROM 'unclaimed:' || OLD.user_id
     AND NOT pg_has_role(current_user, (SELECT relowner FROM pg_class WHERE oid = TG_RELID), 'MEMBER') THEN
    RAISE EXCEPTION 'a session''s owner changes only when its account is removed (to unclaimed:%), or by a migration', OLD.user_id;
  END IF;
  -- A deletion names who made it, and is final: its key is gone, and the
  -- mark says who and when.
  IF OLD.deleted_at IS NULL
     AND (NEW.deleted_at IS NULL AND NEW.deleted_by IS DISTINCT FROM OLD.deleted_by
          OR NEW.deleted_at IS NOT NULL AND COALESCE(NEW.deleted_by, '') = '')
     AND NOT pg_has_role(current_user, (SELECT relowner FROM pg_class WHERE oid = TG_RELID), 'MEMBER') THEN
    RAISE EXCEPTION 'a session''s deletion says who made it, and is set only with the deletion';
  END IF;
  IF OLD.deleted_at IS NOT NULL
     AND (NEW.deleted_at IS DISTINCT FROM OLD.deleted_at OR NEW.deleted_by IS DISTINCT FROM OLD.deleted_by)
     AND NOT pg_has_role(current_user, (SELECT relowner FROM pg_class WHERE oid = TG_RELID), 'MEMBER') THEN
    RAISE EXCEPTION 'a session''s deletion is not undone or rewritten';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS sessions_guard ON sessions;
CREATE TRIGGER sessions_guard BEFORE UPDATE ON sessions
  FOR EACH ROW EXECUTE FUNCTION abhed_sessions_guard();

-- Runs as the table's owner: the server cannot write owner_changes itself.
-- The tenant is set for the insert, as row security requires.
CREATE OR REPLACE FUNCTION abhed_sessions_owner_noted() RETURNS trigger AS $$
DECLARE
  was text := current_setting('app.tenant_id', true);
BEGIN
  PERFORM set_config('app.tenant_id', NEW.tenant_id, true);
  INSERT INTO owner_changes (tenant_id, session_id, from_user, to_user, by_role)
  VALUES (NEW.tenant_id, NEW.id, OLD.user_id, NEW.user_id, session_user);
  PERFORM set_config('app.tenant_id', COALESCE(was, ''), true);
  RETURN NULL;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp;

DROP TRIGGER IF EXISTS sessions_owner_noted ON sessions;
CREATE TRIGGER sessions_owner_noted AFTER UPDATE OF user_id ON sessions
  FOR EACH ROW WHEN (NEW.user_id IS DISTINCT FROM OLD.user_id)
  EXECUTE FUNCTION abhed_sessions_owner_noted();

INSERT INTO schema_version (version) VALUES (9) ON CONFLICT DO NOTHING;

-- Schema version 10: erasure by key.
--
-- Events cannot be deleted, so a session's payloads and checkpoint pre-images
-- are sealed with a key of the session's own, kept here. Deleting a session
-- deletes its key; what stays in events can no longer be read by anyone. The
-- chain still covers the sealed rows, so the record of what happened, and
-- when, survives the erasure of what was said.
CREATE TABLE IF NOT EXISTS session_keys (
  session_id TEXT        PRIMARY KEY,
  tenant_id  TEXT        NOT NULL,
  key        BYTEA       NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- A key belongs to a session; none is kept for one that does not exist.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'session_keys_session_fk') THEN
    ALTER TABLE session_keys ADD CONSTRAINT session_keys_session_fk
      FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE RESTRICT;
  END IF;
END $$;
ALTER TABLE session_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE session_keys FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS session_keys_tenant_isolation ON session_keys;
CREATE POLICY session_keys_tenant_isolation ON session_keys
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- A key goes only with its session's deletion, marked first in the same
-- transaction, so the server's role cannot make a live session unreadable.
CREATE OR REPLACE FUNCTION abhed_session_keys_guard() RETURNS trigger AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM sessions WHERE id = OLD.session_id AND deleted_at IS NOT NULL)
     AND NOT pg_has_role(current_user, (SELECT relowner FROM pg_class WHERE oid = TG_RELID), 'MEMBER') THEN
    RAISE EXCEPTION 'a session''s key is deleted only with the session';
  END IF;
  RETURN OLD;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS session_keys_guard ON session_keys;
CREATE TRIGGER session_keys_guard BEFORE DELETE ON session_keys
  FOR EACH ROW EXECUTE FUNCTION abhed_session_keys_guard();
DROP TRIGGER IF EXISTS session_keys_no_truncate ON session_keys;
CREATE TRIGGER session_keys_no_truncate BEFORE TRUNCATE ON session_keys
  FOR EACH STATEMENT EXECUTE FUNCTION abhed_events_immutable();

INSERT INTO schema_version (version) VALUES (10) ON CONFLICT DO NOTHING;
