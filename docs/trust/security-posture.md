# Security posture

A summary for a security reviewer. Every claim below names the file that
makes it true — read the file if you need more than the summary.

Abhed is at 1.x, built by a small company. No claim here should be read as a
certification; see "What is NOT in place" at the end.

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

**Deny rules are absolute for tool calls.** A matching deny rule blocks the
call even in `bypass` mode — the most permissive mode Abhed has
(`internal/policy/policy.go`, `ModeBypass` comment: "dangerous; refusable by
org policy") — whether the agent or a person makes it. One scope limit: what a
person runs inside the workbench's interactive shell is bounded by the sandbox,
and there deny rules are a best-effort screen on each line as typed (see "A
person's terminal is sandboxed" below). Rules are
scoped per-command, not per-tool: allowing `bash(npm test)` never allows
`bash(rm -rf /)`. A deployment's deny list should block reads of SSH keys,
cloud credentials and `.env` files, and no allow rule should pre-approve an interpreter or file-reading command that could be
used to exfiltrate one of those files under the cover of an approved rule.

**The managed configuration binds every entry point.** `/etc/abhed/config.json`
is read last, and `config.Load` records which keys it set
(`Config.ManagedKeys`). The CLI's flags and the SDK's `Options` pass through
one function, `Config.Apply` (`config/managed.go`), which lets them tighten a
managed setting and refuses, with an error, anything that would loosen one:
`bypass` mode, a mode other than `plan` when the file pins one, a weaker
`tools.syntax_check`, a higher `limits.max_turns`, an added allow rule or
directory when the file sets those lists. Deny rules from a caller are only
ever added. The SDK reads the managed file even without a `ConfigDir`, marks
its engine managed as the CLI and server do, applies the configured ask rules,
and wraps bash in the configured sandbox when the file sets a `sandbox` key.
The console lets a client narrow its session to `plan` and nothing else. Not
bound: the `ABHED_*` environment variables, which override the endpoint,
credentials and database over the managed file; the choice of model; and, in
the SDK, the organisation's `/etc/abhed/ABHED.md` (the SDK loads no memory
files, and `Options.SystemPrompt` replaces the prompt), `limits.max_budget_tokens`
and `limits.max_tokens`, which the SDK does not apply. A managed file that
exists but cannot be read, or a link at the managed path to nothing, stops
Abhed rather than being taken as absent. `abhed eval`, which approves every
prompt with nobody to ask, refuses to run under a managed file, and the
interactive `/mode` command is bound as `-mode` is. The binding covers the
shipped entry points and programs that load configuration with `config.Load`;
a `config.Config` built by hand carries no managed keys.

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

On macOS a command can stat the workspace `.abhed` directory and what is in
it, so `ls -R`, pytest's collection and `git add -A` (with a warning that it
cannot open the directory) pass it by; it cannot list it or read, write, link
or clone what it holds (`TestProcessSandboxWalksPastHarnessState`). A walk
that descends into every folder, such as `find .` or `du`, still reports
`.abhed` and exits 1. Nothing is written into the workspace to achieve this.
On Linux bubblewrap mounts an empty directory over it instead.

**A person's terminal is sandboxed; its line checks are best effort.** Each
tab of the `/ide` terminal is, by default, one interactive `bash` started
through the same sandbox backend as the agent's commands (`Shell` in
`internal/sandbox/process.go` and `container.go`), in the workspace root, under
the tier's filesystem, network and harness-state limits
(`TestProcessSandboxShellIsInteractiveAndConfined`). Opening it is the
person's `bash` call: judged by the policy, so plan mode refuses it, and
recorded with `actor: user`, as is the shell's exit and the last 64 KB of its
output. An interactive shell cannot be judged command by command, because the
shell decides what a line means after it is sent: history recall, tab
completion, aliases, functions, scripts and full-screen programs all happen
inside it. So, for the terminal:

- the sandbox is the enforcement boundary;
- `bash` deny rules are a screen: the server rebuilds each line from the keys
  it forwards and refuses a matching line before its Enter reaches the shell
  (`server/terminal.go`, `shellInput` in `server/pty.go`), recording the
  refusal. It does not see what the shell makes of the line: history recall
  (arrow keys, `!!`, Ctrl-R, Ctrl-O), completion, variables and other
  expansions, a line continued with `\` (`rm -rf \` then `/` passes both
  lines), aliases, functions, scripts, or input to another program, including
  a nested shell;
- each line entered is recorded as `terminal.input`, as typed, marked `edited`
  when keys the server cannot follow were used. When the server cannot confirm
  the terminal showed the line as typed, the line is recorded without its text:
  at a password prompt (on the process and none tiers, read from the
  terminal's own mode), when its echo was not seen before the Enter, and for a
  line under four bytes where the terminal could not be asked. The whole line
  must be seen echoed, so keys a program took without an Enter (`read -s -n`)
  never prefix a recorded line, and an edited line is recorded without its
  text. On the container tier, where the terminal cannot be asked, a password
  that also appears in the prompt printed while it was typed can be recorded.
  Lines typed ahead while a command runs are not screened. While another
  program has the terminal they are not recorded; while the shell itself is
  busy (a builtin, the gap between commands) the terminal is in canonical
  mode, and they are recorded without their text. So is every line after
  `set +o emacs +o vi`, which makes bash read its prompt in canonical mode;
  those lines are still screened. Keys typed ahead
  while a command runs are echoed as they arrive, so they are in the recorded
  output as they were on screen;
- which program has the keys is asked of the terminal on the process and none
  tiers (its foreground process group against the shell's); while it is not
  the shell, keys are neither screened nor recorded. On the container tier
  the engine's CLI holds the terminal, and a switch to the alternate screen is
  the only sign; printing that sequence switches screening and recording off
  there until it is switched back;
- ending a shell (Kill, closing its tab, deleting the session, the idle or
  twelve-hour limit, or `exit`) hangs it up, bash hangs up its background
  jobs, and then every process left in the shell's session is stopped and
  killed, pass after pass until none is new (`Leader` in
  `internal/sandbox/leader.go`). That covers `nohup`, `disown`, `( cmd & )`,
  `trap '' HUP` and a job that keeps forking. The sweep runs only while the
  exited shell is unreaped, so its process id, and with it the session id,
  cannot belong to anything else, and only after the shell's recorded start
  time matches. On Linux each signal goes through a pidfd, so a reused process
  id is never signalled. On macOS a member is checked with `getsid` and then
  signalled by number, both the SIGSTOP and the SIGKILL. If the id were reused
  in between, which needs a pid wrap within microseconds and is not reachable
  in practice, the signal would land on a foreign process: a stop is found on
  the recheck and undone with SIGCONT, a kill is not. A strict fix
  there would need signalling by audit token. When the sweep cannot run (the
  start time unreadable, or no pidfd on a Linux kernel before 5.3) the server
  logs that containment did not run. A process that starts a session of its own
  (`setsid`, a daemon) escapes and runs until it ends, within the sandbox;
  bubblewrap and the container tier end everything regardless. On the none
  and process tiers a shell shares the server's user and so its process
  limit: a fork bomb there can exhaust it for the server too. A pids cgroup
  per shell is the planned follow-up.

What the shell can reach is the tier's, as for the agent's commands, but a
person now has it interactively. On the macOS process tier, Seatbelt denies
writes outside the workspace and reads of `.abhed` and five credential paths
(`~/.ssh`, `~/.aws`, `~/.kube`, `~/.gnupg`, `~/.docker/config.json`); other
files in the home directory, such as `~/.config/gh`, `~/.netrc`,
`~/.git-credentials` and `~/.npmrc`, are readable, and the shell can signal
other processes running as the same user. The environment is an allowlist
(`internal/sandbox/process.go`, `env`): no provider keys, no vault secrets, no
`ABHED_` settings. On the `none` tier the shell has the server's environment
without its `ABHED_` settings, which leaves anything else the operator
exported, and nothing contains it.

`sandbox.terminal: "lines"` returns the terminal to one policy-checked `bash`
call per line with no shell state, and a managed policy with deny rules for
`bash` or for every tool (`*`), or with a policy hook, gets that mode
automatically. Even then a rule checks the line, not what
a script the line runs does. On the `none` tier the shell runs on the host, and
the terminal banner and status bar say so.

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

With the network off, a command cannot see the host's network either. On
Linux the network namespace has only loopback. On the macOS process tier,
Seatbelt also denies the routing sysctls that list interfaces and addresses,
routing sockets, and the system configuration and network services, so
`ifconfig`, `netstat -rn`, `route -n get`, `scutil --nwi` and `ipconfig` fail
instead of showing the LAN address, the gateway or a VPN tunnel (`TestProcessSandboxHidesTheHostsNetwork`). What remains visible
on macOS: the hardware ports and their MAC addresses, which come from the I/O
registry (`networksetup -listallhardwareports`, `ioreg`), and the host name.
Programs that enumerate interfaces get an error rather than a loopback-only
list, as Node's `os.networkInterfaces()` does; Python, git, `go build` and
pytest are unaffected.

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
| Config mounted read-only at the managed path | `--volume $CONFIG:/etc/abhed/config.json:ro` | Loading config at the managed path sets `Managed`, refusing `bypass` mode, and binds every key it sets against flags and SDK options from inside the container |
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
- **A small team.** Zybuu is a small company. There is no security team, no
  on-call rotation, and no bus-factor mitigation beyond what is written down
  in this repository. See `SECURITY.md` for the
  response times that implies.
