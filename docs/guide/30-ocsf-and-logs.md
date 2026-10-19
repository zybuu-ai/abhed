# OCSF events and logs

Abhed's record is the source of truth: a hash-chained line for everything a
session did ([Sessions and the local record](12-records.md)). This page is
about reading it as a log: as [OCSF](https://schema.ocsf.io) events a SIEM
takes in, as one line per event in a terminal, and as a stream you can stop
and resume without losing or repeating a line.

- `abhed logs` prints the local record as shorthand, OCSF or its own lines,
  follows it with `-f`, and keeps its place in a cursor file.
- `record.ocsf` writes the local record to a JSONL file of OCSF events as it
  is recorded, rotated, with a cursor that survives a restart.
- `GET /v1/sessions/{id}/logs` streams a server session's record as OCSF over
  SSE, and `GET /v1/sandboxes/{id}:events?format=ocsf` pages a sandbox's
  control-plane record as OCSF.

Every one of these is derived from the record, line by line, at the time it
is read: nothing is collected on the side, so nothing is best-effort. An
OCSF reader that falls behind resumes from a record sequence number, and a
gap shows as a missing `metadata.sequence`.

## Versions and sources

Events are written in **OCSF 1.8.0**, and can be taken down to **1.5.0**,
**1.3.0** or **1.1.0** for a reader that needs an older one.

The schema facts the mapper relies on, each class and object it writes with
its attributes, their types, requirements, enum values and profiles, are
taken from the published schema server's API (`https://schema.ocsf.io/<version>/api/classes/<class>`,
`/objects/<object>` and `/profiles`) and kept in
`ocsf/schema/subset.json`. `scripts/ocsf-schema.py` regenerates it. The
tests check every event the mapper writes, in every version, against that
subset: an attribute the class or object does not define, a required one
left out, an enum value it does not list, or a profile's attribute without
the profile named in `metadata.profiles` fails them. The subset was taken
on 19 October 2026, when the server listed 1.9.0 as its current release and
1.8.0 among the published ones.

Where the record does not hold a fact a field asks for, the field is left
out rather than filled with a guess:

- `ai_model` is written only when the session named its model provider
  (`session.started`, `session.resumed` or `model.switched`), since OCSF
  requires `ai_provider`; `ai_model.version` and `uid` are never written.
- `device.type_id` is `0` (Unknown): Abhed does not know what kind of
  machine it runs on.
- HTTP Activity has no `http_response` and no `http_status`: the egress
  proxy records its decision on a request, not the server's answer.
- Process Activity has no `launch_type_id`, and `process` names only what the
  record does (`pid`, `cmd_line`, a background shell's id as `uid`, and the
  line's `sandbox`).
- DNS decisions are Network Activity, not DNS Activity (4003): the record
  keeps the name and the decision, not the query and answer DNS Activity
  describes.

## How a line becomes an event

| From the line | In the event |
|---|---|
| `seq` | `metadata.sequence` |
| `id` | `metadata.uid` |
| `session_id` | `metadata.correlation_uid`, and `actor.session.uid` |
| `type` | `metadata.event_code` |
| the tenant | `metadata.tenant_uid` |
| `created_at` | `time` (milliseconds); the exact time is `unmapped.record.created_at` |
| `actor`, `trust`, `parent_id`, `prev`, `hash` | `unmapped.record` |
| `payload` | `unmapped.payload`, as the record holds it |

`metadata.product` is `Abhed` by `Zybuu` with the version; `metadata.log_name`
is `abhed.record` (`abhed.controlplane` for a sandbox's record). The class's
own fields come from the payload, as the table below says.

**Lossless and replayable.** `unmapped.record` and `unmapped.payload` hold
everything the line holds, so the line can be rebuilt from the event: the
`ocsf` package's `Replay` does it, and the rebuilt line hashes to the
`unmapped.record.hash` the record gave it, which is how a reader checks an
OCSF file against the chain without the record. An older version keeps what
it does not define under `unmapped.downgraded`, so a downgraded event
replays too.

**Redaction.** The payload is the record's, redacted with the stored
secrets before the line was first written. An OCSF event, a shorthand line
and a logs stream carry no more than the record does, and never a value the
record withheld.

## Classes

| Record types | OCSF class (uid) | Activity |
|---|---|---|
| `session.started`, `session.resumed`, `session.ended`, `session.woken`, `subagent.spawned`, `subagent.returned`, `sandbox.restarted` | Application Lifecycle (6002) | Start, Restart, Stop, Other (Wake), Start, Stop, Restart. `session.ended` sets `status_id` from its reason |
| `user.message`, `agent.message`, `agent.reasoning`, `subagent.message` | API Activity (6003) | Other (Message), with `message_context.ai_role_id` User or Assistant |
| `model.call` | API Activity (6003) | Other (Model call): `ai_model`, tokens in `message_context`, latency as `duration`, an error as a failure |
| `action.requested` | API Activity (6003); File System Activity (1001) for `read`, `write`, `edit` and `memory_write` with a path; SSH Activity (4007) for `ssh`, `ssh_connect`, `ssh_put` and `ssh_get` | Call; Read or Update; Open or Traffic. `status` is `Requested` |
| `action.approved`, `action.denied`, `subagent.action` | API Activity (6003) | Other (Decision), with `action_id` and `disposition_id` (Allowed, Approved when a person answered, Blocked), and the deciding rule, scope or layer as `policy` |
| `observation` | API Activity (6003) | Other (Result): success, failure, Not run, or Unknown for an outcome a crash left unknown; `duration` |
| `hook.fired` | API Activity (6003) | Other (Hook): a `block` is Denied and Blocked, `ask` and `annotate` Observed |
| `mcp.status` | API Activity (6003) | Other (Restart), the server as a resource |
| `plan.proposed`, `session.branched`, `conversation.forked`, `sandbox.snapshot` | API Activity (6003) | Create |
| `memory.loaded` | API Activity (6003) | Read |
| `model.switched`, `model.fallback`, `session.renamed`, `session.named`, `sandbox.restore` | API Activity (6003) | Update |
| `plan.decided`, `plan.step`, `subagent.ask`, `subagent.backgrounded`, `subagent.notice`, `terminal.input`, `command.invoked`, `change.accepted`, `worktree.taken`, `task.cancelled`, `message.dropped`, `turn.continued` | API Activity (6003) | Other, named for the type |
| `input.mention`, `memory.written`, `manual.edit`, `file.restored`, `checkpoint.saved` | File System Activity (1001) | Read, Update, Update, Create/Update/Delete by what the restore did, Other (Checkpoint); the file's SHA-256 where the line has one |
| `process.launched`, `shell.started`, `shell.ended` | Process Activity (1007) | Launch, Launch, Terminate with `exit_code` |
| `egress.decision` | Network Activity (4001); HTTP Activity (4002) for a plain HTTP request with its method | Open, Refuse for a denial, Traffic for a rate-limit summary (its count as `count`); the method's activity for HTTP. `action_id`/`disposition_id` (Allowed; Denied and Blocked; `would_deny` in audit mode is Allowed and Detected), the rule as `firewall_rule.name`, bytes as `traffic` |
| `sandbox.forward`, `sandbox.forward_listen`, `sandbox.expose` | Network Activity (4001) | Listen, Close |
| `mode.changed`, `permission.changed`, `policy.layer`, `policy.startup_trusted`, `config.refused`, `config.narrowed`, `approval.scope_granted`, `workspace.dir_added`, `sandbox.bound`, `fence.qualified`, `session.wake_set` | Device Config State Change (5019) | Log; a refused setting is a failure |
| `monitor.verdict`, `answer.verdict`, `supervisor.correction`, `supervisor.replan`, `fence.limit`, `fence.state_planted`, `sandbox.git_planted`, `sandbox.git_linked`, `sandbox.git_walk_bounded`, `record.repaired` | Detection Finding (2004) | Create, the type as `finding_info.analytic.name`. A verdict that tightened a decision and a contradicted answer are Medium, a planted `.abhed` or git folder High, the rest Low or Informational |
| `compaction.started`, `compaction.completed`, `context.offloaded`, `plan.updated`, `todo.updated`, `suggestion.offered` | Base Event (0) | Other |
| `agent.delta`, `agent.reasoning.delta` | not mapped | fragments of a reply; the whole text is the `agent.message` or `agent.reasoning` that follows |

A type the record gains later, before the table maps it, is written as a
Base Event rather than dropped; the tests fail until it has a row. Approvals
are API Activity with the Security Control profile rather than Authorize
Session (3003), which OCSF defines as privileges given to a user's sign-in.

**Profiles.** `metadata.profiles` names the ones an event uses: `host`
(`actor`, `device`), `security_control` (`action_id`, `disposition_id`,
`policy`, `firewall_rule`) and, in 1.8.0, `ai_operation` (`ai_model`,
`message_context`). 1.1.0 and 1.3.0 do not apply Security Control to API
Activity, and no version before 1.8.0 has AI Operation; a downgrade moves
those fields under `unmapped.downgraded`.

**Downgrades.** An attribute or profile the older version does not define
moves under `unmapped.downgraded.attributes` by its dotted path, with
`unmapped.downgraded.from`; an enum value it does not list becomes Other
(99) where it has one, the original kept the same way. HTTP Activity before
1.5.0 requires a response, so an egress decision on a request is written as
Network Activity (Open, or Refuse for a denial) in 1.3.0 and 1.1.0.

**The control plane.** A sandbox's record (`/v1/sandboxes/{id}:events`) is a
table apart from the session record. With `?format=ocsf` each row is API
Activity: the principal as `actor.user`, Create, Read, Update or Delete from
the action's verb (Other for the rest, such as `exec`), the outcome as
`status_id`, the sandbox as a resource, and `unmapped.audit` with its
generation and detail. Those rows are not hash-chained yet.

## The shorthand

```
2026-10-19T11:58:22.123Z s-1#21 API:Decision INFO ALLOWED allowed at allow
2026-10-19T11:58:24.123Z s-1#23 API:Decision MED DENIED denied at deny
2026-10-19T11:59:04.123Z s-1#63 NET:Refuse MED DENIED connect evil.example deny
```

Time (UTC, milliseconds), `session#seq`, the class (`LIFE`, `API`, `FILE`,
`PROC`, `NET`, `HTTP`, `SSH`, `CONF`, `FIND`, `BASE`) and activity, the
severity, the security control's action when there is one, and the event's
message on one line. It is for reading; parse the JSON.

## `abhed logs`

```bash
abhed logs                         # the latest session in this workspace, as shorthand
abhed logs fix-build -f            # follow one session as it is recorded
abhed logs -all -format ocsf -cursor ~/ocsf.cursor -f >> siem.jsonl
abhed logs s-01J… -format ocsf -ocsf-version 1.5.0 -since 2h -type egress.decision
abhed logs s-01J… -format record -after 120   # the chained lines themselves
```

| Flag | |
|---|---|
| `-f`, `-follow` | keep printing lines as they are recorded, by this process or another, until interrupted |
| `-since D\|TIME` | only lines from the last `D` (`10m`, `2h`) or after an RFC 3339 time |
| `-after SEQ` | only lines after this seq, for one session |
| `-cursor FILE` | start after the seqs `FILE` holds and keep them there as lines are printed: a reader that is stopped and started again goes on where it stopped, with nothing twice and nothing missed |
| `-format` | `shorthand` (the default), `ocsf` (one JSON event a line) or `record` (the record's own lines, `prev` and `hash` included) |
| `-ocsf-version` | `1.8.0` (the default), `1.5.0`, `1.3.0` or `1.1.0` |
| `-type T,T` | only these record types |
| `-all` | every session in the record, subagents' included; new sessions are picked up while following |

A session is named as `abhed record` takes it: its id, a unique id prefix,
its name, or the path of its file. `abhed logs` reads the local record; with
`storage.driver` `postgres`, read a server's sessions from its logs
endpoint. The cursor file holds only seqs; keep it where you keep the log
it feeds.

## The OCSF file

```json
"record": {
  "ocsf": { "enabled": true, "version": "1.8.0", "max_mb": 64, "keep": 5 }
}
```

| Key | Default | |
|---|---|---|
| `record.ocsf.enabled` | `false` | write the local record to the file as it is recorded |
| `record.ocsf.version` | `1.8.0` | `1.5.0`, `1.3.0` or `1.1.0` for an older reader |
| `record.ocsf.max_mb` | `64` | rotate the file once it would pass this size |
| `record.ocsf.keep` | `5` | how many rotated files (`events.ocsf.jsonl.1`, `.2`, …) are kept; the oldest goes |
| `record.ocsf.path` | `<record dir>/<tenant>/ocsf/events.ocsf.jsonl` | **managed only**; its folder is then state the agent can neither read nor write, and may not be in a workspace |
| `record.ocsf.locked` | `false` | **managed only**: the managed file's `record.ocsf` is then the whole of it, and nothing any other file sets under it applies |

Without a managed lock, a managed value still wins where the managed file
sets it, and your own file or a trusted workspace's sets the rest. The
default folder is inside the record's, which commands cannot reach.

The file is written from each line as the record writes it, and a cursor
beside it (`events.ocsf.jsonl.cursor`) keeps the last seq written of each
session:

- Turning it on starts it at each session's present end; the history before
  is `abhed logs -all -format ocsf`'s to write.
- Lines recorded while no process had the file on (it was off, or Abhed was
  not running, or a command that does not open it wrote them) are written
  the next time a process with it on opens the record. A line that did not
  fit the in-memory queue is read back from the record, in order.
- Several Abhed processes share the file under a lock; none writes a line
  another already did. A process that crashes between writing lines and
  moving the cursor writes them again next time: delivery is at least once,
  and `metadata.uid` tells a repeat.
- The file's directory is `0700` and its files `0600`. A write that fails is
  warned about once and retried from the record; the record itself is never
  held up by the file.

The file covers the command line's local record: `abhed`, `abhed -p`, `abhed
acp` and `abhed rpc`. `abhed serve` keeps sessions in memory or in Postgres
and writes no OCSF file; use its logs endpoint.

## Over the API

```
GET /v1/sessions/{id}/logs                → SSE of OCSF events
    ?format=shorthand                     one line per event instead
    ?version=1.5.0                        an older OCSF version
    ?type=egress.decision,action.denied   only these record types
    ?after=SEQ  or  Last-Event-ID: SEQ    resume after a seq
GET /v1/sandboxes/{id}:events?format=ocsf[&version=V]   a page of a sandbox's record as API Activity
```

The logs stream is the events stream (`/v1/sessions/{id}/events`) mapped:
the same sign-in, the same check that the caller may read the session (a
session that is not theirs is `404`), the same backlog and live tail, and
the record's seq as each SSE `id`, so an `EventSource` that reconnects
resumes where it stopped. A bad `format` or `version` is `400`.

## What is not covered

- Abhed has no fence supervisor process or TLS broker of its own yet: there
  are no supervisor spans, and HTTPS traffic is recorded as the egress
  proxy's decisions on connections, with a request's method and path only
  for plain HTTP or with `egress.inspect_tls`.
- The control plane's sandbox record is mapped, not chained.
- `abhed serve` writes no OCSF file, and its in-memory sessions are gone with
  the process.
- Sending events to a SIEM (syslog over TLS, Splunk HEC, Elastic, object
  storage with retention), and OpenTelemetry, are Enterprise features.
- Windows event tracing is not read.
