# Control plane and drivers: the contract

Status: 2026-10-09, contract version 1, draft for 1.2.8. The Go packages
`driver`, `driver/plugin`, `driver/process` and `controlplane` implement the
parts marked **Implemented**; everything else on this page is the contract the
REST, CLI, per-session and container-driver work builds against.

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
  "created_at": "2026-10-09T09:58:11Z",
  "updated_at": "2026-10-09T10:00:00Z"
}
```

| Field | Set by | Meaning |
|---|---|---|
| `id` | server | `sbx_` plus 16 lowercase hex characters. Never reused. Never contains `:` (the REST surface splits on it). |
| `generation` | server | The incarnation counter. Starts at 1 on create and goes up by one each time the instance is started again (Stopped, Completed or Error → Starting), restored, or its `template` changes. Anything bound to a running instance (a sandbox token, an attach session, a forward) carries the generation and is refused once it no longer matches. |
| `resource_version` | store | Goes up on every write to the object, by a store-wide counter. It is the `ETag`, and `If-Match` compares against it. |
| `template` | client | The template the sandbox was built from (P3-04). Changing it bumps `generation` and needs a restart to take effect. |
| `policy_rev` | server | The policy revision in force. Policy reload is live, so a new revision does not bump `generation`; it is recorded as a `PolicyApplied` condition and in the event record. |
| `driver` | client, default from template | The registered driver name. Immutable after create. |
| `state` | server | One of the states in §3. Clients never write it directly; they call a verb. |
| `conditions` | server | Observations, each `{type, status: True\|False\|Unknown, reason, message, observed_generation, last_transition}`. `last_transition` changes only when `status` changes. |
| `labels` | client | `key: value`, selectable with the selector syntax in §2.1. Keys under `abhed.dev/` are reserved for the server. |
| `owner` | server | The authenticated principal that created it. Authorization checks use it; clients cannot set it. |
| `tier` | driver | The isolation the driver reports it actually provides (`process`, `fence`, `container`, `vm`, `microvm`, `none`). Never the one requested. `vm` is gVisor, a user-space kernel; `microvm` is a hardware-virtualized guest with its own kernel, from the `microvm` driver only. |

Tenancy is a store concern, as for sessions today: a Postgres store is opened
for one tenant and row-level security scopes every statement; an SQLite or
memory store holds one tenant. Nothing in the object carries a tenant a client
could change.

Condition types the server sets: `DriverReady`, `PolicyApplied`,
`IsolationVerified` (the reported tier meets the policy's minimum),
`DeleteFailed`. Drivers may add their own under a `<driver>.` prefix.

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

## 4. REST surface

All under `/v1`, JSON, authenticated by the server's existing auth modes.
Verbs are custom methods on the resource, `POST /v1/sandboxes/{id}:verb`.
Go's `ServeMux` cannot match a wildcard and a literal in one segment, so the
handler registers `POST /v1/sandboxes/{target}` and splits `target` on its
last `:`; ids never contain one.

| Method and path | Body | Result | Notes |
|---|---|---|---|
| `POST /v1/sandboxes` | `{template, driver?, labels?}` | `201` sandbox | Starts in `Provisioning`. Idempotency key honoured. |
| `GET /v1/sandboxes?selector=&state=&limit=&after=` | — | `200 {items, next}` | Owner-filtered unless the caller is an admin. |
| `GET /v1/sandboxes/{id}` | — | `200` sandbox, `ETag` | |
| `PATCH /v1/sandboxes/{id}` | `{labels?, template?}` (JSON merge patch) | `200` sandbox | `If-Match` required. |
| `DELETE /v1/sandboxes/{id}` | — | `202` sandbox in `Deleting` | `If-Match` honoured. |
| `POST …:start` / `:stop` | `{grace_seconds?}` | `202` sandbox | `If-Match` honoured. |
| `POST …:exec` | `{argv \| command, cwd?, env?, stdin?: bool}` | stream | NDJSON frames `{stdout\|stderr\|exit}`; WebSocket when `stdin` is true. |
| `GET …:attach` | — | WebSocket | PTY; resize as a control frame. Carries `generation`. |
| `POST …:ssh` | — | `{host, port, user, known_hosts, expires_at}` | Short-lived credentials for the relay (P3-05). |
| `POST …:forward` | `{port}` | WebSocket | Byte stream to a port inside the sandbox. Loopback-only on the client side. |
| `POST …:expose` | `{port, ttl_seconds}` | `{url, expires_at}` | Off unless the policy allows it. |
| `POST …:cp` | `{direction: in\|out, path}` + tar body or tar response | `200` | Paths confined to the workspace; archive entries validated (no `..`, no absolute paths, no links out). |
| `POST …:snapshot` | `{name?}` | `201 {snapshot, generation}` | |
| `POST …:restore` | `{snapshot}` | `202` sandbox | From `Stopped`, `Completed` or `Error`; bumps `generation`. |

Every mutating call becomes an event in the record with the principal, the
sandbox id, generation and the outcome. Errors are
`{"error": {"code", "message"}}` with codes `invalid`, `not_found`,
`conflict`, `precondition_failed`, `unsupported`, `unavailable`, `internal`.

### 4.1 Idempotency-Key (Implemented: `controlplane.Idempotency`)

- Accepted on every `POST`, `PATCH` and `DELETE`. 1–255 printable ASCII
  characters.
- Scoped to `(principal, method, path)`: another principal's key never
  replays.
- The first request with a key claims it and stores a hash of the body.
  While it runs, a repeat gets `409` with code `conflict`. The claim is a
  5-minute lease, so a server that dies mid-request frees the key. When it ends, its
  status and body are stored and a repeat with the same body gets the same
  response with `Idempotent-Replayed: true`. A repeat with a different body
  gets `422` with code `invalid`.
- A request that fails before any side effect (validation, auth) releases
  its claim. Records expire after 24 hours.

### 4.2 If-Match and resource versions (Implemented)

- `ETag: "<resource_version>"` on every object response.
- `If-Match` is required on `PATCH` and honoured on every verb and `DELETE`.
  A mismatch is `412` with code `precondition_failed`; a malformed header is
  `400`. `If-Match: *` matches any existing object.
- Without `If-Match`, verbs still run as a guarded update against the version
  the server just read, so a lost update is impossible either way; `If-Match`
  lets a client say which state it decided on.

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
```

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

Registration: `driver.Registry` maps names (`[a-z][a-z0-9-]{0,31}`) to drivers.
`driver/builtin.Registry` builds the one a configuration describes: `process`
always, and `microvm` when the managed `sandbox.microvm_socket` names an
`abhed-microvm` socket (registered even when it cannot be reached, refusing
every call with `ErrUnavailable`, so choosing it never falls through).
`Register` runs `Check` and refuses duplicates. `app.WithDriver` (later work)
adds one to the server's registry, which is how the Team module adds the
Kubernetes driver without CE importing it.

### 5.1 The built-in `process` driver (Implemented: `driver/process`)

An adapter over the existing sandbox tiers, with no change to how commands are
confined: `Create` builds the sandbox the configuration selects
(`sandboxconfig.Build`, so Seatbelt or bubblewrap, or the fence when
`sandbox.tier` is `fence`), `Exec` runs a command through that sandbox's
`Command`, and `Attach` runs its interactive shell on a PTY. `Stop` refuses
further commands; `Start` builds the sandbox again; `Delete` closes it, which
releases the fence's cgroup. Copy, forward and snapshot are not declared.

Because it reuses today's selection, the `process` driver also yields the
container or gVisor tier where configuration and host pick one, and says so in
`Status.Tier`. When the docker and podman drivers land (P3-07), whether
`process` is narrowed to the process and fence tiers is decided there.

`ExecRequest.Env` may add variables but never replace one the sandbox set
(`PATH`, `HOME`, `ABHED_SANDBOX`, the proxy variables), and `Cwd` must be
inside the workspace.

### 5.2 The `microvm` driver (Implemented: `driver/microvm`, experimental)

Each instance is a virtual machine with its own guest kernel, reporting the
tier `microvm`; a `MinTier` of any known tier is met, and the `process`
driver answers a `MinTier` of `microvm` with `ErrUnavailable`. Backends: on
Linux, Firecracker under its jailer on KVM (chroot, a uid and gid per VM,
cgroup v2 limits, Firecracker's seccomp filters); on macOS, a separate
Virtualization.framework helper speaking the protocol in
`driver/microvm/helper.go`, not shipped, so the backend refuses until one is
installed. A backend whose requirements are missing fails `Create` with
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

## 7. Per-session sandboxes in `serve`

Today `serve` builds one sandbox at start-up and every session's commands run
in it, except under the fence, where each session already gets a fence of its
own (`app/sessionfence.go`). Under this contract (P3-03):

- A session creates a sandbox object from the server's default template, with
  labels `abhed.dev/session=<session id>` and `abhed.dev/owner=<principal>`,
  through the same code path as `POST /v1/sandboxes`.
- The session's bash tool and workbench terminal call the driver through the
  control plane (`Exec`, `Attach`) instead of holding an `exec.Cmd` builder.
- Closing the session deletes the sandbox; a server restart finds sandboxes
  whose session is gone by label and deletes them.
- The process driver keeps today's behaviour exactly, including the fence's
  per-session cgroup and its refusal to run under another tier.
- A warm pool, when added, holds `Ready` sandboxes from a template with no
  session label and claims one by an update guarded on its resource version.

## 8. Store (Implemented: `controlplane.Store`, memory)

```go
type Store interface {
    Create(ctx, Sandbox) (Sandbox, error)                   // ErrExists
    Get(ctx, id string) (Sandbox, error)                    // ErrNotFound
    List(ctx, ListOptions) ([]Sandbox, string, error)       // selector, state, owner, limit, after
    Update(ctx, Sandbox, expect int64) (Sandbox, error)     // ErrConflict on version mismatch
    Delete(ctx, id string, expect int64) error
    IdempotencyStore // Claim(rec, now), Complete, Release
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
- Backends: memory (implemented), SQLite for a single host, Postgres for
  servers. SQLite must be a pure-Go driver, so the binary stays static with no
  cgo; the dependency is a founder decision and is not added yet. Both SQL
  backends run the same conformance tests as the memory store
  (`controlplane/storetest`).

## 9. Editions

| Piece | Edition |
|---|---|
| Sandbox object, lifecycle, REST, idempotency, `If-Match`, labels | Community |
| Driver interface, out-of-process protocol, process/docker/podman/microVM drivers | Community |
| Memory, SQLite and Postgres stores | Community |
| Per-session sandboxes, warm pool, CLI | Community |
| Kubernetes driver | Team (private module, via `app.WithDriver`) |
| Workspaces, roles, quotas on sandboxes | Team |
| Admission interceptors on every mutation, HA ownership and leases | Enterprise |

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
