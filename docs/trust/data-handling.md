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

A model's reasoning, where the endpoint reports it, is in `events` twice: in
parts as it streamed (`agent.reasoning.delta`) and whole (`agent.reasoning`),
the way a reply is held as `agent.delta` and `agent.message`.

Accounts (username, email, tenant, groups, bcrypt password hash) are stored
either in this same Postgres database or, without `storage.driver: postgres`
configured, in `<workspace>/.abhed/users.json` mode `0600` — or wherever `auth.users_file` (`ABHED_USERS_FILE`) points, which a deployment sets to its state directory so accounts never sit in a workspace
(`docs/ops/enabling-auth.md`, "Where accounts live").

**Uploads** are files the agent reads or writes inside the session workspace.
They are not a separate store; they are ordinary files in that workspace, and
their content that passes through the agent loop is captured in `events` as
tool output like anything else the agent reads. When the server runs in a
container, put the workspace on a named volume rather than a host path, so
there is no host path for an agent to escape to.

## Where

Wherever the operator points `storage.dsn`. Give the database no published
port: reachable only from the network the server is on, never the LAN or the
host at large.

Without the Postgres driver configured, sessions and events live in memory
and do not survive a restart at all. Accounts do survive, in `users.json`;
transcripts do not (`docs/ops/enabling-auth.md`, "Where accounts live";
`docs/guide/02-configuration.md`, "Storage").

## Retention

**Indefinite by default.** Nothing in `store/schema.sql` expires a
row. The schema comment says retention is "handled by dropping partitions or
by a privileged archival role, never by mutating rows in place" — that
mechanism is not implemented in this repository today; there is no scheduled
job that drops old partitions. Rows persist until an operator does something
about it at the database level.

**Deleting a session marks it; the rows stay.** Schema version 3 in
`store/schema.sql` adds `sessions.deleted_at` and `deleted_by`.
The comment is explicit about what this does and does not do: "Events are
append-only by trigger, so a delete cannot remove the transcript rows and
does not try. It marks the session; every read path treats a marked session
as absent. The rows remain for the audit the deployment promised, reachable
only by someone with the database, never through the API again." A deleted
session is gone from the console and the API; it is not gone from the
database.

## What the operator can export or delete

- **Export**: there is a session-export path in the console for a user's own
  sessions while their account is active. An operator with database access
  can export anything directly via `pg_dump`.
- **Delete**: A user can delete their own session from the console UI, which
  marks it per schema version 3 above — it stops being reachable through the
  product but the rows remain in Postgres. An operator with direct database
  access is the only path to actually removing rows, and doing so against an
  append-only `events` table means operating below the API (the immutability
  triggers block `UPDATE`/`DELETE` from any client, including an
  administrator's own SQL session, unless they drop or bypass the trigger
  first — which is a deliberate, high-friction operation, not a supported
  workflow).
- **Accounts**: `abhed user remove <username>` deletes the account
  immediately. It does not delete that user's session history: removing the
  account ends their access; the audit log of what they did stays, which is
  the point of keeping it.

## Self-hosted deployments

**No subprocessors.** A customer running Abhed on their own infrastructure —
laptop, private datacenter, or air-gapped rack — sends data nowhere but the
model endpoint they themselves configure (`docs/vision.md`: "It runs where
the data is... It runs any model... Changing vendors is a line of config").
Zybuu has no access to a self-hosted deployment's data, database, or logs
unless the operator explicitly shares them (for example, to report a bug).

The hosted console that Zybuu runs is a separate deployment with its own
subprocessor list, documented with the Enterprise Edition.
