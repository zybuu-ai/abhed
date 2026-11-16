# Shipping the record to the org server

With `record.ship.enabled`, Abhed sends each session's record to the org
server this machine is signed in to (`abhed login`, see
[Signed org policy](35-org-policy.md)) as it is written. The server receives
the lines exactly as the local record holds them, so it checks the same hash
chain `abhed record verify` does, and an export from it verifies offline.

`record.ship` is managed-only: the org policy or `/etc/abhed/config.json`
turns it on, never your own or a workspace's file. Either way it ships only
to a server this machine enrolled with, which `abhed login` does only with
your consent or when `/etc/abhed` names the server; with no enrolment the
record stays here.

```json
{"record": {"ship": {"enabled": true, "max_backlog_mb": 512}}}
```

| Key | Effect |
|---|---|
| `enabled` | ship sessions started from the moment shipping first began on this machine |
| `max_backlog_mb` | past this much unshipped record, a `backlog` event is raised and reported (512) |

## What is sent

- Every session, subagents' own included, created after shipping first began
  here; sessions recorded before are not sent.
- Each line byte for byte, with the session id, the hash of the line before
  and a host name. The server learns who you are from the sign-in token.

The record is redacted before it is written: stored secrets and the
patterns Abhed recognises never reach the file. Before a line is sent, it is
checked once more against the secrets store; a line it would still change (a
secret stored after the line was written, say) is not sent, nor anything
after it in that session, and a `blocked` event says so.

## When it is sent

Each line written nudges the shipper, which sends what is new in batches of
up to 1 MiB; closing a session makes a last pass. Requests carry your
sign-in token, refreshed as `abhed login` keeps it.

The record itself is the queue. How far each session has reached the server
is kept in `~/.abhed/state/record-ship.json`, out of the agent's reach.
Offline, the record goes on growing on disk; the next pass, in this or a
later Abhed, goes on from where the server is. The server answers each batch
with how many lines it holds, so an answer lost on the way does not send a
line twice or skip one; a server that names a new place more than three
times in a pass is given up on until the next. Batches of several lines are
at most 1 MiB; a longer line goes alone. Only one Abhed process ships at a
time.

## Events

The shipper raises, prints, keeps in its state, and reports to the server:

| Event | Meaning |
|---|---|
| `blocked` | a line still holds a stored secret; that session stops shipping |
| `diverged` | the local record no longer holds lines already shipped (rewritten or restored); that session stops |
| `refused` | the server refused a batch, such as one whose chain does not verify; that session stops |
| `oversize` | a line is longer than 8 MiB, which no batch carries; that session stops |
| `gap` | a session was pruned before all of it was shipped |
| `backlog` | more is waiting than `max_backlog_mb` |
| `resumed` | shipping reached the server again after failing |
| `access_removed` | the server refused the sign-in because the organisation removed the person's access; kept and printed here, never sent |

Each session's `session.started` event records `org_policy.record_ship: true`
when shipping is on.

## When access is removed

When the organisation removes the person's access (an Enterprise server
revokes them, or deprovisions them through SCIM), the server answers this
machine's sign-in with `401` and `"access_removed": true`. Shipping then
stops with a warning, printed when it happens and again as each later
session starts:

```text
abhed: ⚠ Your organisation access was removed: https://abhed.example.com refused this machine's sign-in (…). The record stays on this machine and is not shipped, and the org policy it holds still applies; ask your administrator.
```

`abhed record ship`, `abhed doctor` and Studio's Health view (the
`record_ship` check, failing) say the same. The record keeps being written
here; once a pass reaches the server again (access restored and `abhed
login` run), the warning clears and what waited is shipped. A sign-in that
only lapsed is not taken for a removal: it says to run `abhed login` again.
The warning clears only when the server answers a batch with success, not on
a pass that sent nothing. The same holds for a failed pass: until a batch is
answered, `abhed record ship` and doctor keep saying the last pass failed,
even when the network is back but nothing is waiting.

The marker is the word of whatever answers at the enrolled org URL, taken
without a signature. That URL is `https`, unless this machine enrolled to a
plain-`http` server with pinned keys, where anyone on the network path can
forge it. Either way it changes only what the warning says: an ordinary
refused sign-in already stops shipping, nothing is deleted because of it,
the record stays here, and each pass tries again.

The same warning names a backlog over `max_backlog_mb` at a session's start,
beside the `backlog` event raised when it first goes over.

## Commands

```sh
abhed record ship          # status: server, since when, bytes waiting, each session, the latest events
abhed record ship now      # one pass now
```

## The server side of the protocol

- `POST /v1/org/records/<session>/lines`, `Content-Type: application/x-ndjson`:
  a header line `{"abhed_ship":1,"session":"…","from":N,"before":"<hash of line N>","host":"…"}`,
  then the record's lines N+1 onwards. The server answers `200
  {"lines":M,"hash":"…"}` once it holds them; `409` with where it is when `from`
  or `before` does not match what it holds; `422` with a reason when a line
  does not verify.
- `POST /v1/org/records/events`: `{"events":[{"kind":…,"session":…,"detail":…,"at":…}]}`.

`local.CheckSegment` checks a batch as the record writes it, and
`local.WriteExport` writes what the server holds as an export
`abhed record verify FILE` checks.
