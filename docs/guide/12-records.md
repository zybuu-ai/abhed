# Sessions and the local record

Without a database, Abhed keeps every session in a **local record** on your own
machine. Sessions survive the process: you can list them, resume them, branch
them, rewind them and verify them later. The command line uses it whenever
`storage.driver` is not `postgres`, and the SDK uses it when given
`Options.Store` (see [SDK](09-sdk.md)).

Records are kept until you prune them. Nothing is deleted automatically unless
an organisation's managed configuration sets `record.retention_days`.

## Where it is

```
~/.abhed/records/<tenant>/
  <session>.jsonl        the session's events, one per line, chained
  index.jsonl            what sessions exist; append-only, also chained
  index.head             the index's last line
  head/<session>         the session's last synced line
  locks/<session>.lock   held by the one process writing the session
  blobs/sha256/..        file contents saved before the agent changed them
```

- The tenant is `storage.tenant`, `default` if unset.
- Directories are `0700` and files `0600`. A directory or file found wider is
  made private again when it is opened. On macOS and Linux, a records directory
  that another user owns is refused; Windows has no such owner check and relies
  on the profile folder's access list.
- The record's files are opened without following a link, and a session file
  with a second name (a hard link) is refused. `~/.abhed/records` may itself be
  a link: it is then used, and protected, by where it really is, or refused if
  that is somewhere the agent's commands can write, such as a temp folder.
- Only the managed configuration can move the record (`record.dir`). The
  setting is ignored in your own `~/.abhed/config.json` and in a workspace's.
- The records directory is Abhed's state. The agent's file tools refuse it, and
  the sandbox tiers deny it to the agent's commands. This covers
  `~/.abhed/records`, a managed `record.dir`, the real path of a linked records
  directory, and a record handed to an SDK agent, which is refused if it is
  inside the workspace or any other folder the agent can write. A command run
  with no sandbox at all can reach whatever your own account can.
- One writer per session relies on `flock` (on Windows, `LockFileEx`), so the
  record belongs on a local disk. On a network filesystem the lock may be
  refused, or not hold between two stores in one process.

## What a line holds

Each line is one event, written as canonical JSON with its fields in a fixed
order:

```json
{"seq":3,"id":"01J…","session_id":"s-…","type":"user.message","actor":"user",
 "trust":"trusted","created_at":"2026-09-30T10:12:03.52Z","payload":{…},
 "prev":"<sha256 of the line before>","hash":"<sha256 of this line without hash>"}
```

- `hash` is the SHA-256 of the line's own bytes with the `hash` field left out.
- `prev` is the `hash` of the line before it. The first line's `prev` is 64
  zeros.
- The payload's object keys are sorted, with no whitespace, and numbers are
  kept exactly as written. Writing a payload twice gives the same bytes.
- Secrets are redacted before the first write. A value from the secrets store
  becomes `[secret:NAME]` in every payload, the index and every export.
- Checkpoint blobs are the one exception: they hold your file's content
  unredacted, since a redacted copy could not restore the file. They sit in the
  same private directory and go when their session is pruned. A file that a
  read deny rule covers, or whose name says it holds keys (`.env`, `*.pem`,
  `id_rsa`, anything under `.ssh/` and the like), is not copied at all: its
  checkpoint records why, and undo and rewind cannot restore it.

A line is written in a single `write` call. A write that fails part way, as on
a full disk, is undone, so the file never holds half a line in its middle. The
file is synced, and the head moved to the new line, on a session's first line
and at each turn boundary: a prompt, the end of a round trip to the model, the
end of a run, and each fork, restore, rename or branch. On macOS a sync is
`fsync`, as SQLite uses by default, not the slower full flush of the drive's
cache.

- If the process dies, nothing it wrote is lost.
- If the machine crashes or loses power, the events since the last boundary
  can be lost; after a power cut, so can what the drive itself had cached
  ([SQLite's `fullfsync`](https://sqlite.org/pragma.html#pragma_fullfsync)
  explains the trade). The head can then survive while cached lines do not:
  the session reports lines missing, a false alarm of a cut, and is not
  written to again. `abhed -r` goes on from it in a new session after a yes.
- A session and the index are created with a head that counts no lines,
  before their first line. A crash before the first real head leaves that
  head behind one line, which is noted, not failed; behind more than one it
  is damage, and so is a missing head file. A session whose first real head
  cannot be written takes no more lines.
- If the index fails (a lost or damaged `index.head`, a cut line), no new
  session starts until it is looked at. `abhed record verify` names the line.
  Moving `index.jsonl` and `index.head` aside keeps them as evidence and
  starts a new index; a session file from the old one goes on with
  `abhed -r <file>`, copied into a new session.
- An unfinished last line left by a crash is cut off when the session is next
  opened for writing, but only when the head does not count it. The cut is
  recorded as a `record.repaired` event that says how many bytes went. A whole
  last line that lost only its newline is completed, not cut. Reading a
  session never changes it.

## One writer per session

A session is written by one process at a time, which holds its lock file.

- A process creating or continuing a session takes the lock, and keeps it while
  the session is its conversation.
- Another process that tries to continue the same session is refused. The
  command line then offers to fork it into a new session.
- The operating system drops the lock when the process exits, so a crash never
  leaves a session stuck.
- The command line and the desktop app can each hold a session of their own in
  the same record.

## Starting, resuming and naming

```
abhed -c                     continue this workspace's most recent session
abhed -r                     pick a session to resume from a list
abhed -r auth-fix            resume by name, id, a unique id prefix, or file
abhed -c --fork-session      go on in a copy, leaving the original as it was
abhed -n auth-fix            name the new session
abhed -c -p "and the tests"  continue headless
```

- `-c` and `-r` also work with `-p`. `-p` cannot show the picker, so name the
  session there.
- Resuming replays the last ten prompts on screen. Nothing is run again.
- The session goes on with its turn count, budget and model. If the model is
  different now, you are told, and `/model` switches back.
- Before a session goes on, its record is verified. If the check fails, you are
  shown the record as **unverified**, with the step and the reason, and it goes
  on only if you answer yes. The failed line stays in the record for anyone to
  find.
- `-r path/to/file.jsonl` takes a record from elsewhere, such as an export. The
  file is verified first, then copied into a new session here; the file itself
  is not changed. The copy passes this machine's secrets redaction, and its
  events are marked `untrusted`, as are copies from a record that failed
  verification: anyone can write a file with a valid chain.

Inside a session:

| Command | What it does |
|---|---|
| `/sessions` | this workspace's sessions, newest first |
| `/resume [id or name]` | continue a session; with no argument, pick one |
| `/rename <name>` | name this session (recorded as `session.named`) |
| `/branch [name]` | go on in a copy of this session; the original is left as it was |
| `/clear [name]` | end this session and start a new one; nothing is deleted |
| `/export [path]` | write the transcript |

A branch, from `/branch`, `--fork-session` or a file, is a new session. Its
record opens with `session.branched`, which names the source session and the
last step taken from it, followed by a copy of the conversation. Rebuilding
the branch from its record gives exactly the source's conversation at that
point.

## Rewind and checkpoints

Before the agent changes a file, its content and permission bits go into the
record's blobs. A `checkpoint.saved` event records the file's path and hash,
or, for a file that is not copied, why not.

```
/rewind          pick a prompt, then choose what to take back
/rewind 2        the second prompt back from the latest
/undo            code only: the last turn that changed files
```

`/rewind` takes the code, the conversation, or both back to just before the
prompt you pick:

- **The conversation side is a recorded fork.** A `conversation.forked` event
  names the step the conversation now goes on from. Nothing is deleted: the
  abandoned steps stay in the record and in `abhed record show`, and only
  leave the conversation.
- **Rewinding to the first prompt is a fork at step 0 in the same session.** You
  get an empty conversation; the session is not dropped or replaced.
- **Each file put back is your own write.** It is recorded as your action: the
  request, the policy's decision and the result, so a path a deny rule protects
  is left alone and the refusal is recorded. Each file restored is then
  recorded as `file.restored` with the SHA-256 of its content before and after,
  and gets its permission bits back.
- A branch keeps the undo history: `/undo` and `/rewind` in a branch offer the
  same checkpoints the source would.

Checkpoints outlive the process. After `abhed -c`, `/undo` and `/rewind` still
work, reading the content from the blobs and checking each blob's hash as it is
read. Only changes made with the `write` and `edit` tools are checkpointed; a
file changed through `bash` is not.

## Checking the record

```
abhed record list [-all|-repo]   sessions, newest first
abhed record show <session>      a session's events
abhed record verify              the index and every session
abhed record verify <session>    one session
abhed record verify file.jsonl   an export, or a copy from elsewhere
```

`verify <session>` checks the index first, then the session. It fails, naming
the step, the line and the event where it can, when:

- a line was edited, even by one byte or one space;
- a line was removed, added or moved, or a step appears twice;
- lines the head counts were cut from the end, whole or part way;
- the head file is missing, malformed, or names a line the record does not hold;
- a line the index recorded at the end of a run is gone or different, which
  still shows after the head file was rewritten to match a cut;
- a session the index lists is missing with no prune recorded, or a session
  file in the records folder is not in the index (checked by `verify` with no
  argument, and for a file named on its own);
- the index was edited, reordered or cut, or its head no longer matches it.

A record that fails is never written to again. Reading it, verifying it,
exporting it or opening it for writing leaves every byte as found. Going on
from it is a fork into a new session, after you confirm, and the fork's
`session.branched` names the source and why it failed.

What `verify` cannot show:

- **Lines after the last sync.** The head moves at turn boundaries, so lines
  written since then (streamed reply fragments, a tool's events mid-turn) can
  be cut from the end without a trace, and a crash can lose them.
- **A rollback.** An earlier copy of a live session's file put back with its
  head, or of the whole records folder (a backup or Time Machine restore),
  verifies as OK: every line and head in it is genuine. A session that had
  ended since is still caught, by the end head the index kept for it, unless
  the index was rolled back too.
- **A record cut back to its creation.** A session or the index cut to its
  first line, or to nothing, with its creation head written back, looks
  exactly like a crash during its first write. That is the same edit as
  rewriting a head, below. With two or more lines left, a creation head fails.
- **An owner who rewrites the evidence.** Verification is only as strong as
  the head and index files, and the same owner can rewrite those as well as
  the lines, or compute a whole new chain. The record is tamper-evident against
  the agent and against accidental or partial edits, and it can be verified
  offline. It is not proof against the machine's owner. Abhed does not anchor
  the chain outside the machine.

## Exporting

```
abhed record export <session>                 ~/.abhed/exports/<session>.jsonl
abhed record export <session> -format html -o report.html
/export                                       the current session, as HTML
/export notes.jsonl                           a path is taken from the workspace
```

Exports go to `~/.abhed/exports` unless you give a path, never into the
workspace, where they would end up in the repository.

A `.jsonl` export is the session's lines exactly as recorded, followed by one
trailer line holding the head the record stored and whether it verified.
`abhed record verify` on another machine checks it with nothing else, as far
as the record could be checked here: lines after the stored head are noted,
not proved. An export whose trailer was removed is checked line by line, with a
note that lines cut from its end would not show.

A record that fails verification is not exported unless you pass
`-unverified`; the export is then marked, in the trailer or at the top of an
HTML or text export, and verifying the copy fails. A trailer without the mark
fails too. Like the trailer itself, the mark is not proof against whoever
holds the file, who can rewrite both.

An export never writes through a link, never over a file with a second name,
and never into Abhed's state other than `~/.abhed/exports`. A relative
`/export` path is taken from the workspace, and a path outside it asks first.
HTML and text exports are for reading, not for verification. Every export is
redacted, because the record it comes from is.

## Pruning

```
abhed record prune <session>        asks first (1 No, 2 Yes); -yes to skip the question
abhed record prune -older-than 90d
```

Pruning removes a session and the subagent sessions it started. Before
anything is removed, a tombstone goes into the index for each, with its id,
its head as stored (or as the index last recorded it, if the head is gone),
the time, who pruned it and why, whether its file was already missing, and
whether it verified. A damaged record can be pruned; its tombstone says so.
Checkpoint blobs that no remaining session names go with it.

- A session open in any Abhed process, this one included, is not pruned.
- Pruning is the only way anything leaves the record, and the tombstone keeps
  that visible.
- When the managed configuration sets `record.retention_days`, sessions last
  used longer ago than that are pruned when the record is opened. Each gets a
  tombstone, and each is also named on the terminal.
