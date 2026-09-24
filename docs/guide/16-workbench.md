# The workbench

`abhed serve` has two front ends. `/console` is a conversation. `/ide` is a
workbench: the agent on the right, and on the left everything it is working
on, laid out the way an editor is.

```
┌ activity ┬ side panel ─────┬ editor tabs ──────────────┬ agent ──────────┐
│ sessions │ Explorer         │ retry.go  ·  diff retry.go │ conversation    │
│ explorer │ Changes          │                            │ tool calls      │
│ changes  │ Tools            │ code, or a unified diff    │ approvals       │
│ tools    │ Extensions       ├────────────────────────────┤ the plan        │
│ ext.     │ HawkEYE          │ Terminal · Problems · Events│ composer        │
│ hawkeye  │                  │                            │                 │
└──────────┴──────────────────┴────────────────────────────┴─────────────────┘
  status: connection · sandbox tier · mode · model · context meter · findings
```

| Where | What it shows |
|---|---|
| **Explorer** | the session's workspace; click a file to open it in an editor with syntax highlighting, and save with ⌘S / Ctrl S |
| **Changes** | files the agent changed or you saved, each opening for review: accept or reject chunk by chunk |
| **Tools** | every tool the agent has, whether it asks before running, and the permission rules in force |
| **Extensions** | configured extensions and the events they hook, connected MCP servers, loaded skills |
| **HawkEYE** | the session's totals and [findings](15-hawkeye.md), live |
| **Terminal** | a terminal in the session's sandbox: each line you type is one recorded, policy-checked command, and the agent's commands appear in it too |
| **Problems** | HawkEYE's findings, the way an editor lists diagnostics |
| **Events** | the raw record as it is written, untrusted events marked |

The agent panel works the way a terminal agent does. A call that needs a person
stops and asks: **Allow once**, **Always allow** the narrow rule the policy
suggests, or **Deny**. The plan the agent keeps is drawn as a checklist. `/` in
the composer opens commands — `/changes`, `/tools`, `/hawkeye`, `/stop` and the
rest — and the permission mode is chosen beside it.

| Key | |
|---|---|
| ⌘K / Ctrl K | command palette: files, sessions, commands |
| ⌘S / Ctrl S | save the open file |
| ⌘L / Ctrl L with code selected | quote the selection, with its file and lines, into the composer |
| ⌘B / Ctrl B | side panel |
| ⌘J / Ctrl J | bottom panel |
| ⌘L / Ctrl L | focus the agent |

## The conversation

A message shows the moment you send it, and a *Thinking…* line with a
seconds count stays under it until the first word of the reply. When the model
reports its reasoning, it streams into an open **Thinking** block that closes
when the reply starts, and reads *thought for 6s · 120 words* afterwards; one
click opens it again. While a tool runs the line says *Running bash…*, and
*Compacting context…* while the conversation is summarised.

Replies stream as they are written and are drawn as markdown once complete:
headings, lists, tables, code blocks and links. The text is built into the page
as text, never as markup, and only `http`, `https` and `mailto` links are made
clickable. The panel follows the reply only while you are at the bottom of it;
scroll up and it stays where you are, with **Jump to latest** to come back. A
tool call's arguments and output are drawn when you open it, and long output
shows its first lines until you ask for the rest.

**Sending while the agent works.** Send stays enabled during a run. A message
sent then is not a reason to cancel the step in progress: it is queued, shown
below the conversation as *Queued — will be read at the next step*, and the
agent reads it at the next turn boundary, after the call in flight finishes.
Until then it has two actions:

| | |
|---|---|
| **Send now** | stops the running step and sends the message as a fresh turn |
| **Cancel** | withdraws it; the agent never sees it |

When the agent reads it, the bubble joins the conversation at that point. A
message still queued when a run stops is read with your next one. Esc stops a
run only when pressed twice, so a stray key never costs work; the **Stop**
button stops it at once.

If the connection drops, the page reconnects and asks only for what it has
not drawn yet, rather than replaying the session.

| Key | In the composer |
|---|---|
| Enter | send, or queue while the agent works |
| Shift+Enter | new line |
| Esc, twice | stop the running turn |

The same queue is available to any client: a message posted to a busy session
answers `202` with `"delivery": "steered"` and a `queue_id`,
`GET /v1/sessions/{id}/queue` lists what is waiting, and
`DELETE /v1/sessions/{id}/queue/{qid}` withdraws one. The `user.message` that
delivers a queued message carries the same id as `queue_id`. Posting with
`"interrupt": true` is Send now. Streamed reasoning is recorded as
`agent.reasoning.delta` events; `agent.reasoning` still follows with the whole
text, so a reader that ignores the parts is unaffected.

`@path` in the composer attaches that file's content to the message, with
completion from the workspace tree as you type. Select code in the editor and
press ⌘L to ask about exactly those lines.

Panels resize by dragging the edges, and the layout is remembered per browser.

## Working by hand

You can edit a file and run a command yourself, beside the agent. Neither is a
side door. Both go through the same call the agent's tools go through:

| | The agent | You, in the workbench |
|---|---|---|
| Deny rules | refuse | refuse — `shutdown` is denied for you as it is for the agent |
| Ask rules and mutating tools | a person is asked | taken as answered: you are the person |
| Workspace boundary | cannot leave it | cannot leave it |
| `.abhed/`, `.git/`, anything a read rule withholds | not served | cannot be opened or written |
| Commands | run in the session's sandbox | each line runs in the same sandbox, on a terminal: no network unless the operator allowed it |
| The record | every call, decision and result | the same events, marked as yours (`actor: user`, `by: user`) |

So HawkEYE's report covers what people did as well as what the agent did, and a
save shows up under **Changes** with a diff like any other edit.

A save is refused if the file changed since you opened it, so you cannot write
over an edit the agent made in the meantime; reload and try again.

**Review.** A changed file opens as the text it holds now against what it held
before the session first changed it, chunk by chunk. *Reject* puts the earlier
text back for that chunk, which is a save: it goes through the same call as
any other and is recorded as your write. *Accept* keeps the chunk and moves the
file's baseline, so it stops showing as a change and `/undo` returns to it; the
acceptance is recorded as `change.accepted`. Rejecting everything in a file the
session created removes the file, through a recorded `rm`.

**Terminal.** The terminal is a real one — `vim`, `top`, a program that asks a
question all work — but it is not a shell. Each line you enter is judged as a
`bash` call before it runs, so a deny rule stops it there, and it runs on its
own pseudo-terminal in the session's sandbox. Shell state does not carry from
one line to the next: `cd` is followed, `export` is not. The output of each
command is in the record with the control sequences stripped; what you typed
is not recorded separately, since a terminal echoes it into the output unless
the program turned echo off, which is when it should not be kept. A command
nobody has watched for two minutes is ended. The terminal keeps its own working
directory, so a `cd` there never moves the agent.

Both need a live session, because that is what holds the sandbox. A session
from before a restart has to be resumed with a message first.

## What it is not, yet

- **Shell state does not persist between lines.** Every line is its own
  command, which is what makes every line a policy decision.
- **Files over 4 MB are read-only**, shown in part.
- **Changes** covers saves and edits made with `write` and `edit`. A file
  changed by a shell command, yours or the agent's, does not appear there.
- **No completion or diagnostics** from a language server.

## What it is built on

Nothing in the page is a mock. The editor is CodeMirror and the terminal is
xterm.js, built into the binary under their own licences (`server/ide/vendor/NOTICE`)
so the page still loads nothing from the network; `web/ide/` rebuilds them.
Tools, rules, extensions, MCP servers and skills
come from `GET /v1/capabilities`, which any signed-in user may read. An
extension's name and events are listed; its command line and environment are
not, because those are the operator's and may hold credentials. File content,
tool output and session titles are untrusted, and the page only ever inserts
them as text.
