# Control plane and drivers: the contract

Status: 2026-10-09, contract version 1, draft for 1.2.8. The Go packages
`driver`, `driver/plugin`, `driver/process`, `driver/container`,
`controlplane`, `store` (the Postgres store), `store/sqlite` (the
single-host store), `server` (the REST surface) and
`internal/sessionsandbox` (per-session sandboxes) implement the parts
marked **Implemented**; everything else on this page is the contract the
CLI and per-session work builds against.

This page defines the sandbox object, its lifecycle, the `/v1/sandboxes` REST
surface, the driver interface and its out-of-process protocol, how `abhed
serve`'s per-session sandboxes map onto all of it, and which pieces belong to
which edition. Where the code and this page disagree, fix one of them in the
same change; neither is allowed to drift.

The public packages are an experimental API until 2.0: they may change in a
minor release, and each change is listed in the changelog.

---

## 1. Terms

- **Sandbox.** A named, durable object in the control plane describing one
  isolated execution environment: what it was built from, which policy it
  runs under, which driver runs it, and what state it is in.
- **Driver.** The code that turns a sandbox into something that runs:
  confined host processes, a container, a microVM, a pod. A driver owns
  *instances*; the control plane owns *objects*.
- **Instance.** A driver's runtime for one sandbox, addressed by the
  sandbox's id.
- **Store.** Where sandbox objects and idempotency records persist: memory
  (tests, the single-user CLI), SQLite (single host), Postgres (servers).

## 2. The sandbox object (Implemented: `controlplane.Sandbox`)

```json
{
  "id": "sbx_6f1c0a9e4b2d7c13",
  "generation": 3,
  "resource_version": 41,
  "template": "default",
  "policy_rev": "p-12",
  "driver": "process",
  "state": "Ready",
  "conditions": [
    {"type": "DriverReady", "status": "True", "reason": "Started",
     "message": "", "observed_generation": 3,
     "last_transition": "2026-10-09T10:00:00Z"}
  ],
  "labels": {"team": "payments", "abhed.dev/session": "s_91"},
  "owner": "alice",
  "tier": "process",
  "settings": {"restart.policy": "on-failure"},
  "restarts": 1,
  "next_restart": "2026-10-09T10:05:00Z",
  "created_at": "2026-10-09T09:58:11Z",
  "updated_at": "2026-10-09T10:00:00Z"
}
```

| Field | Set by | Meaning |
|---|---|---|
| `id` | server | `sbx_` plus 16 lowercase hex characters. Never reused. Never contains `:` (the REST surface splits on it). |
| `generation` | server | The incarnation counter. Starts at 1 on create and goes up by one each time the instance is started again (Stopped, Completed or Error → Starting), restored, or its `template` changes. Anything bound to a running instance (a sandbox token, an attach session, a forward) carries the generation and is refused once it no longer matches. |
| `resource_version` | store | Goes up on every write to the object, by a store-wide counter. It is the `ETag`, and `If-Match` compares against it. |
| `template` | client | The template the sandbox was built from (§4.4). Changing it bumps `generation` and needs a restart to take effect; what the template gave at create stays. |
| `policy_rev` | server | The policy revision in force. Policy reload is live, so a new revision does not bump `generation`; it is recorded as a `PolicyApplied` condition and in the event record. |
| `driver` | client, default from template | The registered driver name. Immutable after create. |
| `state` | server | One of the states in §3. Clients never write it directly; they call a verb. |
| `conditions` | server | Observations, each `{type, status: True\|False\|Unknown, reason, message, observed_generation, last_transition}`. `last_transition` changes only when `status` changes. |
| `labels` | client | `key: value`, selectable with the selector syntax in §2.1. Keys under `abhed.dev/` are reserved for the server. |
| `owner` | server | The authenticated principal that created it. Authorization checks use it; clients cannot set it. |
| `tier` | driver | The isolation the driver reports it actually provides (`process`, `fence`, `container`, `vm`, `microvm`, `none`). Never the one requested. `vm` is gVisor, a user-space kernel; `microvm` is a hardware-virtualized guest with its own kernel, from the `microvm` driver only. |
| `settings` | client | Per-sandbox values from the settings registry (§11). Validated against it; never looser than a ceiling. |
| `restarts` | server | Restarts the restart policy made since the sandbox last ran stably (10 minutes Ready) or a client started it. |
| `next_restart` | server | When the restart policy starts it again; absent when no restart is scheduled. |

Tenancy is a store concern, as for sessions today: a Postgres store is opened
for one tenant and row-level security scopes every statement; an SQLite or
memory store holds one tenant. Nothing in the object carries a tenant a client
could change.

Condition types the server sets: `DriverReady`, `PolicyApplied`,
`IsolationVerified` (the reported tier meets the policy's minimum; its
message names the tier and what it lacks on this host, as in
`tier process (no /proc)`),
`DeleteFailed`, `RestartScheduled` (True while a restart waits out its
backoff; False with reason `LimitReached` once the budget is spent). Drivers may add their own under a `<driver>.` prefix.

### 2.1 Labels and selectors (Implemented)

Keys: an optional DNS-subdomain prefix and `/`, then a name of 1–63 characters
from `[A-Za-z0-9._-]`, starting and ending alphanumeric. Values: up to 63 of the
same characters, may be empty. At most 64 labels per sandbox.

Selectors are comma-separated requirements, all of which must hold:
`k=v`, `k==v`, `k!=v`, `k` (exists), `!k` (does not exist),
`k in (a,b)`, `k notin (a,b)`. `k!=v` and `notin` also match a sandbox
without the key. An unparsable selector is an error, never "match all".

## 3. Lifecycle (Implemented: `controlplane.CanTransition`)

```
             create
               │
               ▼
        ┌─Provisioning──┐
        │       │       │
        │       ▼       │
        │     Ready ────┼──────────► Completed ─┐
        │    │     ▲    │                       │
        │    ▼     │    │                       │
        │ Stopping Starting ◄───────────────────┤ start / restore
        │    │     ▲    │                       │
        │    ▼     │    │                       │
        │  Stopped─┘    │                       │
        ▼               ▼                       │
      Error ◄──── (any running state) ──────────┘
        │
        ▼
     Deleting ──► (removed from the store)
```

Allowed transitions; anything not listed is refused with `409 Conflict` and
the object is left unchanged:

| From | To |
|---|---|
| Provisioning | Ready, Error, Deleting |
| Ready | Stopping, Completed, Error, Deleting |
| Stopping | Stopped, Error, Deleting |
| Stopped | Starting, Deleting |
| Starting | Ready, Error, Deleting |
| Completed | Starting, Deleting |
| Error | Starting, Deleting |
| Deleting | (none: the object is removed once the driver confirms) |

Rules:

- Only `Ready` accepts `:exec`, `:attach`, `:ssh`, `:forward`, `:expose` and
  `:cp`. Every other state refuses them with `409`; nothing queues.
- `Completed` means the workload ended on its own with success; `Error` means
  the driver or the workload failed. Both keep the instance's data until
  deleted.
- `Deleting` is terminal. A driver delete that fails leaves the object in
  `Deleting` with `DeleteFailed=True` and is retried; it never goes back to a
  state that accepts work.
- Entering `Starting` from `Stopped`, `Completed` or `Error` bumps
  `generation`.
- A transition and the write that records it are one store update guarded by
  the resource version, so two racing verbs cannot both win.

## 4. REST surface (Implemented: `server/sandboxes.go`, except the verbs marked later)

Served by `abhed serve` when `control_plane.enabled` is set. All under `/v1`, JSON, authenticated by the server's existing auth modes.
Verbs are custom methods on the resource, `POST /v1/sandboxes/{id}:verb`.
Go's `ServeMux` cannot match a wildcard and a literal in one segment, so the
handler registers `POST /v1/sandboxes/{target}` and splits `target` on its
last `:`; ids never contain one.

| Method and path | Body | Result | Notes |
|---|---|---|---|
| `POST /v1/sandboxes` | `{template, driver?, labels?}` | `201` sandbox | Starts in `Provisioning`. Idempotency key honoured. |
| `GET /v1/sandboxes?selector=&state=&limit=&after=` | — | `200 {items, next}` | Owner-filtered unless the caller is an admin. |
| `GET /v1/sandboxes/{id}` | — | `200` sandbox, `ETag` | |
| `PATCH /v1/sandboxes/{id}` | `{labels?, template?, settings?}` (JSON merge patch) | `200` sandbox | `If-Match` required; `428` without it. |
| `DELETE /v1/sandboxes/{id}` | — | `202` sandbox in `Deleting` | `If-Match` honoured. |
| `POST …:start` / `:stop` | `{grace_seconds?}` | `202` sandbox | `If-Match` honoured. |
| `POST …:exec` | `{argv \| command, cwd?, env?}` | stream | NDJSON frames `{stdout\|stderr\|exit\|error}`, byte payloads base64. Ends with `{"exit": n}` only when the command ran to an exit code, else `{"error": {code, message}}`. With `"stdin": true`, `400`: stdin is the WebSocket form. `argv` runs, is screened and is recorded as one line with each word quoted for a POSIX shell (`driver.QuoteArgv`); a word with a NUL is `400`. |
| `GET …:exec` | first message: the `POST` body | WebSocket `abhed.exec.v1` | One command with stdin (§4.3). While a deny or ask rule for `bash` or a hook is in force, a shell reading its commands from stdin has each statement judged before it is sent (a refused one ends the input, recorded as `exec.refused`, and the stream ends `forbidden`), and any other command that could read commands from stdin is `forbidden`; the same holds for an SSH exec. See the sandboxes guide. |
| `GET …:attach?rows=&cols=&cwd=` | — | WebSocket `abhed.attach.v1` | PTY; resize as a control message. Carries `generation`. |
| `POST …:ssh` | — | `{host_alias, user, host_key, fingerprint, known_hosts, relay, subprotocol, generation}` | What a client needs for the relay; no credential is minted, since the relay authenticates the caller. |
| `GET …:ssh` | — | WebSocket `abhed.ssh.v1` | The SSH relay: raw SSH in binary messages. |
| `GET …:forward?port=N` | — | WebSocket `abhed.forward.v1` | Byte stream to a port on the sandbox's loopback. |
| `POST …:expose` | `{port, ttl_seconds}` | `{url, expires_at}` | Off unless the operator enables it. Served by `server/expose` under `/v1/exposed/`, to the same principal and tenant only. |
| `POST …:cp` | `{direction: in\|out, path}` + tar body or tar response | `200` | Paths confined to the workspace; archive entries validated (no `..`, no absolute paths, no links out). |
| `POST …:snapshot` | `{name?}` | `201 {snapshot, generation}` | `snapshot` carries the content `digest`. |
| `POST …:restore` | `{snapshot}` | `202` sandbox | From `Stopped`, `Completed` or `Error`; bumps `generation`. The sandbox is `Starting` before any file is replaced, so nothing else starts it meanwhile; a restore the driver refuses puts it back in its state, keeping the new `generation`. |
| `GET …:events?after=&limit=` | — | `200 {items, next}` | The sandbox's record, oldest first, to whoever may see it; `next` is the last `seq` returned while more remain. |

`:snapshot`, `:restore`, `:cp` and `:expose` are implemented
(`server/sandbox_verbs.go`, over `controlplane.Service`'s `Snapshot`,
`Restore`, `CopyIn`, `CopyOut` and `UseDriver`); each answers `501` with
code `unsupported`, before anything is recorded, when the sandbox's driver
lacks the capability, and `:expose` also while the operator has not turned
exposing on. `:cp` takes `direction` and `path` in the query when the body
is the tar to copy in. `:attach`, `:exec` with stdin, `:forward` and `:ssh`
are WebSockets (§4.3). The seam is `sandboxVerbs` in
`server/sandboxes.go`. All check the caller may see the sandbox first.

Every `:exec`, in each of its forms, and every shell is judged by the
server's deny rules and extension hooks (the policy the workbench terminal
uses) after its `requested` event and before the guard: a refusal is the
`failed` outcome with code `forbidden` (`403`), and the driver is never
asked. Only `deny` refuses; an `ask` does not ask a person who runs a
command themselves.

Every mutating call becomes an event in the record with the principal, the
sandbox id, generation and the outcome: one `requested` event before it acts
(a request that cannot be recorded is refused with `503` and changes
nothing) and one `ok` or `failed` after. The reconciler records its
transitions and restarts as `system:reconciler`. An `:exec`'s command line is
redacted with the secrets store before it is recorded. Errors are
`{"error": {"code", "message"}}` with codes `invalid`, `not_found`,
`conflict`, `precondition_failed`, `unsupported`, `unavailable`, `forbidden`,
`approval_required`, `quota_exceeded`, `internal`. `forbidden` (`403`) is a
request acting in a tenant other than the one the server's store was opened
for, one a guard (§12) refuses, or a call an admission interceptor refused
(§13); `approval_required` and `quota_exceeded` are `403` from a guard, the
first with the approval to ask for in the `Abhed-Approval` response header.
A request sent again with `Abhed-Approval: <id>` names that approval to the
guard. A rate limit spent is `429` with
`Retry-After` and code `unavailable`. A store or driver fault is `503` or
`500`, never success.

Output of an `:exec` is bounded in total, not only per frame: past the
sandbox's `exec.output_bytes` the command is cancelled, exactly that many
bytes have been sent, and the stream ends with an error frame of code
`invalid`. `driver.OutputLimit` is the shared budget any streaming verb uses.

### 4.1 Idempotency-Key (Implemented: `controlplane.Idempotency`)

- Accepted on every `POST`, `PATCH` and `DELETE`. 1–255 printable ASCII
  characters.
- Scoped to `(principal, method, path)`: another principal's key never
  replays.
- The first request with a key claims it and stores a hash of the body.
  While it runs, a repeat gets `409` with code `conflict`. The claim is a
  5-minute lease the running request renews, so a long `:exec` keeps its key
  while a server that dies mid-request frees it. Only the request holding the
  claim can complete or release it. When it ends, its
  status and body are stored and a repeat with the same body gets the same
  response with `Idempotent-Replayed: true`. A repeat with a different body
  gets `422` with code `invalid`.
- A request that fails before any side effect (validation, auth) releases
  its claim. Records expire after 24 hours.
- An `:exec` is not run again on a repeat: the repeat gets the stream's last
  frame (`{"exit": n}` or `{"error": …}`) as one NDJSON line.

### 4.2 If-Match and resource versions (Implemented)

- `ETag: "<resource_version>"` on every object response.
- `If-Match` is required on `PATCH` and honoured on every verb and `DELETE`.
  A mismatch is `412` with code `precondition_failed`; a malformed header is
  `400`. `If-Match: *` matches any existing object.
- Without `If-Match`, verbs still run as a guarded update against the version
  the server just read, so a lost update is impossible either way; `If-Match`
  lets a client say which state it decided on.

### 4.3 Streams (Implemented: package `wsconn`, `server/sandbox_streams.go`, `server/sshrelay`)

The streaming verbs are WebSockets (RFC 6455) served by `wsconn`, Abhed's
own small implementation (no extensions; frames checked for masking,
reserved bits, opcodes, control-frame rules and minimal lengths; text
checked to be UTF-8; frames and messages at most 1 MiB, closed with 1009
past it; the close handshake and ping/pong). It is a public package so the
Kubernetes driver can share it. For each stream, in order:

1. Authentication, by the server's middleware, as for every route.
2. Origin: a request carrying an `Origin` that is not this host or one in
   `server.allowed_origins` is `403` (the same-origin layer passes every
   `GET`, so the upgrade checks it itself).
3. The caller must see the sandbox (`404` otherwise; another tenant `403`).
4. The handshake: `GET`, HTTP/1.1, `Sec-WebSocket-Version: 13`, a 16-byte
   key, and the verb's subprotocol offered (`426` without an upgrade, `400`
   otherwise).
5. The `exec_per_minute` rate limit (`429`) and at most 16 open streams per
   caller, 512 per server (`429`).
6. The verb as `Service.Stream` (or `Service.Exec`): capability, state,
   ownership, the `requested` event, the screen (§4), the guard and the
   interceptor as an `exec` whose command names the verb (`:attach rows=…`,
   `:forward port=…`, `:ssh`). Only then is the connection taken over, so
   every refusal so far is an ordinary HTTP answer. `:exec` reads its
   request from the first message after the upgrade, and refuses in-band.
7. While open: the caller is authorised again at the stream interval (as
   session streams are) and the sandbox's generation and state are checked;
   a driver implementing `driver.Lifetime` ends it at once on stop. It also
   ends after 60 s without a frame from the client (the server pings every
   20 s) and after 12 hours.

Control messages are JSON text: `started {generation}` first; `exit
{code}` or `error {error: {code, message}}` last; from the client
`resize {rows, cols}` (attach) and `eof` (exec stdin, forward
half-close). Exec output is binary led by `1` (stdout) or `2` (stderr).
The outcome event carries `exit_code` (attach, exec), `bytes_in` and
`bytes_out` (attach, forward), `duration_ms` and, for attach, the last
8 KiB of output, scrubbed of withheld lines and redacted.

**Attach** follows the workbench's interactive terminal where the terminal
cannot be asked (`internal/termline`): each entered line is judged by
`EvaluateLine` at its Enter and recorded as `attach.input` (text withheld
when not echoed); a refused line is `attach.refused` and never reaches the
shell. Where `sandbox.terminal` is `lines`, or the managed configuration
sets deny rules or extensions that screen `bash`, attach is `403`.

**SSH** (`server/sshrelay`, `golang.org/x/crypto/ssh`): the server's SSH
server has no listener; the relay hands it one authenticated connection,
so SSH accepts the `none` method. A `shell` is a `Service.Stream` attach
(`via=ssh`), an `exec` is `Service.Exec` (no PTY; bounded by what is left
of `ssh.max_session_seconds` through `ExecIO.Timeout` instead of
`exec.timeout_seconds`, output still capped per channel), a `direct-tcpip` channel
to `127.0.0.1`, `::1` or `localhost` is a forward; global requests, other
channel types, `env`, signals and subsystems are refused. SFTP is not
served: drivers move files as tar (`:cp`), and SFTP over that would copy
whole trees to list them. The host key is Ed25519, per server, in the
control plane's state folder (`0600`, refused if readable by others or a
link, and a state path no tool may read), or the file
`control_plane.ssh_host_key` names, which replicas share; a configured key
that cannot be read refuses the start. `app`'s `abhed sandbox connect
--stdio ID` is the `ProxyCommand` end.

In HA (§13) a `SandboxRouter` must carry the upgrade to the owning replica,
and the replicas share one host key through `control_plane.ssh_host_key`.

### 4.4 Templates (Implemented: `controlplane.Template`, `server/sandbox_templates.go`)

A template is a named set of defaults: `{name, description, driver, labels,
settings}` with `owner`, `resource_version` and `created_at` set by the
server. Names are 1–63 characters from `[A-Za-z0-9._-]`, starting with a
letter or digit.

| Method and path | Body | Result | Notes |
|---|---|---|---|
| `POST /v1/sandbox-templates` | `{name, description?, driver?, labels?, settings?}` | `201` template | Administrators of the whole tenant only (any caller where `auth.mode` is `none`). Idempotency key honoured. |
| `GET /v1/sandbox-templates?selector=&limit=&after=` | — | `200 {items, next}` | Every caller of the tenant. |
| `GET /v1/sandbox-templates/{name}` | — | `200` template, `ETag` | |
| `DELETE /v1/sandbox-templates/{name}` | — | `200` | Administrators only; `If-Match` honoured. |

A create naming a template the store holds is filled from it before the
guard and interceptor see it: the template's driver is the sandbox's (a
create naming another is `400`), its labels are given to the sandbox under
the create's own, and its settings are the sandbox's unless the create
tightens one; a looser one is `400` (`Settings.NoLooser`, per key the
template sets). Nothing is applied again later. A name with no template
object is kept as a plain name. Template writes are recorded as
`template.create` and `template.delete` with no sandbox id. The `abhed
sandbox` command line (P3-04) is built on §4 and this section.

## 5. The driver interface (Implemented: package `driver`)

A driver is a Go value satisfying `driver.Driver`, in process or behind the
out-of-process protocol (§6). The required methods are the lifecycle every
driver can do; everything else is an optional interface the driver also
implements and declares in its capabilities.

```go
type Driver interface {
    Info() Info                                   // name, version, capabilities
    Create(ctx, Spec) (Status, error)
    Start(ctx, id string) (Status, error)
    Stop(ctx, id string, grace time.Duration) (Status, error)
    Delete(ctx, id string) error
    Status(ctx, id string) (Status, error)
    Exec(ctx, id string, ExecRequest) (ExecResult, error)
}

// Optional, each matched by a capability flag:
type Attacher   interface { Attach(ctx, id, AttachRequest) (ExecResult, error) }       // CapAttach
type Copier     interface { CopyIn(ctx, id, dst string, tar io.Reader) error           // CapCopy
                            CopyOut(ctx, id, src string, tar io.Writer) error }
type Forwarder  interface { Forward(ctx, id string, port int) (io.ReadWriteCloser, error) } // CapForward
type Snapshotter interface { Snapshot(ctx, id, name string) (SnapshotRef, error)       // CapSnapshot
                             Restore(ctx, id string, ref SnapshotRef) error }
type Commander  interface { Command(ctx, id, CommandRequest) (*exec.Cmd, error)      // CapCommand
                            ForwardEnv(cmd *exec.Cmd, names []string) }
```

`Command` hands the caller the process for a command or an interactive
shell in an instance, to start, wait on and kill itself: a session's bash
tool and terminal, whose background, detach and kill run on a process
handle. Only in-process drivers can (`process`, `docker`, `podman`); the
out-of-process protocol never offers it, and a driver claiming it over the
socket fails `Check`. The process ends with the caller's context or when
the instance stops; its exit status is the command's, or the engine's own
failure where an engine runs it. `ForwardEnv` passes variables already in
the process's environment into it by name, never as an argument.

Rules every driver keeps:

- **Capabilities are honest.** `driver.Check` refuses at registration a
  driver that declares a capability without implementing its interface. The
  helpers `driver.Attach`, `driver.CopyIn` and the rest return
  `ErrUnsupported` when the capability is absent; the server maps that to
  `501` with code `unsupported`.
- **Status reports the tier in force.** `Status.Tier` is what the instance
  actually provides. `Spec.MinTier` is the floor: a driver that cannot meet it
  fails `Create` with `ErrUnavailable` rather than build something weaker, and
  the control plane checks the reported tier again (`IsolationVerified`).
- **Errors fail the operation.** A driver error never turns into success, a
  default, or a weaker instance. `Exec` returns an exit code only when the
  process ran; a driver that cannot tell returns an error.
- **Idempotent lifecycle.** `Create` of an id that exists returns
  `ErrExists`; `Delete` of an id that does not exist returns `ErrNotFound`,
  which the control plane treats as already deleted.
- **No policy widening.** A driver receives the policy the control plane
  computed and may only narrow it.

Snapshots, forwards and lifetimes (Implemented: `driver/snapshot`,
`driver/forward`, `server/expose`):

- `SnapshotRef.Digest` is the archive's `sha256:<hex>`; a restore refuses
  content that does not match it, a ref of another sandbox, and any entry a
  copy would refuse or that lies in `.abhed`. `Restore` needs a stopped
  instance; the control plane moves the object to `Starting`
  (`Sandbox.Restore`), which bumps `generation`. A driver bumps its own
  count on every start from stopped, so its records agree.
- `Forward` reaches a port only through the driver's channel into the
  sandbox, never by widening its network. Forwards close when the instance
  stops; a driver implementing `driver.Lifetime` says when, so host
  listeners and exposures close with it.
- Events `sandbox.snapshot`, `sandbox.restore`, `sandbox.forward`,
  `sandbox.forward_listen` and `sandbox.expose` go to the record with the
  sandbox id and generation. Details in
  `docs/guide/25-snapshots-and-forwarding.md`.

Registration: `driver.Registry` maps names (`[a-z][a-z0-9-]{0,31}`) to drivers.
`driver/builtin.Registry` builds the one a configuration describes: `process`
always; `docker` and `podman` when `control_plane.docker` or
`control_plane.podman` is enabled (their binary, extra images, mounts,
mount roots, GPUs, CPU bound and seccomp profile are managed-only keys; an
engine that does not answer leaves the driver registered and refusing each
sandbox with `ErrUnavailable` until it does); and `microvm` when the managed
`sandbox.microvm_socket` names an `abhed-microvm` socket (registered even
when it cannot be reached, refusing every call with `ErrUnavailable`, so
choosing it never falls through).
`abhed serve`'s control plane builds its registry this way, and says at
startup which configured driver cannot be reached. `Register` runs `Check` and refuses duplicates. `app.WithDriver` (later work)
adds one to the server's registry, which is how the Team module adds the
Kubernetes driver without CE importing it.

### 5.1 The built-in `process` driver (Implemented: `driver/process`)

An adapter over the existing sandbox tiers, with no change to how commands are
confined: `Create` builds the sandbox the configuration selects
(`sandboxconfig.Build`, so Seatbelt or bubblewrap, or the fence when
`sandbox.tier` is `fence`), `Exec` runs a command through that sandbox's
`Command`, `Attach` runs its interactive shell on a PTY, and `Command` (the
driver capability) hands either out for the caller to run. `Stop` refuses
further commands; `Start` builds the sandbox again; `Delete` closes it, which
releases the fence's cgroup. `Forward` runs a relay inside the sandbox to
its own loopback; snapshot is declared when `Options.SnapshotDir` is set
(workspace only). Copy is not declared.

Because it reuses today's selection, the `process` driver also yields the
container or gVisor tier where configuration and host pick one, and says so in
`Status.Tier`. With the docker and podman drivers in place, whether `process`
is narrowed to the process and fence tiers is still open; it is unchanged
for now.

### 5.2 The `docker` and `podman` drivers (Implemented: `driver/container`)

One long-lived container per sandbox, driven through the engine's CLI. The
policy is built from the configuration as for `process`; `Spec.Config`
(`image`, `mounts`, `gpus`, `memory_mb`, `pids`, `cpus`) may only pick from
what the operator's `Options` list and lower what they bound, and any other
key is refused. `Create` verifies from inside the container that seccomp,
`no_new_privs`, an empty capability set, the uid, the network and a
read-only root are in force, and removes a container that is weaker; the
reported tier is always `container`, so `MinTier: vm` and `sandbox.tier:
fence` fail with `ErrUnavailable`. Under the network allowlist the proxy
stays on the host and a relay inside the container's own network namespace
reaches it over a bound Unix socket; where the engine is not local to a
Linux host this fails with `ErrUnavailable`. `Start` builds a fresh
container; `Stop` stops it with the grace given. Capabilities: `attach`,
`copy`, `forward` (a relay through `exec`, so the container never gets a
network), `command` (the same exec handed to the caller), and `snapshot`
with `Options.SnapshotDir` (the workspace archived and the container
committed). Details and every admission rule are in
`docs/guide/23-container-drivers.md`.

`ExecRequest.Env` may add variables but never replace one the sandbox set
(`PATH`, `HOME`, `ABHED_SANDBOX`, the proxy variables), and `Cwd` must be
inside the workspace.

The control plane fills `Spec.WriteProtected` from `controlplane.Config`
for every driver. For a driver its `controlplane.Egress` names (in `abhed
serve`, `microvm` under the allowlist) it opens a proxy per sandbox before
`Create` and sets `Spec.Egress`, refusing to build the sandbox when the
proxy cannot start; each `:exec` gets its own credential in
`ExecRequest.Env`, ended when the command ends, and a client's own proxy
variables are refused. The proxy closes when the sandbox is deleted.

### 5.3 The `microvm` driver (Implemented: `driver/microvm`, experimental)

Each instance is a virtual machine with its own guest kernel, reporting the
tier `microvm`; a `MinTier` of any known tier is met, and the `process`
driver answers a `MinTier` of `microvm` with `ErrUnavailable`. Backends: on
Linux, Firecracker under its jailer on KVM (chroot, a uid and gid per VM,
cgroup v2 limits, Firecracker's seccomp filters); on macOS, `abhed-vmhelper`
(`macos/abhed-vmhelper`), a separate Virtualization.framework program
speaking the protocol in `driver/microvm/helper.go`, whose code signature,
entitlement and hardened runtime the driver checks before running it, and
which it refuses while missing or wrongly signed. A backend whose requirements are missing fails `Create` with
`ErrUnavailable`.

- No network device. With `Spec.Egress` set (the session's egress proxy
  socket and port), the guest's loopback port is carried over vsock to the
  proxy, which applies the allowlist and per-call credentials as on the
  process tier; with it nil there is no path out. The host reaches the
  guest only over vsock, where
  `cmd/abhed-guest` (the guest's init) runs commands as an unprivileged user.
  Its protocol is `driver/microvm/guest`: one call per connection, JSON
  Lines, an exit code only when the guest sent one.
- `Spec.Workspace` is copied into an ext4 image at `Create` and mounted at
  `/workspace`. After each command and before a stop, the guest's changes
  are copied back, never onto `.abhed`, git's configuration and hooks, or
  `Spec.WriteProtected`. `Spec.Config` accepts only `vcpus`
  and `memory_mib`, within the operator's maxima.
- `Stop` lets commands finish within the grace, asks the guest to power off,
  and kills the VM's cgroup after it. `Start` boots from a fresh chroot; the
  workspace image survives until `Delete`.
- Declares `attach` only.
- `Recover`, run once when the driver process starts, takes the state
  folder's lock and ends the VMs a killed run of the same state folder
  left, found by the marker beside each chroot.
- The jailer needs root, so on Linux the driver runs as its own root process
  (`cmd/abhed-microvm`) behind the out-of-process protocol, with its socket
  owned by the server's user, and builds workspace images as that user.

## 6. Out-of-process drivers (Implemented: package `driver/plugin`)

An out-of-process driver is a separate binary the operator installs, listening
on a Unix socket. The server connects as a client; `plugin.Client` satisfies
`driver.Driver`, so the rest of the control plane cannot tell the difference.

**Transport.** A Unix stream socket, mode `0600`, in a directory only the
server's user can reach. One connection per call, so a call's streams never
interleave with another's and a hung call cannot block the rest.

**Framing.** JSON Lines: one JSON object per line, UTF-8, at most 1 MiB per
line. Byte payloads are base64 in `data`, at most 32 KiB decoded per frame.

**Handshake.** The first frame each way:

```
client → {"type":"hello","versions":[1]}
driver → {"type":"hello","version":1,"info":{"name":"docker","version":"0.1.0","capabilities":["exec","copy"]}}
```

The driver picks the highest version both list. If there is none it sends
`{"type":"error","error":{"code":"version","message":…}}` and closes. The client
refuses a reply naming a version it did not offer, and a reply whose `info.name`
differs from the name it was registered under.

**A call.**

```
client → {"type":"call","method":"create","params":{"spec":{…Spec…}}}
driver → {"type":"result","result":{…Status…}}        or
driver → {"type":"error","error":{"code":"unavailable","message":"…"}}
```

Methods: `create`, `start`, `stop`, `delete`, `status`, `exec`, `attach`,
`copy_in`, `copy_out`, `forward`, `snapshot`, `restore`. `params` is one
object with the fields a method uses: `id`, `spec`, `grace_ms`, `exec`,
`attach`, `path`, `port`, `name`, `ref`.

**Streams.** After `call` for `exec` and `attach` the client sends `stdin`
frames, then `stdin_eof`; `attach` may also send `resize` `{rows, cols}`. The
driver sends `stdout` and `stderr` frames and ends with `result` carrying the
exit code. `copy_in` sends `data` frames then `eof`; `copy_out` receives them.
`forward` exchanges `data` frames both ways after a `result` that accepts it.

**Fail closed.**

- Error codes: `unsupported`, `not_found`, `exists`, `conflict`, `invalid`,
  `unavailable`, `version`, `internal`. An unknown code is `internal`.
- A connection that closes, a malformed frame, a frame larger than the limit,
  or a stream that ends without `result` or `error` is an error. An `exec`
  never reports an exit code it did not receive.
- A call the driver did not declare in its capabilities is refused by the
  client before it is sent.
- Every call has the caller's deadline; on cancel the client closes the
  connection and the driver must kill what the call started.

## 7. Per-session sandboxes in `serve` (Implemented: `internal/sessionsandbox`)

Unless `control_plane.per_session` is on, `serve` builds one sandbox at
start-up and every session's commands run in it, except under the fence,
where each session gets a fence of its own (`app/sessionfence.go`). With it
on (it needs `control_plane.enabled` and refuses `shared_workspace`):

- A session is bound to a sandbox when it is built, new or continued. Scope
  `session` names it by the label `abhed.dev/session=<session id>`, scope
  `user` by `abhed.dev/session-user=<digest of tenant and user>`. The one
  its label names is used, started again (`Starting`, a new generation)
  when it is `Stopped`, `Error` or `Completed`; else one is claimed from
  the warm pool; else one is created from `control_plane.sessions.template`
  through `Service.Create`, as `POST /v1/sandboxes` does. Every session
  sandbox, pooled or not, is owned by `system:sessions`, so ownership never
  moves; the session's user acts on it as a caller scoped to its label.
- The session waits until the sandbox is `Ready` (`ready_seconds`) and is
  refused, never moved to the shared sandbox, when it is not, when the
  reported tier is `none`, or when the session's tenant is not the store's.
  A sandbox made for a refused bind is deleted.
- The session's workspace is the sandbox's folder: the file tools, the
  workbench's views, saves, uploads and downloads, and path rules resolve
  there. The bash tool, background commands and the workbench terminal get
  their process from `Service.Command` (driver capability `command`, §5),
  which records an `exec` with `detail.via=session` and puts it to the guard
  and interceptor as an exec; the bash tool's timeout and output handling
  apply instead of `exec.timeout_seconds` and `exec.output_bytes`. The
  session's record gets `sandbox.bound` before the first command this
  process runs in it; a record that cannot be written runs nothing.
- Deleting a session deletes a `session`-scoped sandbox and its folder.
  Stopping the server applies `control_plane.sessions.retention`: `stop`
  (default), `delete` or `keep`. A server that starts deletes the
  `session`-scoped sandboxes made before it whose session its store says is
  gone; one it cannot judge is left.
- The process driver keeps today's confinement, including the fence's own
  cgroup per sandbox, and with a `workspace_root` hides that root from each
  instance's commands but for the instance's own folder
  (`sandbox.Policy.Hidden`).
- A warm pool (`warm_pool`, at most 16) holds `Ready` sandboxes from the
  template, owned by `system:sessions` and labelled `abhed.dev/pool=<template>`.
  A claim is one update guarded on the member's resource version that
  removes the pool label, adds the session's, and bumps `generation`; it is
  recorded as `claim` by the session's user and put to the guard and
  interceptor as an update. A claimed sandbox never returns to the pool.
  The pool is refilled after each claim and every reconcile interval,
  replaces members in `Error`, `Completed` or `Stopped`, and is drained when
  the server stops. It lives in the store, which holds one tenant.
- An edition's `SandboxCaller` is asked for each session's user, with a
  `POST /v1/sandboxes` request carrying the session's tenant and user and
  no headers. Its scope (reserved labels, such as a workspace's) is stamped
  on the session's sandbox, so the guard admits and counts it there; it
  confines the user's calls on it; and warm pools are kept per scope
  (`PoolConfig.Scope`), each made on a scope's first session, so every fill
  is admitted and counted in its scope and a claim never moves a member out
  of it. The caller passed in carries `Admin` for a server administrator,
  so an edition that leaves an administrator unscoped gives their session
  an unscoped sandbox; only the edition's refusal refuses the session. Everything the server does for a
  session's sandbox or a warm pool reaches the guard with
  `Admission.Origin` `session` (set only by the server, from the context,
  never from a request): a guard counts it against quotas but should not
  hold it for approvals, since no one would send it again. A sandbox found outside the user's
  scope now is not reattached. Under an edition's scope no unscoped pool is
  filled until an unscoped session asks for one.
- Not yet: the microvm driver cannot hand out a process, so it cannot be
  the session driver; under HA a `process` sandbox lives in the replica
  that made it.

## 8. Store (Implemented: `controlplane.Store`, memory, SQLite, Postgres)

```go
type Store interface {
    Create(ctx, Sandbox) (Sandbox, error)                   // ErrExists
    Get(ctx, id string) (Sandbox, error)                    // ErrNotFound
    List(ctx, ListOptions) ([]Sandbox, string, error)       // selector, state, owner, limit, after
    Update(ctx, Sandbox, expect int64) (Sandbox, error)     // ErrConflict on version mismatch
    Delete(ctx, id string, expect int64) error
    IdempotencyStore // Claim(rec, now), Complete, Release
}

type TemplateStore interface { // memory, SQLite and Postgres implement it beside Store
    CreateTemplate(ctx, Template) (Template, error)          // ErrExists
    GetTemplate(ctx, name string) (Template, error)          // ErrNotFound
    ListTemplates(ctx, TemplateListOptions) ([]Template, string, error)
    DeleteTemplate(ctx, name string, expect int64) error     // ErrVersionMismatch
}

func Mutate(ctx, Store, id, ifMatch string, fn func(*Sandbox) error) (Sandbox, error)
```

`Mutate` is how every verb writes: read, check `If-Match`, apply `fn` to a
copy, update guarded by the version read. Losing a race is `412` when the
client sent `If-Match` and a retry when it did not.

- The store assigns `resource_version` from one counter per store, so a list
  can be resumed and, later, watched.
- `Update` and `Delete` succeed only when the stored version equals `expect`.
- Idempotency claims are atomic: two concurrent claims of one key, one wins.
- `store.Sandboxes.WithFence(f)` runs `f` inside the transaction of every
  `Create`, `Update` and `Delete`, before it commits; an error from it
  leaves the write undone. It is how HA holds writes to the replica whose
  lease is current (§13).
- Backends: memory (implemented, for tests), SQLite for a single host
  (implemented: `store/sqlite`, the pure-Go `modernc.org/sqlite`, so the
  binary stays static with no cgo; the same three tables, migrated by the
  server itself and recorded in its own `schema_version`; WAL, full sync,
  one writer connection whose every write is a `BEGIN IMMEDIATE`
  transaction; triggers refuse `UPDATE`, `DELETE` and `INSERT OR REPLACE`
  on `sandbox_audit`) and Postgres for servers (implemented:
  `store.Postgres.Sandboxes`, tables `sandboxes`, `sandbox_idempotency`,
  `sandbox_templates` and the append-only `sandbox_audit`, schema version 7,
  applied by `abhed migrate`). Templates take their `resource_version` from
  the same counter as sandboxes; a store that does not implement
  `TemplateStore` answers the template routes with `501`. `abhed serve` uses Postgres when `storage.driver` is
  `postgres` and SQLite otherwise, never memory. Both SQL backends run the
  same conformance tests as the memory store (`controlplane/storetest`).
- The SQLite file is the server's state: `0600` in a `0700` folder,
  refused when it is a link, has a second name, belongs to another user or
  is open to others, and refused inside a workspace, an added directory,
  `control_plane.workspace_root`, a temp folder or a cache. The agent and
  every sandbox are denied it and its `-wal` and `-shm` files.

## 9. Editions

| Piece | Edition |
|---|---|
| Sandbox object, lifecycle, REST, idempotency, `If-Match`, labels | Community |
| Driver interface, out-of-process protocol, process/docker/podman/microVM drivers | Community |
| Memory, SQLite and Postgres stores | Community |
| Per-session sandboxes, warm pool, templates, `abhed sandbox` CLI | Community |
| Kubernetes driver | Team (private module, via `app.WithDriver`) |
| Workspaces, roles, quotas, four-eyes approvals on sandboxes (via §12) | Team |
| Admission interceptors on every mutation, HA ownership and leases | Enterprise |

The Community module carries only the seams these use (§12, §13); with none of
them set, which is every Community build, nothing behaves differently. The
configuration keys `control_plane.ha`, `control_plane.interceptors` and
`control_plane.kubernetes` (the driver's options as one JSON object, managed
only) are in every edition's schema, and a Community binary refuses to start with them set,
naming the key and the edition, rather than run without them.

The control-plane and driver packages are public, not `internal/`, because the
Team and Enterprise module can only use public packages: the Kubernetes driver
implements `driver.Driver`, interceptors see `controlplane.Sandbox`, and HA
wraps `controlplane.Store`. The Community module never imports the private one.

## 10. Security rules

- A driver that is unavailable, or reports a weaker tier than the policy's
  minimum, fails the operation; nothing falls back.
- The driver socket is reachable only by the server's user; an out-of-process
  driver gets no credentials beyond what its Spec carries.
- Driver output is untrusted: frames are size-limited, and exec output is
  treated as tool output.
- Sandbox objects never hold secrets; credentials reach an instance through
  the credential path, by placeholder.
- The agent inside a sandbox cannot reach the control plane's state, the
  store, or a driver's socket.

## 11. Settings, restart policy and readiness (Implemented: `controlplane`)

**Settings registry** (`controlplane.Registry`). Each setting has a scope:
*global* (server configuration only: `rate.mutations_per_minute`,
`rate.exec_per_minute`, `reconcile.interval_seconds`) or *sandbox* (a
configured default a sandbox may override in `settings`:
`exec.timeout_seconds`, `exec.output_bytes`, `restart.policy`,
`restart.max_restarts`, `restart.backoff_seconds`, `isolation.min_tier`,
`resources.cpus`, `resources.memory_mb`, `ssh.idle_timeout_seconds`,
`ssh.max_session_seconds`). A size in force reaches the driver
as `Spec.Config` keys `cpus` and `memory_mb`; unset passes nothing. A driver
that cannot hold a sandbox to a size refuses the Spec (`process` does).
Every setting orders its values from loosest to tightest. A value the
managed configuration sets is a **ceiling**: a sandbox may set a tighter
value and is refused (`400`) for a looser one, and a ceiling tightened
later holds existing sandboxes when their settings are read. `isolation.min_tier`
is always a floor from `sandbox.min_tier`, managed or not.

**Restart policy and generations.** `never`, `on-failure` (restarts
`Error`) or `always` (restarts `Error` and `Completed`). A sandbox a client
stopped is never restarted. The reconciler schedules restart n+1 at
`restart.backoff_seconds × 2ⁿ` (at most an hour), then moves the sandbox to
`Starting`, which bumps `generation`. After `restart.max_restarts` it stays
in `Error`. A client's `:start` resets the count; so do ten minutes Ready.

**Reconciler** (`Service.Run`). Every `reconcile.interval_seconds`, and at
once when a verb asks, each sandbox not `Stopped` gets one pass, at most one
pass per sandbox at a time: `Provisioning` and `Starting` call `Create` or
`Start` (an instance the driver lost is created again), `Stopping` calls
`Stop`, `Deleting` calls `Delete` then removes the object, `Ready` compares
`Status` with the object, and `Error`/`Completed` apply the restart policy.
Every write is guarded on the state and generation the pass started from.
It ends with its context and waits for the passes it started.

**Readiness.** `GET /v1/ready` is `200` when the server is not draining,
its secrets store reads, its event store answers and, with the control
plane on, the reconciler runs and the store answers; else `503`. It names
its checks with `ok` or `failed` and nothing more. `/v1/health` keeps its
shape. `abhed serve -check` runs the start-up checks without serving.


## 12. Extension points for workspaces (Implemented)

The Team edition adds workspaces, roles, quotas and four-eyes approvals
through these seams. Each is a no-op when unset, so the Community server
behaves as the sections above describe.

- **Caller scope.** `Caller.Scope` is a set of reserved labels
  (`abhed.dev/…`, each with a value). A scoped caller sees, changes and runs
  only sandboxes carrying all of them, an admin included, and every sandbox
  it creates carries them. Labels under the reserved prefix cannot be set or
  patched by a client, so a sandbox never leaves its scope. A scope that is
  not reserved labels with values is refused (`forbidden`).
- **SandboxCaller.** `server.Options.SandboxCaller(r, caller)` refines the
  caller the server worked out (principal, admin) before any sandbox route
  acts: an edition sets the scope and what it may administer. An error
  refuses the request.
- **Guard.** `Service.SetGuard(g)`, once, before serving. `g.Admit` is
  asked about every create, update, start, stop, delete and exec after the
  `requested` record and before the store or a driver is touched; its
  refusal is recorded as the `failed` outcome with its code. It sees the
  caller, the object now, what a create or update would make of it, an
  exec's command, a stop's grace, and the approval the request names. On a
  create it may tighten settings, which are checked against the registry
  again. An error that is not one of the contract's is `unavailable`: a
  guard that cannot decide refuses. The release it returns runs once the
  operation has ended, so it can hold a lock or a slot across it.
- **ApprovalGate.** `server.Options.ApprovalGate` decides who besides a
  session's owner may answer its pending approvals (`MayAnswer`) and whether
  an answer settles the request or is held as one of several it needs
  (`Answer`, told of every answer before it is recorded on the approval
  row). A held answer is `202 {"recorded": true, "settled": false}`; a
  refused one `403`; a gate that fails `503`, and the request keeps waiting.
  `Server.PendingAsks` lists the requests waiting in sessions this process
  runs, arguments redacted as the record keeps them.
- **SessionPolicy.** `server.Options.SessionPolicy(ctx, req)` gives the
  permission rules an edition adds to a session of `req.User` in
  `req.Tenant` as a layer of its own (`server.PolicyLayer`: a name, a
  revision, deny, ask and allow rules). It is asked when a session is
  started, opened at the workbench or continued. Deny and ask rules are
  added and hold in every mode; allow rules are set aside under a managed
  configuration that sets the permissions. An error, or a layer
  `PolicyLayer.Validate` refuses (rules `policy.ParseRule` cannot read, a
  name the configuration's own layers use, more than 500 rules), refuses
  the session with `503`. The layer is recorded as `policy.layer`, and the
  decisions its rules make carry `layer`. `PolicyLayer.Decide(tool, args)`
  applies a layer's deny and ask rules to one call outside a session, such
  as a guard checking an exec's command.
- **Credential sources.** `secretstore.OpenSource(name, src)` is a secrets
  store whose values a driver keeps (load whole, set and remove by name)
  instead of a file. It hands out and redacts values as a file store does;
  a source that cannot be reached hands out nothing, refuses a session, and
  withholds every payload until it answers again.

## 13. Extension points for interceptors and HA (Implemented)

The Enterprise module adds admission interceptors and multi-replica HA
through these public seams. Each is nil or empty by default, and then
changes nothing. On every call a guard (§12) is asked first and the
interceptors after it, so an interceptor sees what the guard admitted.
The verbs backed by driver capabilities are put to both as what they
amount to: `:restore` as a `start`, and `:snapshot`, `:cp` and `:expose` as
an `exec` whose command names the verb (`:snapshot <name>`, `:cp in
<path>`, `:expose port=<n> …`), so a guard or interceptor that refuses an
exec is not gone round. So are the streams (§4.3): `:attach`, `:forward`
and `:ssh` as an `exec` naming the verb, and each SSH session as the verb it
amounts to. They also check ownership as a change does.

| Seam | Where | Used for |
|---|---|---|
| `controlplane.Config.Interceptor` | `controlplane.Interceptor` | Admission of every mutating call |
| `controlplane.Config.Ownership` | `controlplane.Ownership` | Which sandboxes this replica acts on |
| the same, optionally | `controlplane.OwnershipWatcher` | Ends this replica's open streams on a sandbox it stops owning |
| `controlplane.Config.NodeID` | string | Names the replica in every event it records (`detail.node`) |
| `store.Sandboxes.WithFence` | `store.Fence` | Refuses a write, in its transaction, from a replica that lost its lease |
| `server.Options.SandboxRouter` | `server.SandboxRouter` | Relays a call to the replica that owns the sandbox |
| `app.WithControlPlane` | `app.ControlPlaneHook` | Sets the above before the service is built |

**Interceptors.** `Admit` is called for `create`, `update`, `start`, `stop`,
`delete` and `exec`, after the call's `requested` event and before anything
is written or run; `Committed` after its outcome. The request carries the
operation, the caller, the sandbox before (absent on create) and as the call
would leave it, and for an exec its command line already redacted for the
record. Rules the service enforces whatever the interceptor is:

- An error refuses the call: `ErrDenied` answers `403 forbidden`; anything
  else, including a timeout, is `503 unavailable`. Nothing is written or run.
- The object `Admit` returns may differ from the proposed one only in labels
  outside `abhed.dev/` and in settings, and every setting must be as tight or
  tighter than before (`Settings.NoLooser`), within the ceilings (§11). A
  template, state, owner, driver, tier, policy revision or reserved label
  change is refused as `403`, as is any change to an exec. The patched
  object is validated again before it is written.
- Every decision is an `admission` event with the caller's principal, the
  operation, hook, phase and decision, and `fail_open` when a hook's error
  was let through. A decision the record cannot keep refuses the call.
- Calls the reconciler makes as `system:reconciler` are not admitted: they
  carry out what an admitted call asked for.

**Ownership.** With `Ownership` set, the reconciler acts on a sandbox only
when `Owns` says this replica owns it, which takes ownership when no live
replica holds it; then the driver's `Status` decides whether the instance
survived (adopted) or was lost (`InstanceLost`). A change or exec on a
sandbox this replica does not own, or whose ownership it cannot read, is
`503` and changes nothing. Creates are owned by the replica that makes them.
Reads (`GET` of an object or a list) are served by any replica from the
shared store; every other call on a sandbox goes through `SandboxRouter`
first, after the caller is authenticated and its tenant checked.

An `Ownership` that also implements `OwnershipWatcher` is asked, as each
stream opens (`:attach`, `:exec` with stdin, `:forward`, `:ssh`), for a
channel closed once this replica no longer owns the sandbox
(`Watch(ctx, id)`, watched for as long as the stream lasts; nil when it
cannot be lost). When it closes the stream ends as the other ends do: the
client gets `{"type": "error"}` with code `unavailable`, and the stream's
record, and that of an exec it ran, is `failed` with code `unavailable` and
`detail.reason` `ownership lost` (`controlplane.ErrOwnershipLost`). Without
it, a stream runs on until one of its own bounds ends it.
