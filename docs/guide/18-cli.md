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
| `-p`, `-output-format`, `-input-format`, `-no-stdin`, `-json-schema`, `-include-partial-messages`, `-verbose` | headless runs: see [Automation](10-automation.md#headless) |

The familiar spellings `-permission-mode`, `-allowedTools`,
`-disallowedTools` and `-dangerously-skip-permissions` are accepted too; the
table of what each maps to is in [Automation](10-automation.md#familiar-flag-spellings).

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
A `tool_search` tool finds them by what they do and loads the ones it
returns, so a server of two hundred tools does not fill the context. A
loaded tool is policed like any other: it asks unless a rule allows it, and
its output is untrusted.

## Help

| Command | |
|---|---|
| `/doctor` | the configuration, trust, the sandbox, and whether the model answers and calls tools |
| `/release-notes [VERSION\|all]` | the changelog built into this binary, offline |
| `/bug [what happened]` | a prefilled issue link, with the version, platform and provider type, and with the secrets store's values, the provider's key and your home directory redacted; nothing is sent, and it is opened only if you say so |
