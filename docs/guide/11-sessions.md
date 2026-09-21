# Sessions and audit

Abhed's state is a chronological stream of events. The agent is a function from
that history to an action, and the runtime is a function from an action to an
observation. Everything else — replay, forking, export, audit — falls out of
that rather than being built separately.

## What is recorded

Every user message, model reply, reasoning block, tool call, approval, denial,
observation, compaction and terminal reason. Each event carries a sequence
number, an actor, and a **trust tag**: content read from files, tool output and
search results is data, never instruction, and the tag travels with it.

## Storage

In memory by default, which loses everything when the process exits. Postgres
makes sessions durable, and is what `/sessions`, `/resume`, replay and audit
need:

```json
"storage": { "driver": "postgres", "dsn": "postgres://...", "tenant": "default" }
```

The event table is append-only: database triggers refuse an update, a delete or
a truncate. Against the server's own credentials that holds only when the
server runs as a role that does not own the table — see
[two roles, not one](02-configuration.md). Tenants are isolated by row-level security, enforced at the data
layer as well as in the application — a boundary that exists in only one place
is not a boundary.

## Going back

```
/tree            the session's steps
/fork 12         rebuild the conversation up to step 12 and continue from there
```

A wrong turn three steps back should cost three steps, not the session.
Everything before it was still right, and re-establishing it means paying for
the same reading twice.

Forking needs no extra storage: the messages are derivable from events that were
already being recorded for audit. The log was never a description of the
session — it is the session.

## Getting it out

```
/export                     a self-contained HTML transcript
/export session.json        the raw events, for a program
```

The HTML embeds everything and fetches nothing, so it works from a filesystem,
an email attachment, or an air-gapped machine. Tool output is escaped and never
rendered as markup: a transcript is a record of untrusted content, and a session
that read a hostile file must not produce a page that runs it.

## Replay

```bash
curl -s $B/v1/sessions/$SID/replay
```

Deterministic replay is the point of the design. It answers what an agent
actually did, not what it reported doing — which is the question that matters
after an incident, and the one a summary cannot answer.

## Context over a long session

History is summarized as it approaches the window, keeping the recent exchanges
verbatim and re-injecting the system prompt and memory file whole. A single tool
result is capped at a quarter of the window, since no amount of summarizing
earlier turns rescues one message too large to send.

Compaction is visible: `/cost` reports how many have happened, and each one is
an event with its token accounting. It is a capacity number, not a curiosity —
every compaction invalidates the prefix cache and pays cold prefill again.

## Continuing a finished chat, on any node

A finished session is not a dead end. Posting a message to it continues it
from the record: the conversation is rebuilt from the event log the same way
a fork is, the new events extend the same sequence, the turn budget carries
over, and the policy is the one the deployment runs now. This is what lets a
session outlive the process that started it — after a restart, or on a
different replica behind a load balancer.

On the Postgres store the continuation is claimed atomically, so two replicas
asked to continue the same session at once cannot both do it; the second
answers `409`. Only the session's owner can continue it.

What this is not: a running turn on a node that dies is not migrated. It
ends, is recorded as interrupted, and the session can be continued from
there. High availability of *sessions* is this; high availability of
*turns in flight* is not something Abhed claims.

## Reading the workspace and what changed

With a chat open, **Files** and **Changes** in the console open a panel beside
the conversation: the workspace as a tree, any file in it, and a diff of every
file the agent has changed in this session against what it held before the
first edit. The same is available over the API:

```bash
curl -s "$B/v1/sessions/$SID/tree?path=cmd"        # one directory, folders first
curl -s "$B/v1/sessions/$SID/file?path=cmd/main.go"
curl -s  $B/v1/sessions/$SID/changes               # unified diffs
```

It is read-only. Nothing can be edited or saved from the browser: a change to
the workspace that no event accounts for would make the record incomplete.

What it will not show:

- anything outside the workspace, by `..`, by an absolute path, or through a
  symlink. The answer is `404`, the same as for a file that is not there.
- anything a `deny` or `ask` rule on `read` covers — `read(**/.env)`,
  `read(**/.ssh/**)`. The viewer is held to the rules the agent is; those
  files are left out of the tree and refused with `403`.
- `.git`, `node_modules`, and `.abhed`, which holds the server's own
  configuration and password hashes.
- more than 512 KB of one file (`truncated` is set), or a binary file's bytes
  (`binary` is set and there is no content).

**Changes** covers edits made with the `write` and `edit` tools. A file the
agent changed through `bash` is not listed. The originals are kept in memory
with the running session, so a session reopened from its record after a
restart reports `"available": false` rather than an empty list, and once it is
continued it lists changes from that point on.

## Deleting a chat

In the console, hover a chat in the list (or focus it with the keyboard) and
press the bin, or press Delete; confirm inline. The same is
`DELETE /v1/sessions/<id>`. Only the chat's owner can delete it, and a running
chat is stopped first.

What "delete" means depends on the store, and the console says so rather than
pretending:

- **Postgres** marks the session deleted. It disappears from every list, get
  and replay — nothing reads it through the API again — but the transcript
  rows stay, because the events table refuses `DELETE` by trigger and that
  refusal is a property this store promises. The mark records who deleted it
  and when. Someone with the database can still see it; someone with only the
  API cannot.
- **Memory** drops the events outright.
- A store built without deletion answers `501`, and the console reports that
  this deployment keeps an append-only record. A delete button that silently
  does nothing would be a privacy bug wearing a feature's clothes.
