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
| **Explorer** | the session's workspace; click a file to open it in a tab |
| **Changes** | files the agent changed, each opening as a diff tab |
| **Tools** | every tool the agent has, whether it asks before running, and the permission rules in force |
| **Extensions** | configured extensions and the events they hook, connected MCP servers, loaded skills |
| **HawkEYE** | the session's totals and [findings](15-hawkeye.md), live |
| **Terminal** | each shell command the agent ran, its output and exit code |
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
| ⌘B / Ctrl B | side panel |
| ⌘J / Ctrl J | bottom panel |
| ⌘L / Ctrl L | focus the agent |

Panels resize by dragging the edges, and the layout is remembered per browser.

## What it is not, yet

- **It does not edit.** Files and diffs are read-only. A person changing the
  workspace from the browser is an action that belongs in the audit record,
  and how it is recorded is not settled. Accepting or rejecting a change hunk
  by hunk comes with that.
- **The terminal is the agent's, not yours.** It shows what the agent ran. It
  is not a shell.
- **No syntax highlighting.** That needs an editor component, which has to be
  vendored into the binary: the page loads nothing from anywhere, so that it
  opens on an air-gapped network.
- **Changes** covers edits made with `write` and `edit`. A file changed by a
  shell command does not appear there.

## What it is built on

Nothing in the page is a mock. Tools, rules, extensions, MCP servers and skills
come from `GET /v1/capabilities`, which any signed-in user may read. An
extension's name and events are listed; its command line and environment are
not, because those are the operator's and may hold credentials. File content,
tool output and session titles are untrusted, and the page only ever inserts
them as text.
