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
  `UPDATE` and `DELETE` on `sandboxes` and `sandbox_idempotency`, and only
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

These last four use what the sandbox's driver can do. A driver that cannot
answers `501` with code `unsupported`, before anything is recorded: the
`process` driver snapshots and restores but does not copy, and the microVM
driver answers for itself. `:attach`, `:ssh`, `:forward` and `:exec` with
`"stdin": true` answer `501` in this version.

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
