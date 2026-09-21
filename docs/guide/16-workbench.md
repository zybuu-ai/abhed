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
| **Explorer** | the session's workspace; click a file to open it in a tab, edit it, and save with ⌘S / Ctrl S |
| **Changes** | files the agent changed or you saved, each opening as a diff tab |
| **Tools** | every tool the agent has, whether it asks before running, and the permission rules in force |
| **Extensions** | configured extensions and the events they hook, connected MCP servers, loaded skills |
| **HawkEYE** | the session's totals and [findings](15-hawkeye.md), live |
| **Terminal** | each command the agent ran, and a line to run your own in the same sandbox |
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
| ⌘B / Ctrl B | side panel |
| ⌘J / Ctrl J | bottom panel |
| ⌘L / Ctrl L | focus the agent |

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
| Shell commands | run in the session's sandbox | run in the same sandbox: no network unless the operator allowed it |
| The record | every call, decision and result | the same events, marked as yours (`actor: user`, `by: user`) |

So HawkEYE's report covers what people did as well as what the agent did, and a
save shows up under **Changes** with a diff like any other edit.

A save is refused if the file changed since you opened it, so you cannot write
over an edit the agent made in the meantime; reload and try again. Your terminal
keeps its own working directory, so a `cd` there never moves the agent.

Both need a live session, because that is what holds the sandbox. A session
from before a restart has to be resumed with a message first.

## What it is not, yet

- **The terminal runs commands; it is not a shell.** There is no tty and no
  input, so `vim`, `top` or a prompt that waits for an answer will not work,
  and output arrives when the command ends.
- **No syntax highlighting, and a plain text area to edit in.** A real editor
  component has to be vendored into the binary: the page loads nothing from
  anywhere, so that it opens on an air-gapped network.
- **Large files are read-only.** A file the viewer truncates cannot be saved
  without losing the rest, and a save is limited to one megabyte.
- **No accepting or rejecting a change hunk by hunk.**
- **Changes** covers saves and edits made with `write` and `edit`. A file
  changed by a shell command, yours or the agent's, does not appear there.

## What it is built on

Nothing in the page is a mock. Tools, rules, extensions, MCP servers and skills
come from `GET /v1/capabilities`, which any signed-in user may read. An
extension's name and events are listed; its command line and environment are
not, because those are the operator's and may hold credentials. File content,
tool output and session titles are untrusted, and the page only ever inserts
them as text.
