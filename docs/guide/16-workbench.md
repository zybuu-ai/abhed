# The workbench

`abhed serve` has two front ends. `/console` is a conversation. `/ide` is a
workbench: the agent on the right, and on the left everything it is working
on, laid out the way an editor is.

```
┌ activity ┬ side panel ─────┬ editor tabs ──────────────┬ agent ──────────┐
│ sessions │ Explorer         │ retry.go  ·  diff retry.go │ conversation    │
│ explorer │ Search           │ src › retry.go     Ln 4    │ tool calls      │
│ search   │ Changes          │ code, or a diff to review  │ approvals       │
│ changes  │ Tools            │                            │ the plan        │
│ tools    │ Extensions       ├────────────────────────────┤ composer        │
│ ext.     │ HawkEYE          │ Terminal · Problems · Events│                 │
│ hawkeye  │                  │                            │                 │
└──────────┴──────────────────┴────────────────────────────┴─────────────────┘
  status: connection · sandbox tier · mode · model · context meter · findings
```

| Where | What it shows |
|---|---|
| **Explorer** | the session's workspace; click a file to open it in the editor. New file, new folder, rename (F2) and delete are on its toolbar and right-click menu |
| **Search** | text or a regular expression across the workspace, with match case and whole word; results grouped by file, a click opens the line |
| **Changes** | files the agent changed or you saved, each opening for review: accept or reject change by change |
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
| ⌘P / Ctrl P, or ⌘K outside the editor | command palette: files, sessions, commands |
| ⇧⌘P / Ctrl Shift P | the same palette |
| F1, in the editor | the editor's own commands |
| ⌘S / Ctrl S | save the open file |
| ⌥⌘S / Ctrl Alt S | save every open file |
| ⌘W / Ctrl W | close the editor tab, where the browser passes the key on (see below) |
| Ctrl G | go to line |
| ⇧⌘O / Ctrl Shift O | go to a symbol in the file, for languages the editor understands |
| ⌘F, ⌥⌘F / Ctrl F, Ctrl H | find, and find and replace, in the file |
| ⇧⌘F / Ctrl Shift F | search the workspace |
| ⇧⌘E / Ctrl Shift E | Explorer |
| ⌘L / Ctrl L with code selected | quote the selection, with its file and lines, into the composer |
| ⌘L / Ctrl L | focus the agent |
| ⌘B / Ctrl B | side panel |
| ⌘J / Ctrl J | bottom panel |
| Ctrl \` | terminal, on every platform |

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
delivers a queued message carries the same id as `queue_id`, and a
`client_id` sent with any message comes back on its `user.message` too.
Posting with `"interrupt": true` is Send now. On a server with node routing,
the queue routes and `/interrupt` answer `421` with `Abhed-Session-Node` for
a session running on another node. Streamed reasoning is recorded as
`agent.reasoning.delta` events; `agent.reasoning` still follows with the whole
text, so a reader that ignores the parts is unaffected.

`@path` in the composer attaches that file's content to the message, with
completion from the workspace tree as you type. Select code in the editor and
press ⌘L to ask about exactly those lines.

Panels resize by dragging the edges, and the layout is remembered per browser.

## The editor

The editor is Monaco, the editor engine behind the most widely used desktop
code editor, with its default keybindings. Multiple cursors (⌥-click, ⌘D,
⌥⌘↑/↓), the minimap, find and replace, folding, bracket matching and
colouring, sticky scroll, and syntax highlighting for some eighty languages
all work as they do there. JSON, CSS, HTML, JavaScript and TypeScript also get
completion, diagnostics and the symbol outline from language services that run
in the browser. The path of the open file is shown above it, with the cursor's
line and column.

Each tab keeps its own undo history, cursor and scroll. A tab with unsaved
edits is marked, closing it takes a second click or keypress, and the page asks
before it is closed or reloaded. The open tabs come back after a reload, per
session, in this browser. Light and dark follow the page.

The browser keeps ⌘W / Ctrl W for itself in an ordinary tab, so it closes the
editor tab only where the key reaches the page, such as the workbench installed
as an app; the palette's *Close the editor tab* works everywhere, and unsaved
edits are guarded either way.

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
before the session first changed it, side by side or inline (the review bar
switches between them, and the arrows step through the changes). Each change
has its own *Accept* and *Reject*. *Reject* puts the earlier text back for that
change, which is a save: it goes through the same call as any other and is
recorded as your write. *Accept* keeps the change and moves the file's
baseline, so it stops showing as a change and `/undo` returns to it; the
acceptance is recorded as `change.accepted`. The baseline only moves to text on
disk, so accepting in a tab with unsaved edits saves them first. Rejecting
everything in a file the session created deletes the file, as the Explorer
does.

**Explorer.** A new file is a save of an empty file, refused if the name is
taken. New folder, rename and delete are each one command, `mkdir -p`,
`mv -n` or `rm`, run through the same call as a line typed into the terminal,
so the bash rules, the sandbox and the record apply to it. Before it runs,
every path it touches must be one the workbench would open, which rules out
`.abhed/`, `.git/` and anything a read rule withholds, and one a save to which
would not be refused by a write rule. Each path is judged as named and with
its folder's links followed, since that is where the command acts. For a
folder, the same holds for everything inside it, up to 5,000 entries, and a
rename judges each entry at its old path and at its new one: a folder cannot
be moved or deleted if anything in it could not be, and a read-denied file
cannot reappear under another name. Rename never replaces what is already at
the new name, and deleting a link removes the link, not what it points to.
The checks and the command run together, with no other action of yours in
between, and the command finishes even if the page is closed; an edit the
agent makes at that moment is not held back, as it is not for a command in
the terminal.

**Search.** Search reads what the Explorer shows and nothing more: a folder or
file a read rule withholds is not opened, and neither is `.abhed/`, `.git/` or
a folder the `grep` tool passes over (`node_modules`, `vendor`, `dist`, `.venv`
and the like). Binary files are skipped. A regular expression is RE2, as in
`grep`. A search stops at 2,000 results, 20,000 files or five seconds and
says which; a file with more than 100 matches is listed with its first 100 and
marked. A line is shown as at most 240 bytes around the match. A session runs
at most two searches at once. A search changes nothing, so it is not
recorded.

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
- **No go to definition, hover or diagnostics from a language server.** The
  browser's own language services cover JSON, CSS, HTML, JavaScript and
  TypeScript; other languages are highlighted only. See below for where a
  language server will attach.

## What it is built on

Nothing in the page is a mock. The editor is Monaco (MIT) and the terminal is
xterm.js, built into the binary under their own licences (`server/ide/vendor/NOTICE`)
so the page still loads nothing from the network; `web/ide/` rebuilds them.
Monaco's workers are files served beside it from `/ide/vendor/`, so the page's
content security policy needs no `blob:`, `worker-src` or `unsafe-eval`; the
one addition is `font-src 'self'`, for the editor's icon font. Each worker is
served with a policy of its own, `default-src 'none'; script-src 'self'`, so
code that reads untrusted file content can load nothing and call nowhere. The
components are kept gzipped in the binary and sent that way, about 3.5 MB in
all; a client that does not accept gzip gets them unpacked. Half of that is
the TypeScript service, which loads only when a JavaScript or TypeScript file
is opened.
Tools, rules, extensions, MCP servers and skills
come from `GET /v1/capabilities`, which any signed-in user may read. An
extension's name and events are listed; its command line and environment are
not, because those are the operator's and may hold credentials. File content,
tool output and session titles are untrusted, and the page only ever inserts
them as text.

## Language servers

Go to definition, hover and diagnostics for other languages need a language
server, which is not wired in yet. The seam is in `web/ide/editor.js`:

```js
AbhedEditor.registerLanguageServer("go", {
  attach(model, editor) { /* connect; return {dispose()} */ },
});
```

The workbench calls `attachLanguageServer` for every file it opens and
disposes what it returns when the tab closes. Nothing is registered today, so
it is a no-op. The server half, running the language server in the session's
sandbox and carrying its messages over the session's own authenticated
connection, comes with the first provider.
