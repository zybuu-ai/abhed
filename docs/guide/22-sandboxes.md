# Sandboxes over the API

`abhed serve` can manage sandboxes as objects of their own: create one, run
commands in it, stop it, start it again, delete it, all over `/v1/sandboxes`.
Each sandbox is confined exactly as a session's commands are today (the
`process` driver builds it from your `sandbox` settings), and every change
to one is recorded with who asked for it.

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
| `workspace_root` | the server's workspace | Gives each sandbox a folder of its own, `<root>/<id>`, removed when the sandbox is deleted. Unset, every sandbox works in the server's workspace, as sessions do. Must be absolute. |
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

With `storage.driver` `postgres`, sandboxes and their record live in the
same database as sessions and survive a restart; run `abhed migrate` after
upgrading, as for any new table (the server refuses to turn the control
plane on against an older schema and names what is missing). The runtime
role gets `SELECT`, `INSERT`, `UPDATE` and `DELETE` on `sandboxes` and
`sandbox_idempotency`, and only `SELECT` and `INSERT` on `sandbox_audit`,
which is append-only like `events`. Without Postgres, sandboxes are kept in
memory and lost when the server stops.

## Check before you serve

```bash
abhed serve -check -addr :8080
```

Loads the configuration, resolves the model, reads the secrets store, builds
the sandbox, builds sign-in, opens storage, builds the control plane when it
is on (listing every setting, where its value came from, and which are
ceilings) and binds the listen address once to release it. It serves nothing.
Each check prints `ok` or `FAIL`; the exit status is 0 only when all pass.

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
weaker.

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

`:attach`, `:ssh`, `:forward`, `:expose`, `:cp`, `:snapshot`, `:restore`
and `:exec` with `"stdin": true` answer `501` with code `unsupported` in this
version.

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

With Postgres, a server restart loses the `process` driver's instances; the
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
