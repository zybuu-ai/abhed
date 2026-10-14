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

Before any event is written it passes the redactor: a value from the secrets
store, as written or in the encoded forms
[Secrets](04-permissions.md#secrets) lists, is replaced with `[secret:NAME]`. The
record is append-only, so this is the only moment a secret can be kept out of
it. A streamed reply or reasoning is redacted across its fragments, not one
fragment at a time, so a value split between two is still caught. See
[Secrets](04-permissions.md#secrets).

## Storage

The command line keeps sessions in the local record by default: a chained,
verifiable file per session under `~/.abhed/records`, described in
[Sessions and the local record](12-records.md). `abhed serve` keeps them in
memory unless configured otherwise, which loses everything when the process
exits. Postgres makes sessions durable for a server and for teams:

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
/rewind          take code and/or the conversation back to before a prompt
/resume <id>     replay a recorded session and continue its conversation
```

`-c`, `-r`, `/rename`, `/branch` and `/rewind` are covered in
[Sessions and the local record](12-records.md).

In the interactive CLI, every task you type continues one conversation, and
the session's record holds all of them: one sequence, with a `session.ended`
after each task. The turn limit counts the whole conversation. `/clear`
starts a new conversation and a new session, and `/resume` switches to
another; either way `/cost`, `/diff`, `/undo` and any "always allow" scope
start over. A resumed session's recorded totals (turns, tokens and
compactions) go on from where its record left them. Each new session gets a
random id, so two CLIs started in the same second never share one. After `/fork` or `/resume`, the
next task continues from the rebuilt conversation, and its events extend
that session's record. A fork is recorded as a `conversation.forked` event:
the steps it abandoned stay in the record for audit, but no later `/fork`,
`/tree` or `/resume` brings them back. A fork is refused while background
tasks are running, naming them: wait for them or cancel them (`/tasks cancel`),
then fork. It never cancels them itself. `/resume` continues only a session that is not
running elsewhere, and on Postgres only one recorded as yours, in your
tenant; another user's session is not replayed either. A subagent's session
id is refused, in the CLI and in the console: it is shown, but continues only
through the session that started it. The CLI names that session when
the record carries it; the console refuses the id as it refuses an
unknown session. On Postgres each
task's end releases the session, so the CLI claims it again before the next
task, `/fork` or `/compact` writes to it. If another process has continued it
meanwhile, the conversation is rebuilt from the record first; if another
process is running it now, the task is refused. `/fork` and `/compact`
record the session's last end again after writing, with the same reason and
totals, so the session is left as they found it. Leaving a resumed session
unused, by `/clear`, another `/resume` or quitting, leaves it as it was.
During a store outage, a failed task can leave the session claimed until the
store answers again.

A wrong turn three steps back should cost three steps, not the session.
Everything before it was still right, and re-establishing it means paying for
the same reading twice.

Forking needs no extra storage: the messages are derivable from events that were
already being recorded for audit. The log was never a description of the
session — it is the session.

## Finding and switching chats

The console's rail lists your chats in order of last activity, so a chat you
just continued comes first, grouped by Today, Yesterday, This week and
Earlier. Activity is the conversation: your messages, the agent's replies and
the calls it makes. Opening a chat, its terminal or its files, renaming it or
switching its model does not move it. Each row's age is how long since then.

- **Search** matches every word you type in a chat's title or its first
  message, and says so when nothing matches.
- **The open chat is in the address**, as `/console?s=<id>`. A reload, a
  bookmark or a second tab opens that chat again, so two tabs can stay on two
  chats. **Workbench** in the header opens the same session there.
- **The header shows the title.** Its tooltip and **copy id** give the id.
  Double-click the title, or press F2 on a row, to rename the chat in place;
  Enter keeps it and Esc does not. A title with hidden characters is shown
  with them written out, and is not put in the box raw.
- **Ctrl+K** (⌘K on a Mac) opens a quick switcher: type to filter, ↑↓ and
  Enter to open, Esc to close. The open chat is marked.
- **Alt+↑ / Alt+↓** goes to the chat above or below the open one in the rail.
  In a message box with text in it the keys move the cursor instead.

`GET /v1/sessions` returns each session's `updated` (when its conversation last went on)
alongside `created`, newest activity first. It also takes:

| Parameter | |
|---|---|
| `q` | only sessions whose title or opening request contains it, ignoring case; an id prefix matches too |
| `limit` | at most this many, 1 to 200 |
| `cursor` | the page after the one whose response carried it |

When more remain, the response has an `X-Next-Cursor` header; pass it back as
`cursor` for the next page. The body is the same JSON array as before, and a
request with none of these returns the whole list as it always did, up to
your 200 most recently active sessions. A search or a paged list looks at
your 500 most recently active sessions and no further: a session older than
those is not found by `q` and is past the last page, though it still opens
by its id.

## Getting it out

```
/export                     a self-contained HTML transcript, in ~/.abhed/exports
/export session.json        the raw events, for a program
/export session.jsonl       the chained record with its head, verifiable offline
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
answers `409`. Only the session's owner can continue it. Opening a finished
session in the workbench to read it does not claim it or write to it: it
stays ended, with its reason, and a background result it still owes is
delivered only once something claims it. The first thing written to it, a message or workbench work such
as a save or a terminal, claims it, after catching up on anything another
process recorded meanwhile; while another process is running it, that write
is refused. A claim taken for workbench work alone is given back two minutes
after its last write, once no terminal is open and no result is owed, by recording the end it was
opened with again, so that work appears in the record after an end. A
session running elsewhere is not opened at all.

A running turn on a node that stops, whether it dies or drains past
`drain_seconds`, is not moved to another node in this edition. It ends, is
recorded as interrupted, and the session can be continued from there.
Before the end, each call the turn left open gets its one outcome: one that
was approved, and so may have run, is answered "outcome unknown", and any
other is denied with step `handover`. A call answered "outcome unknown" is
asked of a person before the same call (tool and arguments) runs again in
that session, in every mode.

Moving a running turn to another replica, on a drain or once a replica
dies, is part of the Enterprise edition's high availability. An edition
does it through `server.Options.TurnMover`; with none set, as here, nothing
continues a stopped turn on its own.

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

This panel is read-only; edits from the browser go through the
[workbench](16-workbench.md), where each save is a recorded action, since a
change to the workspace that no event accounts for would make the record
incomplete.

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

**Changes** covers edits made with the `write` and `edit` tools, and files
changed by shell commands, marked `by_command`, against a note of the
workspace taken as the session opens; past 20,000 files `partial` says the
rest were not compared. The originals are kept in memory
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

- **Postgres** erases what the session said and keeps that it happened. Each
  session's event payloads and file pre-images are sealed (AES-256-GCM) with
  a key of its own, kept in `session_keys`; delete removes the key, along with
  the session's label, title and its approvals' arguments. The event rows
  stay, because the events table refuses `DELETE` by trigger and that refusal
  is a property this store promises, but what is left in them is ciphertext
  no one can open: type, actor, time and hash remain, the words do not. Its
  subagents' sessions go the same way. The mark records who deleted it and
  when. Every replica reads a session's key from the database on each read
  rather than from memory, so once the delete commits no server that shares
  the database opens those payloads again; an audit export over the time
  range shows the deleted session's events as `{"erased":true}`. A write to a
  deleted session, such as a run another server still holds, is refused
  rather than stored. What
  erasure does not reach: sessions recorded before 1.2.8, whose
  payloads were never sealed; a database backup or WAL archive taken before
  the delete, which still holds the key until it expires; a streaming
  replica, until it replays the delete; and, until `VACUUM` runs and the
  replicas replay it, the deleted key's bytes in the table's free space.
- **The local record** (`~/.abhed/records`) deletes as `abhed record prune`
  does: the session's file, its subagents' and the pre-images no other session
  names are removed, and a tombstone in the index keeps its id and head. Lines
  the index wrote earlier, such as its title, stay, since the index is
  append-only.
- **Memory** drops the events outright.
- A store built without deletion answers `501`, and the console reports that
  this deployment keeps an append-only record. A delete button that silently
  does nothing would be a privacy bug wearing a feature's clothes.
