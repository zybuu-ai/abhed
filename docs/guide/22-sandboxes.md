# Sandboxes over the API

`abhed serve` can manage sandboxes as objects of their own: create one, run
commands in it, stop it, start it again, delete it, all over `/v1/sandboxes`.
Each sandbox is confined exactly as a session's commands are today (the
`process` driver, the default, builds it from your `sandbox` settings), and
every change to one is recorded with who asked for it. A sandbox may name
another driver the configuration registers: `microvm`, when the managed
`sandbox.microvm_socket` is set ([The microVM driver](24-microvm.md)). A
configured driver that cannot be reached is named when `abhed serve` starts,
and sandboxes asking for it fail with `unavailable` rather than running
under another.

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
| `workspace_root` | a folder in the control plane's state folder | Gives each sandbox a folder of its own, `<root>/<id>`, mode `0700`, removed when the sandbox is deleted. Must be absolute, outside the server's workspace and outside any `.abhed`, and not writable by others. Unset, the root is `~/Library/Application Support/Abhed/control-plane/sandboxes` on macOS and `$XDG_STATE_HOME/abhed/control-plane/sandboxes` (`~/.local/state/…` without it) elsewhere. |
| `shared_workspace` | `false` | Every sandbox works in the server's workspace instead, as sessions do, and sees what the others and the server's sessions write there. Set it only when that is what you want; it cannot be combined with `workspace_root`. |
| `snapshot_dir` | a folder in the control plane's state folder | Where sandbox snapshots are kept: `snapshots` beside the default `sandboxes` folder. Must be absolute and outside every workspace. |
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
| `GET /v1/sandboxes/{id}:attach?rows=&cols=&cwd=` | a WebSocket: an interactive shell, [below](#streams-attach-exec-with-stdin-forward-and-ssh) |
| `GET /v1/sandboxes/{id}:exec` | a WebSocket: one command with stdin |
| `GET /v1/sandboxes/{id}:forward?port=N` | a WebSocket: a byte stream to a port inside the sandbox |
| `POST /v1/sandboxes/{id}:ssh` | `200 {host_alias, user, host_key, fingerprint, known_hosts, relay, subprotocol, generation}` |
| `GET /v1/sandboxes/{id}:ssh` | a WebSocket: the SSH relay |

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
[TLS and command-line sign-in](26-tls-and-cli-login.md)).

Each SSH session is its own recorded call, judged as the verb it amounts
to: a shell is an `:attach` (`via: ssh` in the record; refused where
`:attach` is), a command is an `:exec` (judged by the deny rules, bounded
by `exec.timeout_seconds` and `exec.output_bytes`, and run without a
terminal even when one was asked for), and a `direct-tcpip` channel to
`127.0.0.1`, `::1` or `localhost` is a `:forward`. The connection itself
is recorded as `ssh`, with its sessions, forwards and refusals. Everything
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
mode `0600`, not a link, and outside every workspace, added directory,
sandbox folder, temp folder and cache. A configured key that cannot be read
refuses the start instead of leaving the relay off. It is a protected state
file like the TLS key. From a Kubernetes Secret, mount the one file with
`subPath` (the Secret volume's own files are links) and `defaultMode: 0400`
under a `runAsUser` that owns it, or copy it into place with an init
container. `abhed serve -check` and `abhed doctor` name the key in use and
its fingerprint.

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
