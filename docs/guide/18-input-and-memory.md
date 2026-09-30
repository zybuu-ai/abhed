# Input, memory and custom commands

What you type at the interactive prompt is more than a message. `@` attaches
files, `!` runs a shell command, `#` saves a note, and a `/` name can be a
command of your own. Each of these acts as you, and goes through the same
policy, sandbox and record as the agent's own calls. Nothing you type is
pre-approved.

## Attaching files with @

```
explain @internal/agent/loop.go:120-180
what is in @docs/
```

- `@path` attaches a file. A relative path is the workspace's; `~/` is your
  home directory.
- `@path:10-20`, `@path:10` and `@path#L10-20` attach those lines only.
- `@dir/` attaches the directory's file list, not its contents.
- `@"a file with spaces.md"` quotes a path.

A mention is read by the read tool as your own call. So the workspace
boundary holds, a link that leads out of the workspace is refused, Abhed's
own state (`.abhed`, `~/.abhed`) is never attached, read deny rules apply
with the rule shown, and secrets are redacted. If a mention names a file
that may not be read, the message is not sent, and you are told why. A
mention of something that does not exist, such as `@alice`, stays as text.

A file is attached up to 256 KB, and the rest is noted. Each attachment is
recorded as `input.mention` with the SHA-256 of what was attached.

## Running a command with !

```
!git status
!go test ./internal/agent/
```

The line runs through the bash tool as your call: in the sandbox, under your
deny rules, and refused in plan mode. A destructive command such as
`rm -rf build` asks first, and no answer refuses it. The call is recorded as
yours (`by: user`) with its output.

The output is shown, 40 lines at most, and all of it (up to 30,000
characters, redacted) joins your next message, so the agent sees what you
ran and what came back.

## Saving a note with #

```
# run go vet before committing
```

You choose where the note goes:

| Choice | File | For |
|---|---|---|
| Project | `ABHED.md` | the team: it is committed with the repository |
| Local | `ABHED.local.md` | you, in this project |
| User | `~/.abhed/ABHED.md` | you, in every project |

A note in the workspace is written as your write call, so write rules and
plan mode apply and `/undo` reverts it. The note is redacted, and recorded
as `memory.written`. `/memory add <project|local|user> <note>` does the same
without the question.

A `!` or `#` line typed while the agent is working waits until the turn
ends, as a slash command does.

## Memory

Memory files are read into the system prompt of every session, in this
order:

| Scope | File |
|---|---|
| User | `~/.abhed/ABHED.md` |
| Project | `ABHED.md` in the workspace, or `AGENTS.md` where there is no `ABHED.md` |
| Local | `ABHED.local.md` in the workspace |
| Rules | the rule files in `rules.dirs` (below) |
| Auto | the agent's own notes, when auto memory is on (below) |
| Managed | `/etc/abhed/ABHED.md`, last, so nothing after it overrides it |

`AGENTS.md` is read only in a directory that has no `ABHED.md`, and
`/memory` labels it so. No other tool's files are read. To bring one in, use
`/import <path>`: it shows the file and appends it to `ABHED.md` only if you
confirm.

A workspace memory file came with the repository, so it is read as the file
tools read: never through a link out of the workspace or into Abhed's state,
and not where a read deny rule forbids it. A file over 4 MiB is skipped.

### Imports

A memory file can pull in another with `@path` on its own or after a space:

```markdown
See @docs/conventions.md for style.
```

The path is relative to the file that names it. Imports are followed
`memory.import_depth` levels deep (5 by default, 10 at most), and a file
already loaded is not loaded again, so a cycle ends. An import from a
workspace file must stay in the workspace; one from your own or the
organisation's file may also reach `~/.abhed` or `/etc/abhed`. Text inside a
code fence is not an import, and neither is `@alice`.

### Rules

`rules.dirs` names directories of rule files. A rule file is markdown; a
`paths` header scopes it to matching files:

```markdown
---
paths: ["**/*.go"]
---
Wrap errors with %w.
```

```json
"rules": { "dirs": [".abhed/rules", "~/rules"] }
```

A relative directory is the workspace's. Setting `rules.dirs` in a
workspace's `.abhed/config.json` takes effect only once the workspace is
trusted. Rules are in the prompt with their scope, so the agent applies a
scoped rule when it works on matching files.

### /memory

`/memory` lists every file, its scope, size and hash, and why any file was
not loaded. `/memory show <n>` prints one. Each conversation records
`memory.loaded` with the path, scope and SHA-256 of every file its prompt
carries.

### Auto memory

Auto memory lets the agent keep short notes between sessions. It is off
unless you turn it on:

```
/memory auto on
```

or in `~/.abhed/config.json`:

```json
"memory": { "auto": true }
```

A managed value binds, and a workspace may only turn it off. When it is on,
the agent has a `memory_write` tool that saves a `user`, `feedback`,
`project` or `reference` note to `~/.abhed/projects/<id>/memory/MEMORY.md`.
Text the agent reads could try to make it save something, so:

- a save is a change, judged and asked as one (allow `memory_write` to stop
  the question);
- its text is redacted;
- it is shown as it happens, and recorded as `memory.written` by the agent,
  marked untrusted;
- later sessions load the notes (the first 200 lines or 25 KB) labelled as
  the agent's notes, not your instructions.

`MEMORY.md` is yours to edit or delete.

## Custom commands

A custom command is a markdown file whose name is the command:

```markdown
---
description: Review a file for correctness bugs
argument-hint: <path>
allowed-tools: [read, grep, glob]
---
Review @$1 for correctness bugs. Report each with file:line.
Current status: !`git status --short`
```

Saved as `~/.abhed/commands/review.md`, it runs as `/review src/app.go`.
A file in a subdirectory, `frontend/test.md`, is `/frontend:test`.

| Header key | |
|---|---|
| `description` | shown in `/help` and `/commands` |
| `argument-hint` | shown after the name |
| `allowed-tools` | the tools the command's turn may use: a subset of the session's, never an allow rule; a tool the session lacks refuses the command |
| `model` | a configured provider the turn runs on; the session switches back afterwards |

Any other key refuses the command: a command cannot change the mode, add
rules, or touch the sandbox.

In the body, `$ARGUMENTS` is everything after the name, and `$1` to `$9` are
the words (quote a word with spaces). A body that names no argument gets the
arguments appended. `@path` attaches a file as `@` does at the prompt.
`` !`command` `` runs a shell line and puts its output in the text; every
such line asks first, since you typed the command's name, not the line, and
the policy decides as for `!`.

The text runs as your next turn. The record holds `command.invoked` with the
name, source, the file's SHA-256 and your arguments, redacted.

### Where commands come from

| Source | Directory | Loads |
|---|---|---|
| Managed | `/etc/abhed/commands` | always; its names cannot be taken |
| User | `~/.abhed/commands`, and `commands.dirs` | always |
| Workspace | `.abhed/commands` | only once you trust exactly this content |

A command that came with a repository is instructions to the agent. So the
workspace's commands are listed but do not run until you trust them:
`/commands trust` shows each file and its hash and asks. The decision is
kept in `~/.abhed/command-trust.json` for exactly that content; a change to
any file needs trust again. Starting with `--trust`, or with the workspace
trust variable set, trusts them for that run; a workspace started untrusted
never runs them.

Built-in commands always win: a custom command cannot use a built-in's name,
or one that looks like it. A workspace command cannot take a name your own
or the organisation's commands use. `/commands` lists them all;
`/commands show <name>` prints one.

## /init

`/init` asks the agent to write `ABHED.md` from what the repository shows:
what the project is, the exact build and test commands, the layout, and its
stated conventions. It writes through the write tool, so you approve the
change as any other. If `ABHED.md` exists, it is improved rather than
replaced. Add a note after the name to steer it: `/init keep it under 50
lines`.

## /context and /compact

`/context` shows how full the context window is and with what: the system
prompt, memory files, built-in tools, MCP tools and the messages, each in
tokens and as a share of the window.

`/compact` summarizes the conversation now. `/compact <focus>` tells the
summary what to keep above all else:

```
/compact keep the API names and the failing test
```

## Questions from the agent

In an interactive session the agent has an `ask_user` tool for a decision
that is yours: it offers two to six choices, and you pick one or none. It is
a question, not an approval: the answer grants nothing, and tool calls are
still approved on their own. With no one to answer, such as over a pipe or
in `-p`, the agent is told so and decides itself; it is never answered for
you.

## Output styles

An output style tells the agent how to write its answers. A style is a
markdown file in `~/.abhed/styles` (or the organisation's `/etc/abhed/styles`,
whose names win), with an optional `description` header.

```
/output-style            list them
/output-style terse      use one for the rest of the session
/output-style off        stop
```

A workspace's styles are not read.
