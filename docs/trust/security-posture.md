# Security posture

A summary for a security reviewer. Every claim below names the file that
makes it true — read the file if you need more than the summary.

Abhed is pre-release (0.1.x), built by a one-person company. No claim here
should be read as a certification; see "What is not in place" at the end.

## Trust boundaries

**Sandbox tiers.** Abhed executes agent-issued commands inside a sandbox
whose strength is explicit and self-reporting (`internal/sandbox/sandbox.go`):

| Tier | Mechanism | Notes |
|---|---|---|
| `none` | Runs directly on the host | Suitable only for a trusted single-user local run |
| `process` | Process-level confinement (macOS `sandbox-exec`, Linux namespaces/seccomp) | The minimum to set on any shared server: `sandbox.min_tier: "process"` |
| `container` | OCI container: namespace isolation, shared kernel | |
| `vm` | microVM or gVisor userspace kernel | Strongest tier implemented |

`Select` (`internal/sandbox/sandbox.go`) picks the strongest backend
available that meets the configured `MinTier`, and **refuses to start** if
none qualifies, naming what it tried and how to fix it — it never silently
downgrades (see the comment above `Select` and `README.md`'s "The sandbox
never silently downgrades"). Pin `sandbox.min_tier` explicitly in the config
of any shared deployment so that an empty or `none` value cannot ship
unnoticed; `abhed doctor` reports the tier actually in force.

Note: `docs/architecture/03-security.md` describes a more elaborate tier
design (gVisor by default, a microVM per session) as the target architecture.
That document is explicit that this is **engineering judgment, not verified
practice**, and the tiers actually implemented in
`internal/sandbox/sandbox.go` today are `none` / `process` / `container` /
`vm`, verified by the escape tests in that package. Treat the design doc as
direction, and this document and the code as what runs today.

**Policy engine order.** Every tool call is evaluated in a fixed order,
documented at the top of `internal/policy/policy.go`:

```
Hooks → Deny rules → Ask rules → Permission mode → Allow rules → Callback
```

**Deny rules are absolute.** A matching deny rule blocks the call even in
`bypass` mode — the most permissive mode Abhed has (`internal/policy/policy.go`,
`ModeBypass` comment: "dangerous; refusable by org policy"). Rules are
scoped per-command, not per-tool: allowing `bash(npm test)` never allows
`bash(rm -rf /)`. A deployment's deny list should block reads of SSH keys,
cloud credentials and `.env` files, and no allow rule should pre-approve an interpreter or file-reading command that could be
used to exfiltrate one of those files under the cover of an approved rule.

**The agent cannot reach its own configuration.** `.abhed/` in the workspace
and in the home directory holds the policy, the users file and the keys. The
file tools refuse any path with that component (`internal/tools/session.go`,
`isHarnessState`) and the process sandbox denies commands both reading and
writing it (`internal/sandbox/process.go`), with `~/.abhed/skills` the one
readable part. This is enforced before the rules are consulted, because a
rule that protects the file the rules live in can be removed by editing
that file. `internal/tools/state_test.go` and
`internal/sandbox/state_test.go` try to read the configuration, rewrite the
deny list and plant a users file, and fail if any succeeds. Container and VM
tiers keep the workspace mount as configured; mount `.abhed` there read-only
or leave it out of the mount.

**Extensions may only veto, never permit.** `internal/extension/extension.go`
states the rule directly: "An extension may VETO, never PERMIT." Hooks run
first in the policy order specifically so they can veto before anything
else executes (`internal/extension/host.go`), and an extension's refusal is
enforced the same way a deny rule is — it can turn an `allow` into a `deny`,
never the reverse. `internal/extension/extension_test.go` asserts this
directly (an extension can force `Decision: Deny`).

**Tool output is tagged untrusted at ingest.** `store/schema.sql`'s
`events` table carries a `trust` column (`CHECK (trust IN ('trusted',
'untrusted'))`) on every event, so provenance travels with the data rather
than being inferred later. File contents, tool output, and MCP responses are
tagged at the point they enter the system (`README.md`: "Everything untrusted
is tagged at ingest"). Policy decisions are made on the *action* requested,
never on the untrusted text that motivated it (`docs/architecture/03-security.md`
§2, "L2").

## Data flow — what leaves the deployment

**Nothing, by default.** The shell tool gets no network access unless
`sandbox.allow_network` is set to true (verified by
`TestProcessSandboxBlocksNetworkByDefault` on both the macOS and the Linux
backend — the Linux run needs a privileged CI job, since a hosted runner
cannot unshare a network namespace — per `docs/architecture/03-security.md`
§7), and a containerised server with no
bind mount has no route to the host filesystem.

**`web_search`, when enabled**, is the one narrow, structured exception: a Go
tool in the server process makes the request, not the sandboxed shell, so a
compromised session cannot turn it into an arbitrary outbound connection. It
is off by default (`web_search.enabled: false` in
`config/config.go`'s defaults) and is a separate capability from
shell networking — enabling one does not enable the other.

**The model endpoint the operator configured.** Prompts and context go to
whatever model endpoint is set in `model.providers`. Abhed is model-agnostic
by construction (`docs/vision.md`); a model served on the same machine means
prompts never leave it, and an operator who points Abhed at a third-party API
is sending data to that API. Either is a property of that deployment's
configuration, not a promise Abhed makes about every deployment.

## Storage

**Postgres**, with a schema in `store/schema.sql`. Two properties
the schema comment states directly: events are append-only (no `UPDATE`, no
`DELETE`), and tenant isolation is enforced by row-level security, not only
by query construction.

**Append-only by trigger and by privilege.** `abhed_events_immutable()` raises
on any `UPDATE`, `DELETE` or `TRUNCATE` against `events`. On its own that stops
a bug or a stray statement; it does not stop the role that owns the table,
which may disable a trigger or drop the table outright. So the schema is
applied by an owner role through `abhed migrate`, and the server runs as a
separate role holding `INSERT` and `SELECT` on `events` and owning nothing. The
server **refuses to start** as a role that could alter the record, asking the
database what the connection can do rather than trusting configuration.
`TestRuntimeRoleCannotAlterTheRecord` tries thirteen routes as the runtime role
— rewrite, delete, truncate, disabling or dropping the triggers, replacing the
trigger function, `session_replication_role`, taking ownership, dropping the
table, granting itself the privilege — and CI fails if that test skips.

*This paragraph used to claim the database refused "regardless of how the
application is compromised". That was wrong until 0.3: the application role
owned the tables, so it could truncate them or switch the triggers off. With
`storage.single_role` set it is still the case, by the operator's choice.*

**What is not covered.** The owner role and any database superuser can still
alter the record; that is what owning a database means. Keep those credentials
off the server host. Detecting tampering by them needs a hash chain over the
events with the head stored elsewhere, which is not built.

**`FORCE ROW LEVEL SECURITY`**, not just `ENABLE`. The schema comment explains
why this specific word matters: a table's owner bypasses ordinary RLS, and
the application role almost always owns the tables it created, so `ENABLE`
alone leaves the isolation policy silently inert. This was found by
`TestRowLevelSecurityIsolatesTenants`, which could read another tenant's rows
until `FORCE` was added (`README.md`'s evidence-discipline section tells the
same story). `sessions`, `events` and `checkpoints` all carry `ENABLE` and
`FORCE` together, with a `current_setting('app.tenant_id', true)` policy on
each.

**Two database roles, and the app refuses to run as the wrong one.**
Postgres does not apply row-level security to a superuser or a `BYPASSRLS`
role, not even with `FORCE`, so an Abhed connected as one has every
isolation policy in the schema and none of the isolation. Provision two
roles: a superuser used only to create the database and the application
role, and an application role (`NOSUPERUSER NOBYPASSRLS NOCREATEROLE
NOCREATEDB`) that owns the application's tables and is the only one in the
server's DSN. On connect, Abhed checks `rolsuper OR rolbypassrls` on the role
it connected as and **refuses to start** if that role is privileged
(`store/postgres.go`).

## Authentication

Three modes in this edition (`docs/ops/enabling-auth.md`): `none` (local dev
only), `local` (username/password Abhed holds), and `proxy` (identity from a
trusted reverse proxy's headers). OIDC sign-in against an IdP — full token
verification against the JWKS, PKCE, single-use `state`, browser sessions —
is part of the Enterprise Edition and documented with it.

- **Local accounts use bcrypt** at the library default cost
  (`auth/local.go`, `auth/filestore.go`). A wrong password
  and an unknown username return the same error in the same time — a missing
  user is still run through bcrypt against a dummy hash — specifically to
  prevent timing-based username enumeration, with a test asserting it.
- **No self-registration by default.** `allow_signup` defaults off, so
  accounts are created by an administrator with `abhed user add`. A new
  account lands with no groups — it can use the console and read its own
  sessions, and cannot administer the console or change policy unless an
  administrator puts it in the admin group (`-admin`). Invites, access
  requests and grants are part of the Enterprise Edition.
- **Sign-in is rate-limited** for anonymous callers (`throttle` in
  `server/server.go`).

## Container hardening

When the server runs in a container, run it with these flags. Each closes a
specific threat:

| Control | Flag | Why |
|---|---|---|
| No host filesystem access | no bind mount; workspace is a named volume | No host path is mounted, so there is no host path to escape to |
| Unprivileged user | `--user 10001:10001` | |
| No Linux capabilities | `--cap-drop ALL` | An agent's shell has no business with `CAP_NET_ADMIN` or `CAP_SYS_ADMIN` |
| No privilege escalation | `--security-opt no-new-privileges` | Blocks setuid-binary escalation |
| Immutable image | `--read-only` | A compromise cannot install a backdoor into the binary or persist outside the writable volumes |
| Ephemeral scratch space | `--tmpfs /tmp:rw,noexec,nosuid,size=512m` | Gone on restart, `noexec` so a dropped payload cannot run |
| Resource caps | `--memory 2g --memory-swap 2g --pids-limit 512 --cpus 2` | A runaway or hostile agent should exhaust its own limits, not the host's |
| Loopback-only bind | `--publish 127.0.0.1:8080:8080` | The reverse proxy is the sole route in |
| Config mounted read-only at the managed path | `--volume $CONFIG:/etc/abhed/config.json:ro` | Loading config at the managed path sets `Managed`, making `bypass` mode refusable and policy non-escalatable from inside the container |
| Skills mounted read-only | `--volume $SKILLS:/workspace/.abhed/skills:ro` | Skills are instructions; the agent must not be able to rewrite its own operating rules |

The transport and browser layers — TLS and HSTS, a Content-Security-Policy,
`frame-ancestors 'none'` — belong to the reverse proxy in front of the
container, which should be the only route to the published port.

## Telemetry

**Off, and not shipped in this edition.** `config/config.go`'s
`TelemetryConfig.Enabled` is a bare `bool` with no default set, so it is
`false` unless explicitly turned on, and the Community Edition contains
nothing that sends telemetry anywhere. The OpenTelemetry exporter is part of
the Enterprise Edition; when enabled there, it ships events over OTLP/HTTP to
whatever collector the operator runs — there is no built-in Zybuu endpoint.
The event log stays the source of truth regardless; telemetry is a tap on
it, so a collector being down cannot slow or fail a session.

## What is NOT in place

Stated plainly rather than buried:

- **No SOC 2, ISO 27001, HIPAA, or FedRAMP certification.** Any mapping of
  Abhed's controls onto a compliance framework is engineering judgment to
  confirm with your compliance function, not an attestation.
- **No third-party penetration test yet.** `README.md`'s evidence-discipline
  section: "A human red-team engagement remains outstanding and is not
  substitutable" for the internal adversarial suite. The suite that exists
  (`internal/redteam`) proves the implemented controls resist the attacks its
  author thought of; it is not independent verification.
  `docs/ops/red-team-scope.md` says what to commission.
- **Single node.** The server is one process against one database. There is
  no horizontal scaling and no failover in the software.
- **One maintainer.** Zybuu is a one-person company (`docs/vision.md`). There
  is no second reviewer, no on-call rotation, and no bus-factor mitigation
  beyond what is written down in this repository. See `SECURITY.md` for the
  response times that implies.
