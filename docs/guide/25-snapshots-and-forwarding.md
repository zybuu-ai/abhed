# Snapshots, forwarding and exposed services

The `process`, `docker`, `podman` and `microvm` drivers can snapshot a sandbox and put
it back, forward a port inside a sandbox to the host, and the server can
publish a sandbox's service on an authenticated path. These are driver
capabilities in the Go packages `driver/snapshot`, `driver/forward` and
`server/expose`, part of
[the control-plane contract](../architecture/control-plane-contract.md) and
an experimental API until 2.0. `abhed serve`'s control plane uses them for
`/v1/sandboxes/{id}:snapshot`, `:restore`, `:expose` and `:forward`, a
WebSocket to a port inside the sandbox ([Sandboxes over the
API](22-sandboxes.md#streams-attach-exec-with-stdin-forward-and-ssh)).

Every one of them writes an event to the record, with the sandbox's id and
generation. A driver built without a record logs a warning instead.

| Event | When | Payload besides `sandbox_id` and `generation` |
|---|---|---|
| `sandbox.snapshot` | a snapshot is taken | `snapshot_id`, `name`, `digest`, `bytes`, `entries`, `skipped`, `image` (container drivers) |
| `sandbox.restore` | a restore is done or refused | the same, `outcome` (`restored` or `refused`), `error` |
| `sandbox.forward` | a forwarded connection opens and closes | `port`, `action`, on close `bytes_in` and `bytes_out` |
| `sandbox.forward_listen` | a host listener opens and closes | `port`, `address`, `wide`, `action` |
| `sandbox.expose` | an exposure opens and closes | `exposure_id`, `port`, `owner`, `expires_at`, on close `reason` |

## Snapshots

```go
d := process.NewWithOptions(cfg, process.Options{SnapshotDir: "/var/lib/abhed/snapshots", Record: rec})
ref, err := driver.Snapshot(ctx, d, "sbx_1", "before-upgrade")
// later, with the sandbox stopped:
err = driver.Restore(ctx, d, "sbx_1", ref)
```

A driver declares `snapshot` only when `Options.SnapshotDir` is set. The
folder must be outside every workspace (a snapshot inside one is refused,
since the sandbox could read it), and should be reachable only by the
server's user; the driver makes each sandbox's folder `0700` and each
snapshot `0600`.

**What a snapshot holds.** The workspace as a gzip-compressed tar: folders,
regular files and links that stay inside it, with their modes and times.
Abhed's state (`.abhed`, in any case, at any depth) is never included. Links
pointing outside the workspace, sockets, devices and pipes are left out and
counted as `skipped`. The container drivers also commit the container to an
image, `localhost/<prefix>snapshot:<sandbox>-<snapshot>`; with the read-only
root this pins the exact image the sandbox ran, and the start after a
restore runs it.

A snapshot of a running sandbox is taken while its commands may write, and
a file that shrinks while it is read fails it. Stop the sandbox first
for a consistent one.

**The ref.** `driver.SnapshotRef` carries the snapshot's id, the generation
it was taken at, and `digest`, the SHA-256 of the archive as
`sha256:<hex>` (and `image`, the committed image's id). Whoever holds the
ref decides what may be restored: content that does not match the digest is
never restored.

**Restore** runs only on a stopped instance, and the start that follows is a
new generation (`controlplane.Sandbox.Restore` moves the object to
`Starting` and bumps it). In order:

1. The ref must name a snapshot of this sandbox; another sandbox's snapshot
   is not found.
2. The archive's digest is checked; a mismatch is refused before anything
   is written. A container driver also checks the committed image's id.
3. The archive is unpacked into a fresh folder inside the workspace, through
   an `os.Root` that no link or `..` can leave, with the checks a copy gets:
   relative names only, no `..`, no links pointing out, no devices or pipes,
   nothing in `.abhed`. Files are created, never replaced, and links are made
   after every file, so no write goes through a link the archive made.
   Setuid, setgid and sticky bits are cleared.
4. The digest of what was unpacked is checked again.
5. Only then is the workspace cleared, except `.abhed` and any folder that
   holds one further down, and the unpacked contents moved in. A snapshot
   with a file where such a folder is now is refused before anything is
   removed.

Deleting a sandbox deletes its snapshots and committed images, since they
restore only into it. There is no fast path yet: btrfs, ZFS or overlay
snapshots would be quicker on large workspaces, and the archive is gzip
rather than zstd so that no dependency is added.

On macOS the engine runs in a VM that sees the workspace through a file
share, which can show a removed file for about a second after a restore.

## Forwarding a port

```go
rwc, err := driver.Forward(ctx, d, "sbx_1", 8000)        // one byte stream
l, err := forward.Listen(ctx, d, "sbx_1", 8000, forward.ListenOptions{Record: rec})
fmt.Println(l.Addr())                                     // 127.0.0.1:<free port>
```

The bytes always enter through the driver's own channel: a relay run inside
the sandbox like any other command (bash's `/dev/tcp`, or `nc` where there is
no bash) connects to `127.0.0.1:<port>` from there. So:

- a container under `--network none`, or under the allowlist, is reached
  through the engine's `exec` and is never given a network for it;
- a forward reaches only what the sandbox itself could connect to. Under
  Seatbelt with the network off the sandbox can neither listen nor connect,
  so forwarding fails there, and the fence's network rules apply the same
  way;
- nothing listening is an error (`unavailable`), not a stream that ends at
  once. Only IPv4 loopback is reached.

A forward lives until it is closed or the instance stops: stopping or
deleting the sandbox closes every forward and every host listener bound to
it, without waiting out the stop's grace.

`forward.Listen` binds `127.0.0.1` on a free port unless `Addr` says
otherwise. An address that is not loopback is refused unless `Wide` is set;
when it is, the record says so (`"wide": true`).

## Exposing a service

```go
ex := expose.New(expose.Options{Enabled: true, Record: rec, MaxTTL: time.Hour})
mux.Handle(expose.DefaultPrefix, ex)   // behind auth.Middleware
e, err := ex.Expose(ctx, expose.Request{Driver: d, SandboxID: id, Generation: gen,
    Port: 8000, Owner: who.Owner(), Tenant: who.Tenant})
// e.Path is /v1/exposed/exp_<32 hex>/
```

Off unless the operator sets `Enabled`; then:

- an exposure is usable only by the principal that made it, in its tenant;
  anyone else, an expired exposure and an unknown one all get the same
  `404`;
- it ends when its TTL runs out (at most `MaxTTL`, one hour by default),
  when it is revoked, when the sandbox's incarnation stops, and, with
  `Options.Current` set, when the sandbox's generation moves on;
- the viewer's `Cookie` and `Authorization` never reach the sandbox; the
  service's `Set-Cookie` never reaches the viewer, and every response carries
  `Content-Security-Policy: sandbox …`, so a page the sandbox serves runs as
  an opaque origin and cannot use the viewer's session against the server.
  Because of that, a browser page's own scripts and subresources load
  without the session cookie; exposed services suit API clients with a
  bearer token today, and browser apps will need a separate origin;
- at most 16 exposures per sandbox.

`abhed serve` turns this on with `control_plane.expose`: `POST
/v1/sandboxes/{id}:expose` calls `Expose` with the authenticated identity
and returns `{id, url, path, expires_at}`, and `Options.Current` follows the
sandbox's generation. Every request to the exposure also makes the check
`GET /v1/sandboxes/{id}` makes, so a caller who loses the sandbox loses its
exposures at once.

An exposure lives in the memory of the server that made it. With several
servers behind a load balancer, a request for it that reaches another one is
`404`: route `/v1/exposed/` with session affinity, or to one server. A
restart ends every exposure.
