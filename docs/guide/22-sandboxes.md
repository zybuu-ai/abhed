# Sandboxes over the API

`abhed serve` can manage sandboxes as objects of their own: create one, run
commands in it, stop it, start it again, delete it, all over `/v1/sandboxes`.
Each sandbox is confined exactly as a session's commands are today (the
`process` driver, the default, builds it from your `sandbox` settings), and
every change to one is recorded with who asked for it. A sandbox may name
another driver the configuration registers: `docker` or `podman`, when
`control_plane.docker` or `control_plane.podman` is enabled
([below](#the-docker-and-podman-drivers)), and `microvm`, when the managed
`sandbox.microvm_socket` is set ([The microVM driver](24-microvm.md)). A
configured driver that cannot be reached is named when `abhed serve` starts,
and sandboxes asking for it fail with `unavailable` rather than running
under another.

`abhed serve` can also give every session a sandbox of its own, in place of
the one sandbox all sessions share: see [per-session
sandboxes](#per-session-sandboxes).

This is the control plane. It is off unless you turn it on, and with it off
`abhed serve` behaves exactly as before. The full contract, including the
parts still to come, is [the control-plane contract](../architecture/control-plane-contract.md).

## Turning it on

```json
"control_plane": {
  "enabled": true,
  "workspace_root": "/srv/abhed/sandboxes"
}
```

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Serve `/v1/sandboxes` and run the reconciler. |
| `workspace_root` | a folder in the control plane's state folder | Gives each sandbox a folder of its own, `<root>/<id>`, mode `0700`, removed when the sandbox is deleted. Must be absolute, outside the server's workspace and outside any `.abhed`, and not writable by others. Unset, the root is `~/Library/Application Support/Abhed/control-plane/sandboxes` on macOS and `$XDG_STATE_HOME/abhed/control-plane/sandboxes` (`~/.local/state/…` without it) elsewhere. A sandbox's commands can neither read nor write the other folders under the root (on macOS the `process` driver's profile denies them; on Linux they are never mounted in), so a root inside a `sandbox.read_only_paths` entry or a skills folder, which would show them, is refused. |
| `shared_workspace` | `false` | Every sandbox works in the server's workspace instead, as sessions do, and sees what the others and the server's sessions write there. Set it only when that is what you want; it cannot be combined with `workspace_root`. |
| `snapshot_dir` | a folder in the control plane's state folder | Where sandbox snapshots are kept: `snapshots` beside the default `sandboxes` folder. Must be absolute and outside every workspace. |
| `ssh.idle_timeout_seconds` | 3600 | An SSH connection with no traffic either way for this long is closed. |
| `ssh.max_session_seconds` | 43200 | Longest an SSH connection stays open. Its commands are bounded by this, not by `exec_timeout_seconds`. |
| `ssh_host_key` | a key the server makes | The `:ssh` relay's host key file, so several servers can share one key; see [SSH](#streams-attach-exec-with-stdin-forward-and-ssh). |
| `expose` | `false` | Lets `:expose` publish a sandbox's port to whoever asked; see [exposed services](#the-api). |
| `expose_max_ttl_seconds` | 3600 | Longest an exposure lives. |
| `sqlite_path` | `~/.abhed/controlplane/sandboxes.db` | The SQLite file sandboxes are kept in when `storage.driver` is not `postgres`; see [where sandboxes are kept](#where-sandboxes-are-kept). Must be absolute. |
| `exec_timeout_seconds` | 600 | Longest one `:exec` may run. |
| `exec_output_bytes` | 16 MiB | Most output one `:exec` may write, stdout and stderr together. |
| `restart_policy` | `never` | `never`, `on-failure` or `always`; see [restarts](#restarts-and-generations). |
| `max_restarts` | 5 | Restarts the policy makes before it gives up. |
| `restart_backoff_seconds` | 10 | Delay before the first restart; each next one doubles, up to an hour. |
| `mutations_per_minute` | 120 | Changes to sandboxes one caller may make a minute. |
| `exec_per_minute` | 60 | `:exec` calls one caller may make a minute. |
| `reconcile_seconds` | 10 | How often every sandbox is checked against its driver. |
| `sandbox_cpus` | unset | CPUs each sandbox may use, such as `0.5` or `2`. Unset leaves it to the driver. |
| `sandbox_memory_mb` | unset | Memory each sandbox may use, in MiB. Unset leaves it to the driver. |

A size is passed to the driver, which holds the sandbox to it or refuses
it. The `process` driver cannot hold one sandbox to a size, so a sandbox
with `sandbox_cpus` or `sandbox_memory_mb` in force goes to `Error` under it
rather than run unbounded; use a driver that bounds each sandbox.

The weakest isolation a sandbox may run under is your existing
`sandbox.min_tier`.

| `per_session` | `false` | Give each session of `abhed serve` a sandbox of its own; see [per-session sandboxes](#per-session-sandboxes). |
| `sessions` | | How those sandboxes are made; see below. |
| `docker`, `podman` | off | Register the container drivers; see [below](#the-docker-and-podman-drivers). |

## Where sandboxes are kept

Sandboxes, idempotency records and the record of every change survive a
restart of the server, in one of two places:

- **Postgres**, when `storage.driver` is `postgres`: the same database as
  sessions. Run `abhed migrate` after upgrading, as for any new table (the
  server refuses to turn the control plane on against an older schema and
  names what is missing). The runtime role gets `SELECT`, `INSERT`,
  `UPDATE` and `DELETE` on `sandboxes` and `sandbox_idempotency`,
  `SELECT`, `INSERT` and `DELETE` on `sandbox_templates`, and only
  `SELECT` and `INSERT` on `sandbox_audit`, which is append-only like
  `events`.
- **SQLite**, otherwise: one file on the server's host,
  `~/.abhed/controlplane/sandboxes.db` for the user `abhed serve` runs as,
  or `control_plane.sqlite_path`. A server run as a system service should set
`sqlite_path`, for example to `/var/lib/abhed/sandboxes.db`, rather than
rely on its user's home directory. It suits one server on one host; several
  servers sharing sandboxes need Postgres. The file is created when the
  server first starts with the control plane on, and its tables are
  created and upgraded by the server itself, so there is no `abhed migrate`
  step.

The SQLite file is the server's own state, so it is held to the same rules
as the accounts and secrets files:

- Its folder is created `0700` and the file `0600`. The server refuses to
  start when the folder or the file can be read or written by other users,
  belongs to another user, or when the file is a symbolic link or has a
  second name (a hard link). It never loosens or repairs them for you; fix
  the mode (`chmod 700` the folder, `chmod 600` the file) or pick another
  path.
- It is refused inside the workspace (its `.abhed` included), an added
  directory, `workspace_root`, a temp folder or a cache: anywhere a
  command could move or replace it. Under `~/.abhed` is always fine.
- The agent's tools and every sandboxed command are denied the file and
  the `-wal` and `-shm` files SQLite keeps beside it.
- `sandbox_audit` is append-only: triggers refuse every `UPDATE` and
  `DELETE` of it, and an `INSERT OR REPLACE` over an existing row. As with
  `storage.single_role` on Postgres, this guards the record against
  mistakes, not against whoever can write the file itself.

The file uses SQLite's write-ahead log with full sync on commit, so a
server that is killed mid-change loses that change and nothing committed
before it. To back it up while the server runs, use `sqlite3
sandboxes.db ".backup copy.db"`, or stop the server and copy the file and
any `-wal` file beside it.

The memory store in the Go API (`controlplane.NewMemory`) is for tests;
`abhed serve` never falls back to it.

## Check before you serve

```bash
abhed serve -check -addr :8080
```

Loads the configuration, resolves the model, reads the secrets store, builds
the sandbox, builds sign-in, opens storage, builds the control plane when it
is on (naming its store, `sqlite (<path>)` or `postgres (tenant …)`, and
listing every setting, where its value came from, and which are
ceilings) and binds the listen address once to release it. It serves nothing.
Each check prints `ok` or `FAIL`; the exit status is 0 only when all pass.
The check creates, migrates and writes nothing: with no SQLite file yet it
says where serving would create it (after checking the folder), and for an
existing file it reads the schema version and says whether serving will
migrate it. `abhed doctor` names the store too, on its
`sandboxes` line, without opening it.

## Per-session sandboxes

By default every session `abhed serve` runs shares one sandbox and the
server's workspace. With `per_session` on, each session gets a control-plane
sandbox of its own instead, made from a template, and works in that
sandbox's folder:

```json
"control_plane": {
  "enabled": true,
  "per_session": true,
  "sessions": {"scope": "session", "driver": "podman", "retention": "stop", "warm_pool": 2}
}
```

| Key | Default | Meaning |
|---|---|---|
| `sessions.scope` | `session` | `session`: one sandbox per session. `user`: one per user, which all of that user's sessions share. |
| `sessions.template` | `session` | The template each sandbox is made from. Letters, digits, `.`, `_` and `-`. |
| `sessions.driver` | `process` | The driver each runs under: `process`, `docker` or `podman`. `microvm` cannot run a session's commands yet and is refused. |
| `sessions.retention` | `stop` | What happens to the sandboxes when the server stops: `stop` them, `delete` them (and their folders), or `keep` them running. |
| `sessions.warm_pool` | 0 | Ready sandboxes kept for new sessions to claim, at most 16. |
| `sessions.ready_seconds` | 120 | How long a new or continued session waits for its sandbox before it is refused. |

What changes for a session:

- **Its workspace is its sandbox's folder**, `<workspace_root>/<id>`, never
  the server's workspace. The agent's file tools, the workbench's tree,
  file views, saves, uploads and downloads, and the folder `session.started`
  names are all that folder. Another session's folder is outside it, by
  path or through a link.
- **Its commands run in its sandbox.** The bash tool, background commands
  and the workbench terminal go through the control plane to the sandbox's
  driver, with git protection, `.abhed` hidden, and the network as the
  driver builds it. Each command is in the control plane's record as an
  `exec` by the session's user (with `"via": "session"`), put to a guard
  and interceptors as any `:exec` is. The bash tool's own timeout applies,
  not `exec_timeout_seconds`.
- **The record links them.** The sandbox carries the reserved label
  `abhed.dev/session=<session id>` (or `abhed.dev/session-user=<digest>`
  per user), is owned by `system:sessions`, and its `create` or `claim`
  event names the session. The session's record gets `sandbox.bound`,
  with the sandbox's id, generation, driver, tier and folder, before the
  first command this server runs in it.
- **Continuing a session reattaches.** A stopped sandbox, or one a restart
  of the server lost, is started again as a new generation, with its
  folder as it was. A session whose sandbox is gone gets a new, empty one.
- **Deleting a session deletes its sandbox** and its folder. A per-user
  sandbox stays for the user's other sessions.
- **A restart of the server removes the sandboxes of sessions that no
  longer exist**, found by their label.

A session is refused rather than run with less: when its sandbox cannot be
made `Ready` in `ready_seconds`, when its driver reports no isolation
(tier `none`), or when the session is in a tenant other than the one the
control plane's store was opened for. A sandbox made for a session that was
then refused is deleted.

With per-session sandboxes, a file attached before a session exists has no
folder to go to, so `POST /v1/uploads` answers `409`; start the session,
then attach it.

**Warm pool.** With `warm_pool` above 0, the server keeps that many `Ready`
sandboxes made from the template, owned by `system:sessions` and labelled
`abhed.dev/pool=<template>`. A new session claims one in a single update
guarded on its resource version, so two sessions never get the same one;
the claim relabels it for the session, raises its generation (nothing tied
to it in the pool carries over) and is recorded as `claim` by the session's
user, put to a guard as an update. A claimed sandbox never goes back to the
pool. The pool is refilled in the background, failed members are replaced,
it never holds more than `warm_pool`, and it is drained (its sandboxes
deleted) when the server stops. The pool is in the control plane's store,
which holds one tenant, so it is never shared across tenants. When the pool
is empty a session gets a new sandbox.

**Under an edition's workspaces.** When an edition confines sandbox calls
to a scope, such as a Team workspace, a session's sandbox is made in the
workspace its user acts in: it carries that workspace's label, so the
workspace's quotas count it, and the workspace's policy rules judge the
commands run in it. What the server does for the session (making,
claiming, starting and removing its sandbox, and filling the warm pool) is
shown to the edition's guard as the session's own, so a four-eyes rule on
creating or deleting sandboxes does not hold a session someone has already
started. Warm pools are kept per workspace, made
when a workspace's first session starts, and each fill is counted in that
workspace. A user who is in no workspace gets no session.

**Isolation.** Under the `process` driver each session's commands can
neither read nor write the other sessions' folders; under `docker` and
`podman` only the session's own folder, of all the sandboxes' folders, is
mounted. Under the fence tier each
sandbox is a fence of its own, with its own cgroup. Folders in
`additional_dirs` and skills folders are added to every session, as before,
so they are shared; so are MCP servers and extensions, which belong to the
server.

**Not yet.** Under several replicas (Enterprise HA) a `process` sandbox
lives in the replica that made it, so per-session sandboxes need a driver
every replica reaches. The microVM driver cannot hand a session's commands
out yet. Workspace scopes from an edition's sandbox caller are not applied
to a session's sandbox.

`abhed serve -check` checks all of this: a `per-session sandboxes` line
fails when the session driver is not registered, cannot be reached or
cannot run a session's commands. `abhed doctor` names the setting on its
`sessions` line. With `per_session` off, sessions behave exactly as before.

## The docker and podman drivers

```json
"control_plane": {
  "enabled": true,
  "podman": {"enabled": true, "image": "ghcr.io/example/dev:1"}
}
```

Each sandbox on them is one long-lived container; what every container
gets, and every admission rule, is in [Container drivers](23-container-drivers.md).

| Key (under `docker` or `podman`) | Who may set it | Meaning |
|---|---|---|
| `enabled` | any configuration | Register the driver. |
| `image` | any configuration | The default image; `ABHED_SANDBOX_IMAGE` or the container tier's when unset. |
| `binary` | managed only | The engine's program; found on `PATH` by its name when unset. |
| `images` | managed only | Further images a sandbox may ask for in `Config["image"]`. |
| `mounts` | managed only | Host folders every sandbox gets: `{"host", "target", "rw"?, "relabel"?}`. |
| `mount_roots` | managed only | Folders under which a sandbox may ask for its own mounts: `{"path", "rw"?, "relabel"?}`. |
| `gpus` | managed only | CDI device names a sandbox may ask for, such as `nvidia.com/gpu=all`. |
| `cpus` | managed only | CPUs each sandbox may use, 2 when unset. |
| `seccomp_profile` | managed only | A seccomp profile file in place of the engine's default; `unconfined` is refused. |

Each managed-only key widens what a sandbox can reach or loosens a bound, so
a user's or workspace's file that sets one is set aside and named, as for
other managed-only settings. Options that could never be admitted (a raw
device path for a GPU, an unconfined profile) stop `abhed serve` at start.

An engine that does not answer leaves its driver registered: every sandbox
on it fails with `unavailable` until the engine answers, and none is handed
to another driver. `abhed serve` names it at start, `abhed doctor` shows it
as `UNAVAILABLE`, and `abhed serve -check` fails its `driver docker` or
`driver podman` line. When it is the per-session driver, `abhed serve`
refuses to start.

## Settings: global and per sandbox

Every setting above except the rate limits and `reconcile_seconds` can also
be set on one sandbox, under `settings` with its registry name:

| Registry name | Configuration key |
|---|---|
| `exec.timeout_seconds` | `control_plane.exec_timeout_seconds` |
| `exec.output_bytes` | `control_plane.exec_output_bytes` |
| `restart.policy` | `control_plane.restart_policy` |
| `restart.max_restarts` | `control_plane.max_restarts` |
| `restart.backoff_seconds` | `control_plane.restart_backoff_seconds` |
| `isolation.min_tier` | `sandbox.min_tier` |
| `resources.cpus` | `control_plane.sandbox_cpus` |
| `resources.memory_mb` | `control_plane.sandbox_memory_mb` |
| `ssh.idle_timeout_seconds` | `control_plane.ssh.idle_timeout_seconds` |
| `ssh.max_session_seconds` | `control_plane.ssh.max_session_seconds` |
| `rate.mutations_per_minute` | `control_plane.mutations_per_minute` (server only) |
| `rate.exec_per_minute` | `control_plane.exec_per_minute` (server only) |
| `reconcile.interval_seconds` | `control_plane.reconcile_seconds` (server only) |

```json
{"template": "default", "settings": {"exec.timeout_seconds": "60", "restart.policy": "on-failure"}}
```

A value the [managed file](02-configuration.md#the-managed-file) sets is a
**ceiling, not an override**: a sandbox may ask for something tighter (a
shorter timeout, fewer restarts, stronger isolation) and is refused with
`400` when it asks for something looser. A ceiling tightened later holds
sandboxes created before it too. Without a managed value, a sandbox may set
any value the setting takes. `isolation.min_tier` is always a floor: a
sandbox may require stronger isolation than `sandbox.min_tier`, never
weaker. The strongest is `microvm`, which only the microVM driver meets; it
meets every other floor.

## The API

All routes need the same sign-in as the rest of the API. A sandbox belongs
to whoever created it; others in the tenant get `404` for it, and members
of the admin group see and act on every sandbox. The server's control plane
serves the tenant its storage was opened for; a request acting in another
tenant gets `403`. Browser requests from another origin are refused as for
every other route.

| Call | Result |
|---|---|
| `POST /v1/sandboxes` `{template, driver?, labels?, settings?}` | `201`, the sandbox in `Provisioning` |
| `GET /v1/sandboxes?selector=&state=&limit=&after=` | `200 {items, next}` |
| `GET /v1/sandboxes/{id}` | `200`, with `ETag` |
| `PATCH /v1/sandboxes/{id}` (JSON merge patch of `labels`, `template`, `settings`) | `200`; needs `If-Match`, `428` without it |
| `DELETE /v1/sandboxes/{id}` | `202`, the sandbox in `Deleting` |
| `POST /v1/sandboxes/{id}:start`, `:stop` `{grace_seconds?}` | `202` |
| `POST /v1/sandboxes/{id}:exec` `{argv \| command, cwd?, env?}` | a stream, below |
| `POST /v1/sandboxes/{id}:snapshot` `{name?}` | `201 {snapshot, generation}`; `snapshot` carries its `digest` |
| `POST /v1/sandboxes/{id}:restore` `{snapshot}` | `202`, the sandbox in `Starting` as a new generation |
| `POST /v1/sandboxes/{id}:cp?direction=in&path=…` with a tar body | `200` |
| `POST /v1/sandboxes/{id}:cp?direction=out&path=…` (or `{direction, path}`) | `200`, a tar stream |
| `POST /v1/sandboxes/{id}:expose` `{port, ttl_seconds?}` | `201 {id, url, path, expires_at}` |
| `GET /v1/sandboxes/{id}:attach?rows=&cols=&cwd=` | a WebSocket: an interactive shell, [below](#streams-attach-exec-with-stdin-forward-and-ssh) |
| `GET /v1/sandboxes/{id}:exec` | a WebSocket: one command with stdin |
| `GET /v1/sandboxes/{id}:forward?port=N` | a WebSocket: a byte stream to a port inside the sandbox |
| `POST /v1/sandboxes/{id}:ssh` | `200 {host_alias, user, host_key, fingerprint, known_hosts, relay, subprotocol, generation}` |
| `GET /v1/sandboxes/{id}:ssh` | a WebSocket: the SSH relay |
| `GET /v1/sandboxes/{id}:events?after=&limit=` | `200 {items, next}`: the sandbox's record, oldest first |
| `POST /v1/sandbox-templates` `{name, description?, driver?, labels?, settings?}` | `201`, the template; administrators only |
| `GET /v1/sandbox-templates?selector=&limit=&after=` | `200 {items, next}` |
| `GET /v1/sandbox-templates/{name}` | `200`, with `ETag` |
| `DELETE /v1/sandbox-templates/{name}` | `200`; administrators only, `If-Match` honoured |

Snapshots, copies, exposures, `:attach` and `:forward` use what the
sandbox's driver can do. A driver that cannot answers `501` with code
`unsupported`, before anything is recorded: the `process` driver
snapshots, restores, attaches and forwards but does not copy, and the
microVM driver answers for itself.

**Snapshots.** `:snapshot` takes one from a `Ready` sandbox (while its
commands may write) or a stopped one (consistent). `:restore` takes the
`snapshot` object `:snapshot` returned, only while the sandbox is
`Stopped`, `Completed` or `Error`, and starts it again as a new generation.
The driver refuses a snapshot of another sandbox or one whose content does
not match its digest. Snapshots are kept in `control_plane.snapshot_dir`
and deleted with their sandbox; see [Snapshots and
forwarding](25-snapshots-and-forwarding.md).

**Copies.** Only a `Ready` sandbox takes a copy. The driver keeps paths in
the workspace and checks every archive entry. A copy out is bounded by
`exec.output_bytes`; one that fails after its first byte ends the stream
and says why in the `X-Abhed-Error` trailer.

**Exposed services.** Off unless `control_plane.expose` is on. Then
`:expose` publishes a port of a `Ready` sandbox at `path` (under
`/v1/exposed/`), for the caller alone, in its tenant, behind the server's
sign-in, until `ttl_seconds` (at most `expose_max_ttl_seconds`), or until
the sandbox stops or starts again. Everyone else gets `404`. The bytes go
through the driver's forward, so a sandbox without a network serves without
being given one.

Every snapshot, restore, copy and exposure is in the record twice: the
request with who asked, and the driver's own `sandbox.snapshot`,
`sandbox.restore` or `sandbox.expose` event as `system:driver`.

**The record of a sandbox.** `:events` returns what the record holds for
one sandbox, oldest first, to whoever may see the sandbox: each request and
its outcome, the reconciler's transitions, and the driver's events, each
with its sequence number. Pass the last `seq` read as `after` for the next
page (`next` is set while more remain).

Errors are `{"error": {"code", "message"}}` with codes `invalid`,
`not_found`, `conflict`, `precondition_failed`, `unsupported`,
`unavailable`, `forbidden` and `internal`. A store, record or driver that
cannot be reached is `503`; nothing is assumed to have worked. An edition
with workspaces adds `403` with `quota_exceeded`, and `approval_required`
with the approval to ask for in the `Abhed-Approval` response header; once
it is approved, send the same request again with that header.

Selectors: `team=payments`, `tier!=gold`, `env in (dev,test)`, `!archived`,
comma-separated, all of which must hold.

**Idempotency.** Send `Idempotency-Key` on any `POST`, `PATCH` or `DELETE`
and a repeat with the same body gets the first response again with
`Idempotent-Replayed: true`; with a different body, `422`; while the first is
still running, `409`. A request refused before it changed anything frees its
key. A repeated `:exec` does not run again: it gets the last frame of the
first run.

**Versions.** Every sandbox response carries `ETag: "<resource_version>"`.
Send it back as `If-Match` and a change made by someone else in between is
`412`, not overwritten.

### Running a command

```bash
curl -N -X POST "$B/v1/sandboxes/$ID:exec" -d '{"command": "make test"}'
```

The answer is `application/x-ndjson`, one frame per line, with output in
base64:

```
{"stdout":"b2sK"}
{"stderr":"d2FybmluZwo="}
{"exit":0}
```

It ends with `{"exit": n}` when the command ran to an exit code, or
`{"error": {...}}` when it did not: past `exec.output_bytes` (code
`invalid`; output stops at exactly that many bytes and the command is
stopped), past `exec.timeout_seconds`, or a driver failure. Only a `Ready`
sandbox takes a command; any other state is `409`. The response header
`X-Abhed-Generation` is the generation it ran in.

The record keeps who ran a command, in which sandbox and generation, and how
it ended. The command line itself goes through your secrets redaction first.

A command a deny rule or an extension's hook refuses (your
`permissions.deny`, as for the agent's `bash` and the workbench's
terminal) is refused with `403` and code `forbidden`, and the refusal is
recorded. Only a refusal stops a command here: rules that would ask the
agent first do not ask a person who runs a command themselves. A command
given as `argv` is judged as its words joined by spaces.

### Streams: attach, exec with stdin, forward and SSH

`:attach`, `:exec` with stdin, `:forward` and the `:ssh` relay are
WebSockets (RFC 6455, over HTTP/1.1; behind a proxy, let it pass
`Upgrade`). The client offers the verb's subprotocol in
`Sec-WebSocket-Protocol`: `abhed.attach.v1`, `abhed.exec.v1`,
`abhed.forward.v1` or `abhed.ssh.v1`. Before the connection is upgraded the
request is signed in like any other, refused with `403` when it comes from
another site's page (its `Origin` is not this server or one in
`server.allowed_origins`), and refused with `404` when the caller cannot
see the sandbox; then the driver's capability, the sandbox's state
(`Ready` only) and the rate limit are checked, so each of those failures
is an ordinary HTTP answer with the usual error body. A request without
the upgrade gets `426`.

While a stream is open the caller is authorised again every few seconds
(as the workbench's streams are): once their sign-in ends, or they can no
longer see the sandbox, the stream ends. It also ends when the sandbox
stops, is deleted or starts as a new generation, when the client stops
answering pings for a minute, and after twelve hours. One caller may have
16 streams open at once; each opening counts against `exec_per_minute`.
Messages are at most 1 MiB.

Text messages carry JSON control messages; binary messages carry bytes.
Each stream that is not SSH starts with `{"type": "started", "generation": n}`
and ends with `{"type": "exit", "code": n}` when what ran ended with an
exit status, or `{"type": "error", "error": {"code", "message"}}`, then a
normal close.

**`:attach`** opens the sandbox's interactive shell on a terminal of
`rows` × `cols`, in `cwd` (inside the workspace) when given. Binary
messages are keys to the shell and its output back;
`{"type": "resize", "rows": 40, "cols": 120}` changes the terminal's size.
It is followed the way the workbench follows its interactive terminal
([Working by hand](16-workbench.md#working-by-hand)) where the terminal
cannot be asked: each line you enter is rebuilt from your keys, put to the
deny rules at its Enter, and recorded (`attach.input`, without its text
when the terminal did not show it as typed). A line the rules refuse never
reaches the shell: the terminal says why and the line is dropped, and the
refusal is recorded (`attach.refused`). This is best effort: the shell
decides what a line means, so the sandbox is the boundary. When the
server's terminal must judge every line (`sandbox.terminal` is
`"lines"`), or the managed configuration sets deny rules or extensions
that could refuse a command, `:attach` is refused with `403` and you run
each command with `:exec` instead. The shell's end is recorded with its
exit status, its duration, the bytes each way and the last 8 KiB of its
output, with withheld lines taken out and your secrets redaction applied.

**`:exec` with stdin.** Open `GET …:exec` as a WebSocket and send the
request as the first message, the same JSON as the `POST` form. Then
binary messages are stdin, and `{"type": "eof"}` closes it. Output comes
back as binary messages whose first byte says the stream (1 stdout, 2
stderr). The command is judged, bounded and recorded as a `POST` one is.
`POST …:exec` with `"stdin": true` answers `400`, naming the WebSocket form.

**`:forward?port=N`** carries a byte stream to `127.0.0.1:N` inside the
sandbox, through the driver's own channel (see [Snapshots and
forwarding](25-snapshots-and-forwarding.md#forwarding-a-port)): binary
messages both ways, and `{"type": "eof"}` from the client ends what it
sends while the reply is still read. Nothing listening on the port is an
HTTP `503` before the upgrade. The record has who opened it and, when it
closes, the bytes each way.

**SSH.** `abhed serve` runs an SSH server for its sandboxes with no port
of its own: the only way in is `GET …:ssh`, a WebSocket carrying the raw
SSH connection, which the server has already signed in and bound to the
caller and the sandbox. So SSH asks for no password or key, and any user
name is accepted. `POST …:ssh` returns what a client needs: the server's
host key (`host_key`, and a `known_hosts` line for `host_alias`), and the
relay's path and subprotocol. For OpenSSH and editors that use it:

```
Host abhed-sbx_6f1c0a9e4b2d7c13
  User sandbox
  HostKeyAlias abhed-sbx_6f1c0a9e4b2d7c13
  ProxyCommand abhed sandbox connect --stdio sbx_6f1c0a9e4b2d7c13
```

`abhed sandbox connect --stdio ID` joins its stdin and stdout to the relay,
signed in with the current profile (`-profile NAME` for another; refused
inside an agent's command; see
[TLS and command-line sign-in](26-tls-and-cli-login.md)). `abhed sandbox
ssh` and `abhed sandbox ssh-config`, [below](#from-the-command-line-abhed-sandbox),
write the host key and these lines for you.

Each SSH session is its own recorded call, judged as the verb it amounts
to: a shell is an `:attach` (`via: ssh` in the record; refused where
`:attach` is), a command is an `:exec` (judged by the deny rules, its
output held to `exec.output_bytes` on its channel, and run without a
terminal even when one was asked for), and a `direct-tcpip` channel to
`127.0.0.1`, `::1` or `localhost` is a `:forward`. The connection itself
is recorded as `ssh`, with its sessions, forwards and refusals.

An SSH connection has limits of its own, so a server an editor starts
over SSH keeps running: it closes after `ssh.idle_timeout_seconds` with
no traffic either way (an hour by default; a client's keepalives count)
and `ssh.max_session_seconds` after it opened (twelve hours by default),
and a command run over it is bounded by what is left of that, not by
`exec.timeout_seconds` (the record notes the bound as `timeout_seconds`).
Both can be set per sandbox, and a managed value is a ceiling a sandbox
may only tighten. Each SSH session also counts against `exec_per_minute`. Everything
else is refused: remote forwarding, agent and X11 forwarding, environment
variables, signals, and subsystems.

**SFTP is not served.** The driver interface moves files as tar archives
(`:cp`), not one open file at a time, so an SFTP server would have to copy
a whole folder to list it. Copy files with `:cp` instead. An
SFTP client is told so and refused; `scp` without `-s` runs the sandbox's
own `scp` as a command, judged like any other.

The host key is an Ed25519 key in the control plane's state folder
(`ssh_host_ed25519_key` beside the default `sandboxes` folder, mode
`0600`), made the first time the server starts with the control plane on.
A key file that others may read, or that is a link, leaves the relay off:
the server says so when it starts, and `:ssh` answers `501`. No command or
file tool may read it.

Several servers behind one name (Kubernetes replicas, or a server whose
state folder does not survive a restart) should share one key, set with
`control_plane.ssh_host_key`: an absolute path to an OpenSSH private key
(`ssh-keygen -t ed25519 -N '' -f relay_key`) owned by the server's user,
readable by that user alone (`0600` or `0400`), not a link, and outside every workspace, added directory,
sandbox folder, temp folder and cache. A configured key that cannot be read
refuses the start instead of leaving the relay off. It is a protected state
file like the TLS key. From a Kubernetes Secret, mount the one file with
`subPath` (the Secret volume's own files are links) and `defaultMode: 0400`
under a `runAsUser` that owns it, or copy it into place with an init
container. `abhed serve -check` and `abhed doctor` name the key in use and
its fingerprint. The Helm chart copies one in from a Secret with
`controlPlane.sshHostKey`; see [Running on Kubernetes](27-kubernetes.md#files-that-must-be-private).

## Templates

A template is a named set of defaults a sandbox is created from: a driver,
labels and settings. `POST /v1/sandboxes` with `"template": "web"` copies
them into the new sandbox when the server has a template by that name:

- **Driver.** A template that names one fixes it: a create naming another
  is `400`.
- **Labels.** The template's labels select it (`?selector=` on the list)
  and are given to every sandbox made from it; the create's own labels win
  where both set a key. Labels under `abhed.dev/` are the server's, as on
  sandboxes.
- **Settings.** Each sandbox gets the template's settings. A create may
  tighten one (a shorter `exec.timeout_seconds`, a stronger
  `isolation.min_tier`) and is refused with `400` for a looser one; the
  ceilings of the managed file hold over both.

What a template gives is copied at create: changing the sandbox's
`template` later, or removing the template, leaves its driver, labels and
settings as they are. A name with no template object stays a plain name,
as it was before templates were kept, so sessions' sandboxes and existing
clients behave as before; `abhed sandbox create -template NAME` refuses a
name the server has no template for, other than `default`.

Templates live in the same store as sandboxes (the `sandbox_templates`
table in Postgres and SQLite), one set per tenant, versioned with the same
`ETag`s. Everyone signed in to the tenant reads them; only a member of the
admin group, acting for the whole tenant, creates or removes one, and each
change is in the record as `template.create` or `template.delete`. A
server whose store keeps no templates answers `501`.

## From the command line: `abhed sandbox`

`abhed sandbox` drives all of this as the profile `abhed login` saved
([TLS and command-line sign-in](26-tls-and-cli-login.md)). Every command
takes `-profile NAME` (default: the current profile) or `-server URL`
(the profile signed in to that server), and `-o table` (the default) or
`-o json`. There is no YAML output: Abhed has no YAML writer.

```bash
abhed login https://abhed.internal
abhed sandbox template create web -l team=a -min-tier container -set exec.timeout_seconds=300   # admins
abhed sandbox create -template web -l app=shop -wait
abhed sandbox list -l team=a,app=shop -state Ready
abhed sandbox exec sbx_6f1c0a9e4b2d7c13 -- make test
echo data | abhed sandbox exec -i sbx_6f1c0a9e4b2d7c13 -- wc -c
abhed sandbox connect sbx_6f1c0a9e4b2d7c13
abhed sandbox cp ./src sbx_6f1c0a9e4b2d7c13:work
abhed sandbox forward sbx_6f1c0a9e4b2d7c13 8080:80
```

| Command | What it does |
|---|---|
| `create [-template T] [-l k=v]... [-set k=v]... [-min-tier T] [-cpus N] [-memory-mb N] [-driver D] [-wait]` | Creates a sandbox (`default` template unless named); `-wait` waits until it is `Ready`. |
| `list [-l SELECTOR] [-state S] [-limit N] [-after CURSOR] [-all]` | One page, or every page with `-all`; a table prints the next cursor on stderr. |
| `get ID` | The sandbox, its settings and conditions. |
| `update ID [-l k=v] [-unlabel k] [-set k=v] [-unset k] [-template T] [-if-match V]` | A merge patch sent with `If-Match`: the version just read, or the one you give. A change made in between is exit 5. |
| `exec [-i] [-c LINE] [-cwd DIR] [-e K=V] ID [--] COMMAND [ARG...]` | Runs a command and exits with its status. `-i` sends stdin over the WebSocket form. |
| `connect ID [-cwd DIR] [-escape ^X\|none]` | An interactive shell on a terminal in raw mode, resized with yours. **Ctrl-]** leaves (the shell ends with the connection); `-escape` picks another key. |
| `connect --stdio ID` | stdin and stdout joined to the SSH relay: ssh's `ProxyCommand`. |
| `ssh ID [-- SSH ARGS]` | Runs your `ssh` through the relay, with the server's host key. |
| `ssh-config [ID...] [-l SELECTOR]` | Prints a `Host` block per sandbox (every `Ready` one by default) for `~/.ssh/config` or an editor. |
| `cp SRC ID:DIR`, `cp ID:PATH DIR` | Copies a file or folder in, or out into an existing folder. |
| `forward ID [LOCAL:]REMOTE [-address A]` | Listens on `127.0.0.1:LOCAL` (any free port when omitted) and carries each connection to `REMOTE` inside the sandbox. |
| `expose ID PORT [-ttl 1h]` | Publishes the port at a URL for you alone, when the operator turned exposing on. |
| `logs ID [-after SEQ] [-limit N]` | The sandbox's recorded events. |
| `stop ID [-grace S]`, `start ID`, `delete ID [-y]` | Lifecycle verbs, each with `-if-match`. `delete` asks first and, away from a terminal, needs `-y`. |
| `snapshot ID [-name N]`, `restore ID -f FILE` | `restore` takes what `snapshot -o json` printed (`-` reads stdin), or `SNAPSHOT_ID -digest DIGEST`. |
| `template list [-l SELECTOR]`, `template get NAME`, `template create NAME [...]`, `template rm NAME [-y]` | Templates; `create` takes the same `-l`, `-set` and size flags as `create`, plus `-driver` and `-description`. |

`abhed sandbox -h` lists them; `abhed sandbox COMMAND -h` gives each
command's flags. Flags may come before or after the sandbox id; for
`exec`, everything after the command's first word belongs to the command.

**Exit status.** `0` done; `1` the call failed (the server refused it or
could not be reached); `2` the command was called wrongly; `3` not signed
in, or the sign-in may not do this (`401`, `403`, a deny rule included);
`4` no such sandbox or template; `5` a conflict (`409`, `412`, `428`: the
sandbox is in the wrong state, or changed). `exec`, `connect` and `ssh`
exit with the remote command's status once it ran, and `255` when it
started but did not end with one; leaving `connect` with the escape key is
`0`.

**Sign-in.** A token near its expiry is renewed before a call when the
profile holds a refresh token, and once more if the server answers `401`;
the new token is saved to the profiles file. Tokens are never printed. A
profile that is signed out, or whose token was refused with nothing to
renew it, is exit 3 with the `abhed login` to run.

**Retries.** Every `POST`, `PATCH` and `DELETE` carries an
`Idempotency-Key` the command makes. When the answer is lost (the
connection dropped, or `502`, `503`, `504` or `429`), the same call is sent
again, up to three more times, with the same key, so the server acts once:
a repeated create returns the first sandbox, and an `exec` whose output
stream broke gets the command's exit status without running it again.
A copy in is sent once.

**Copies.** `cp` moves a tar archive, as `:cp` does. Going in, the local
file or folder is packed under its own name, leaving out links that lead
out of it, sockets, devices, pipes and any `.abhed` folder (the count of
what was left out is printed). Coming out, the archive from the sandbox is
untrusted: every entry is checked as the server checks a copy in (no
absolute names, no `..`, no links pointing outside the folder or into
`.abhed`, no devices) and written through an `os.Root` opened on the
target folder, so nothing lands outside it; an entry in a `.abhed` folder,
in any case, stops the copy, and no existing file is overwritten. A local
or sandbox path in `.abhed` is refused before anything is sent.

**SSH.** `ssh` and `ssh-config` ask the server for the sandbox's host key
over the signed-in connection and keep it in `sandbox_known_hosts` beside
the profiles file (one line per sandbox, replaced when it changes; `0600`).
`ssh` then runs `ssh -o ProxyCommand=… -o UserKnownHostsFile=… -o
StrictHostKeyChecking=yes -o HostKeyAlias=abhed-ID -l sandbox abhed-ID
[ARGS]`: the host key is always checked, never skipped. `ssh-config`
prints the same as a block:

```
Host abhed-sbx_6f1c0a9e4b2d7c13
  HostKeyAlias abhed-sbx_6f1c0a9e4b2d7c13
  User sandbox
  ProxyCommand /usr/local/bin/abhed sandbox connect --stdio -profile prod sbx_6f1c0a9e4b2d7c13
  UserKnownHostsFile "/home/me/.abhed/sandbox_known_hosts"
  StrictHostKeyChecking yes
  CheckHostIP no
```

**Forwarding.** `forward` listens on loopback unless `-address` says
otherwise, and then warns that anyone who can reach that address reaches
the sandbox's port as you. Each connection is its own `:forward` stream,
recorded with its bytes; the command runs until Ctrl-C.

**Inside an agent.** Every `abhed sandbox` command is refused when run by
the agent (`ABHED_SANDBOX` set, or under its fence), as `abhed login` is:
a session must not reach a server's sandboxes with your sign-in.

## Lifecycle

`Provisioning → Ready`, `Ready → Stopping → Stopped → Starting → Ready`,
and `Error`, `Completed` and `Deleting` as the contract describes. Verbs
record the change and return at once; the reconciler does the driver's work
and moves the sandbox on. A driver that fails, or reports isolation weaker
than `isolation.min_tier`, puts the sandbox in `Error` with a condition
saying why; nothing falls back to something weaker, and an instance that
came up too weak is stopped. A delete the driver refuses leaves the sandbox
in `Deleting` with `DeleteFailed` and is tried again.

### Restarts and generations

Each sandbox has a `generation`: 1 at create, one more each time it starts
again or its template changes. Anything tied to a running instance carries
it and is refused once it no longer matches.

The restart policy acts on a sandbox that ended on its own: `on-failure`
starts an `Error` sandbox again, `always` also a `Completed` one, and
`never` neither. A sandbox you stopped is never restarted. The first
restart waits `restart.backoff_seconds`, each next one twice as long (at
most an hour), shown as `next_restart` and the `RestartScheduled`
condition. After `restart.max_restarts` it stays in `Error` with
`RestartScheduled` false, reason `LimitReached`. The count, `restarts`,
starts again from zero when the sandbox has run for ten minutes, or when you
start it yourself.

A server restart loses the `process` driver's instances; the
reconciler finds each `Ready` sandbox gone (`InstanceLost`) and the restart
policy brings it back as a new generation.

## Health and readiness

`GET /v1/health` is unchanged: the process is up. `GET /v1/ready` says
whether it should be sent work, and answers `503` when it should not:

```json
{"ready": true, "checks": {"serving": "ok", "secrets": "ok", "event_store": "ok", "control_plane": "ok"}}
```

`serving` fails while the server drains, `event_store` when Postgres does
not answer, and `control_plane` (present when it is on) when the reconciler
is not running or its store does not answer. Both routes need no sign-in;
`/v1/ready` names its checks and nothing else.
