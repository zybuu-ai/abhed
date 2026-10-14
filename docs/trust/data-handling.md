# Data handling

What Abhed stores, where it lives, how long it lives, and what an operator can
do about it.

## What Abhed stores

From `store/schema.sql`, on the Postgres storage driver:

| Table | Contents |
|---|---|
| `sessions` | One row per agent session: tenant, user, workspace, model, the opening prompt, turn/token counts, cost, terminal reason |
| `events` | Every event in a session, append-only: tool calls, **tool output**, model responses, approvals and refusals — tagged `trusted` or `untrusted` per event |
| `checkpoints` | The content of a file immediately before the agent changed it, keyed to the event that changed it — this is what `/undo` reads |
| `models` | The registry of configured model endpoints and their capability profile |
| `approvals` | Questions asked of a person and their answers, with the call's arguments (the arguments are cleared when the session is deleted) |
| `sandboxes`, `sandbox_idempotency`, `sandbox_audit`, `sandbox_templates` | Control-plane sandboxes, replayable requests, and the sandboxes' hash-chained audit record |
| `owner_changes`, `account_changes` | Trigger-written history of every session owner change and every change to an account's groups or tenant |
| `session_keys` | Each session's sealing key; deleting the session deletes it |

A model's reasoning, where the endpoint reports it, is in `events` twice: in
parts as it streamed (`agent.reasoning.delta`) and whole (`agent.reasoning`),
the way a reply is held as `agent.delta` and `agent.message`.

Accounts (username, email, tenant, groups, bcrypt password hash) are stored
either in this same Postgres database or, without `storage.driver: postgres`
configured, in `<workspace>/.abhed/users.json` mode `0600` — or wherever `auth.users_file` (`ABHED_USERS_FILE`) points, which a deployment sets to its state directory so accounts never sit in a workspace
(`docs/ops/enabling-auth.md`, "Where accounts live").

**Secrets** are kept in the operator's store, `~/.abhed/secrets.json`. On
`abhed serve` with sign-in each account has a store of its own under
`~/.abhed/secrets.d` (or `ABHED_ACCOUNT_SECRETS_DIR`), and the operator's
serves scheduled runs only. No value is written to the record; a value that
reaches an event is redacted first.

**Uploads** are files the agent reads or writes inside the session workspace,
each upload in a folder of its own with a random name. On a server with more
than one account each session works in a sandbox folder of its own, unless
`server.shared_sessions` is set. They are not a separate store; they are ordinary files in that workspace, and
their content that passes through the agent loop is captured in `events` as
tool output like anything else the agent reads. When the server runs in a
container, put the workspace on a named volume rather than a host path, so
there is no host path for an agent to escape to.

## Where

Wherever the operator points `storage.dsn`. Give the database no published
port: reachable only from the network the server is on, never the LAN or the
host at large.

Without the Postgres driver configured, `abhed serve` keeps sessions and
events in memory, and they do not survive a restart. Accounts do survive, in
`users.json` (`docs/ops/enabling-auth.md`, "Where accounts live";
`docs/guide/02-configuration.md`, "Storage").

## The local record (command line and SDK)

Without Postgres, the command line keeps every session in the **local record**
under `~/.abhed/records/<tenant>/`, or a managed `record.dir`; an embedder can
hand one to an SDK agent (`docs/guide/12-records.md`).

- **What:** each session's events, redacted before the first write, one
  chained file per session, plus an index, head files and lock files.
- **Blobs:** `blobs/sha256/` holds the content of each file just before the
  agent changed it, **unredacted**, since a redacted copy could not restore
  it. A file a read deny rule covers, or whose name says it holds keys
  (`.env`, `*.env`, `.envrc`, `*.pem`, `*.key`, `id_rsa`, `.git-credentials`,
  `.pgpass`, `credentials.json`, `*.tfvars`, `.ssh/`, `.aws/`, `.azure/`,
  `.kube/` and the like), is not copied; its checkpoint records why. The
  file's content can still reach the record another way, in the edit tool's
  own events.
- **Where and who:** directories `0700`, files `0600`, refused if another user
  owns them (not checked on Windows). The agent's file tools and the sandbox
  tiers refuse the directory. It belongs on a local disk.
- **How long:** until `abhed record prune` removes a session, leaving a
  tombstone in the index, or, when the managed configuration sets
  `record.retention_days`, until a session is older than that when the record
  is opened. Pruning takes the session's blobs that no other session names.

## Retention

**Indefinite by default.** Nothing in `store/schema.sql` expires a
row. The schema comment says retention is "handled by dropping partitions or
by a privileged archival role, never by mutating rows in place" — that
mechanism is not implemented in this repository today; there is no scheduled
job that drops old partitions. Rows persist until an operator does something
about it at the database level.

**Deleting a session erases what it said; the rows stay.** Schema version
3 in `store/schema.sql` adds `sessions.deleted_at` and `deleted_by`, and
version 10 adds `session_keys`. Since 1.2.8 each session's event payloads
and file pre-images are sealed (AES-256-GCM) with a key of the session's own,
and a delete removes that key, the session's label and title, and its
approvals' arguments. The event rows stay, append-only by trigger, but hold
ciphertext no one can open; what remains is that each step happened, its
type, actor, time and hash. Not erased: sessions recorded before 1.2.8, whose
payloads were never sealed; database backups and WAL archives taken before
the delete, which hold the key until they expire; streaming replicas, until
they replay the delete; and the deleted key's bytes until `VACUUM` reclaims
them and the replicas replay it.

## What the operator can export or delete

- **Export**: there is a session-export path in the console for a user's own
  sessions while their account is active. An operator with database access
  can export anything directly via `pg_dump`.
- **Delete**: A user can delete their own session from the console UI, which
  marks it and erases its content by key, as above — it stops being reachable
  through the product, and the rows that remain in Postgres cannot be read. An operator with direct database
  access is the only path to actually removing rows, and doing so against an
  append-only `events` table means operating below the API (the immutability
  triggers block `UPDATE`/`DELETE` from any client, including an
  administrator's own SQL session, unless they drop or bypass the trigger
  first — which is a deliberate, high-friction operation, not a supported
  workflow).
- **Accounts**: `abhed user remove <username>` deletes the account
  immediately and stops what a running server runs for it. It does not
  delete that user's session history: removing the account ends their
  access; the audit log of what they did stays, which is the point of
  keeping it. On Postgres their sessions move to an unclaimed owner, so a
  new account of the same name does not inherit them.

## Self-hosted deployments

**No subprocessors.** A customer running Abhed on their own infrastructure —
laptop, private datacenter, or air-gapped rack — sends data nowhere but the
model endpoint they themselves configure (`docs/vision.md`: "It runs where
the data is... It runs any model... Changing vendors is a line of config").
Zybuu has no access to a self-hosted deployment's data, database, or logs
unless the operator explicitly shares them (for example, to report a bug).

The hosted console that Zybuu runs is a separate deployment with its own
subprocessor list, documented with the Enterprise Edition.
