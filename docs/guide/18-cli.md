# The command line

`abhed` with no command opens an interactive session in the current
directory. This page lists its flags and the commands that show and change
the session. Running without a terminal is in [Automation](10-automation.md#headless);
the first run is in [Getting started](01-getting-started.md).

## Starting

```bash
abhed                          # a session in this directory
abhed -C ~/src/app             # in another workspace
abhed "fix the failing tests"  # a session that starts with this task
abhed -- fix the failing tests # the same, without quotes
abhed -p "fix the failing tests"  # one task, headless, then exit
```

A single word that is not a command is refused with a hint, since it is
more often a mistyped command than a task.

| Flag | |
|---|---|
| `-C DIR` | the workspace (default: the current directory) |
| `-model NAME` | a configured provider for this run |
| `-fallback-model A,B` | configured providers to move to when the model is unavailable |
| `-mode MODE` | `default`, `accept-edits`, `plan`, `auto` or `bypass` |
| `-allow RULES`, `-deny RULES` | rules for this run, e.g. `bash(go test*)` |
| `-add-dir DIRS` | more directories the agent may read and write |
| `-max-turns N`, `-max-budget-tokens N` | limits for this run; a managed limit may only be lowered |
| `-append-system-prompt TEXT` | instructions added to the system prompt |
| `-system-prompt TEXT` | the system prompt replaced; refused under a managed configuration |
| `-trust-workspace` | trust the workspace's `.abhed/config.json` for this run |
| `-settings FILE` | a settings file, or inline JSON, for this run; see below |
| `-mcp-config FILE`, `-strict-mcp-config` | MCP servers for this run, and whether they are the only ones; see below |
| `-agents JSON`, `-agent NAME` | subagent definitions for this run, and a role to run the session as; see below |
| `-p`, `-output-format`, `-input-format`, `-no-stdin`, `-json-schema`, `-include-partial-messages`, `-verbose` | headless runs: see [Automation](10-automation.md#headless) |

### Moving between sessions

```bash
abhed sessions                 # this workspace's sessions, latest active first (abhed record list)
abhed -r                       # pick one to resume
```

Inside a session, `/resume` or `/switch` alone opens the picker: type to
filter it, Enter opens the first match, and the session you are in is marked
`● current`. The footer names the session you are in. The details are in
[Sessions and the local record](12-records.md#starting-resuming-and-naming).

The familiar spellings `-permission-mode`, `-allowedTools`,
`-disallowedTools` and `-dangerously-skip-permissions` are accepted too; the
table of what each maps to is in [Automation](10-automation.md#familiar-flag-spellings).

### Settings, servers and roles for one run

Each of these takes a file, or JSON written inline (a value starting with
`{`), and the start of the session's record names the source and the
SHA-256 of what was read. They behave the same with and without `-p`. A
file inside the workspace is something the agent could have changed since
you last read it, so, like the workspace's own configuration, it is read
only with `-trust-workspace` (or the workspace trust variable); otherwise
the run stops with exit code 2. A file outside the workspace is trusted as
your own, even where an agent could have written it, such as the sandbox's
temporary folder, so name only files you wrote or have read.

- **`-settings`** is merged over your own `~/.abhed/config.json`, as if it
  were part of it: a workspace's trusted file and the managed file are still
  laid over it, a managed-only setting in it is ignored with a warning, and
  when the managed file sets any `permissions` setting its allow rules are
  left out, as yours are. `/permissions` shows its rules as `settings`.
- **`-mcp-config`** (repeatable) adds MCP servers, in the
  `{"mcpServers": {"name": {"command": …}}}` shape or Abhed's
  `{"mcp": {"servers": […]}}`. Naming the file is your consent, so each of its
  servers is enabled unless it says `"enabled": false`; a server with the
  name of a configured one replaces it for the run. With
  `-strict-mcp-config` only these servers start. When the managed
  configuration sets the `mcp` section, `-mcp-config` is refused and
  `-strict-mcp-config` leaves the organisation's servers as they are.
- **`-agents`** gives subagent definitions as
  `{"name": {"description": …, "prompt": …, "tools": […], …}}`, with the keys
  of a definition file and `prompt` for its instructions. Each is checked
  exactly as a file is ([Agent definitions](17-agent-definitions.md)), and
  one that is refused ends the run with exit code 2. They take a name over
  the workspace's and your own definitions, never over the organisation's.
- **`-agent NAME`** runs the session as that agent type: its instructions
  join the system prompt, and the session and the subagents it starts keep
  only its tools, MCP servers and skills. Its `permission_mode`, `effort`
  and `max_turns` apply where they narrow, and its `model` when `-model` is
  not given. A `worktree` role runs only as a subagent.

Start-up does not wait for the container runtime or the model endpoint: both
are checked behind the prompt. An endpoint that is down is named with what
to do.

## The model

| Command | |
|---|---|
| `/model` | pick a configured model: its id, context window, and whether it is local |
| `/model NAME` | switch to a configured provider, keeping the conversation; recorded |
| `/effort low\|medium\|high` | the reasoning effort, where the provider takes one |
| `/effort on\|off` | thinking on or off, where the provider has the switch |
| `/effort default` | back to the provider's own setting |

`/model` offers only configured providers, never an endpoint typed at the
prompt, and a managed `model.default` is not switched.

### Fallback

```json
{"model": {"default": "main", "fallback": ["backup"], "providers": {"main": {}, "backup": {}}}}
```

When the model is unreachable or refuses access (401, 403, 404, 429 or a
server error after the retries), the session moves to the next configured
provider in `model.fallback` or `-fallback-model`, says so, and records a
`model.fallback` event with the reason. A request the model rejects for its
content never moves. A managed `model.default` is left only when the managed
configuration names the fallbacks too, and then only for those: a
`-fallback-model` is ignored with a warning. `-model` naming another
provider is refused under a managed `model.default`, as `/model` is.

## Status

| Command | |
|---|---|
| `/status` | model, mode, workspace and branch, sandbox, record, session, context use, the turn limit and what it counts, token budget, background tasks, trust, managed settings |
| `/usage` (or `/cost`) | tokens, cache hit rate, prefill saving, compactions, and a breakdown by subagent and by tool source |
| `/todos` | the agent's task list as it stands; Ctrl-T lists the open items above the input — [The terminal](20-terminal.md#what-the-harness-does) |
| `/copy` | put the last reply on the clipboard through the terminal (OSC 52); `cli.copy: false` turns it off — [The terminal](20-terminal.md#the-title-notifications-and-the-clipboard) |
| `/config` | each setting and where it comes from: managed, a file, or the default |
| `/config set KEY VALUE` | set `model.default`, `permissions.mode`, `sandbox.allow_network`, `limits.max_turns`, `tools.syntax_check` or `statusline.command` in your own `~/.abhed/config.json`, for the next session |

`/config set` refuses a setting the managed configuration makes, and asks
before a change that lets the agent do more: a broader mode, network on
(in any spelling, such as `1` or `T`), a higher or no turn limit, a looser
syntax check, a statusline command, or a default model that is hosted
rather than on this machine, since the code is sent to it.
"More" is judged against your own file, or the default where it says
nothing, not against the session: a session already in bypass from a flag,
or with network on from a trusted workspace, still asks before writing
either into your file, and no answer writes nothing.

### A status line of your own

```json
{"statusline": {"command": "~/bin/abhed-status.sh"}}
```

The command reads the session's status as one JSON object on stdin:

```json
{"model":"qwen3-coder:30b","provider":"local","mode":"default","mode_locked":false,
 "context_tokens":5120,"context_percent":8,"tokens_in":20480,"tokens_out":900,
 "background_tasks":0,"waiting_ask":false,"sandbox_tier":"process","network":false,
 "record":"local","git_branch":"main","cwd":"~/src/app"}
```

`record` is the store the session writes to: `local` for the local record,
`unverified` for Postgres, which this process has not verified, and `memory`
when nothing outlives the process.

On a terminal its first line replaces the footer's second row, run again at
most once a second as the session changes; in a piped session it is printed
after each task; `/status` shows it too. It runs under the
process sandbox with the network off, whatever the session's tier or
`sandbox.allow_network`, for at most 300 ms. Where the process sandbox is
not available it does not run at all: the status line is empty and one
warning says why. When the command starts with a script named by path (`~/…` or
absolute), such as the example above, that file is checked once, when the
session starts, and pinned:

- It must be an executable file somewhere the agent cannot change it, such
  as `~/bin` or `/usr/local/bin`. A script in the workspace, in a folder
  granted to the agent's tools (`-add-dir`, `additional_dirs`, a skill's
  folder), in a temp or toolchain cache folder, in `~/.abhed` (skills
  included), in the workspace's `.abhed`, or in a configured state path is
  refused, whether it is named there or a link resolves there. The status
  line then shows the reason, on every redraw, and nothing else.
- The file it resolves to is what runs, and it is shown to the sandbox
  read-only; on Linux, where the sandbox does not show your home directory,
  that one file is bound in.
- Before each run it must still be the same file, unchanged: the same inode,
  size, modification and change times. Replaced, edited, or swapped for a
  link, a folder or another file, it is not run, the allow goes with it, and
  the status line says so; start a new session to use the script as it is
  now.

What the script itself reads must be visible to the sandbox too: the
workspace and the system directories are, while on Linux the rest of your
home directory is not. Only
text and colour reach the terminal: a sequence that would move the cursor,
clear the screen, set the title or write the clipboard is dropped. In a
workspace's configuration it needs trust, like any other process.

## Tools, agents and servers

| Command | |
|---|---|
| `/tools` | every tool the agent has, where it came from, and whether it changes things |
| `/agents` | the subagent types `task` can start, by source, with their model and tools |
| `/skills` | the skills the agent can load |
| `/mcp` | each MCP server: connected or why not, and its tools |
| `/mcp restart NAME` | reconnect a server; its tools reach the new connection |

With more than 40 MCP tools, they are not listed to the model one by one.
A `tool_search` tool, whose description lists the servers and their tool
names, finds them by name or by what they do and loads the ones it returns, so a server of two hundred tools does not fill the context. A
loaded tool is policed like any other: it asks unless a rule allows it, and
its output is untrusted.

## Reviews

| Command | |
|---|---|
| `/review [BASE]` | review the current change for bugs, missing tests and maintainability |
| `/security-review [BASE]` | review the current change for security issues, with a severity for each |

Both read the change with `git status --short` and `git diff BASE` (default
`HEAD`, so staged and unstaged changes), run as your own shell command: the
policy decides, a deny rule holds, and the call and its output are in the
record. External diff and textconv drivers are off. `BASE` must be a plain
revision such as `main` or `HEAD~3`.

The review then runs as your next turn in plan mode, so the agent can read
files but change nothing; the mode you had comes back when the turn ends
(from bypass, the session stays in plan). The prompts are built into the
binary, and `command.invoked` records the command with the prompt's SHA-256.
The diff goes to the model marked as data, not instructions, capped at
30,000 characters. Plan mode refuses every shell command, so in plan mode
`/review` asks you to switch to default first. A custom command named
`review` or `security-review` is left out, with a notice.

## Help

| Command | |
|---|---|
| `/doctor` | the configuration, trust, the sandbox, and whether the model answers and calls tools |
| `/release-notes [VERSION\|all]` | the changelog built into this binary, offline |
| `/bug [what happened]` | a prefilled issue link, with the version, platform and provider type, and with the secrets store's values, the provider's key and your home directory redacted; nothing is sent, and it is opened only if you say so |
