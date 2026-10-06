# Configuration

Abhed reads `.abhed/config.json` from the workspace. `abhed init` writes a
starter file; everything below is optional and has a default.

The workspace file is untrusted until you trust its exact contents, because a
repository can ship one. Until then Abhed applies only what makes it stricter
and names each setting it ignored. See [Trusting the workspace
configuration](#trusting-the-workspace-configuration).

```json
{
  "model": {
    "default": "local",
    "providers": {
      "local": {
        "type": "ollama",
        "base_url": "http://127.0.0.1:11434/v1",
        "model": "qwen3-coder:30b",
        "context_window": 32768,
        "params": { "temperature": 0.2, "top_p": 0.9 }
      }
    }
  },
  "permissions": { "mode": "default" },
  "storage": { "driver": "memory" }
}
```

## Sections

| Section | What it controls |
|---|---|
| `model` | providers and which one is default — [Models](03-providers.md) |
| `permissions` | what runs unattended — [Permissions](04-permissions.md) |
| `context` | compaction threshold and memory files |
| `limits` | turn, token and subagent budgets |
| `sandbox` | process isolation and network access; `tier`: `fence` chooses the fence preview — [below](#the-fence-tier-preview-linux); `terminal`: whether the workbench terminal is a shell (`shell`, the default) or checks each line (`lines`) — [The workbench](16-workbench.md) |
| `fence` | `cpu_percent`: the fence tier's CPU bound — [below](#the-fence-tier-preview-linux) |
| `storage` | in-memory or Postgres |
| `auth` | who may use a server deployment |
| `skills` | where skills are loaded from — [Skills](06-skills.md) |
| `agents` | where subagent definitions are loaded from, or `disabled` — [Agent definitions](17-agent-definitions.md) |
| `subagents` | what a background task's result does while the session is idle — [Parallel subagents](14-parallel-subagents.md#background-tasks) |
| `extensions` | processes that can intercept — [Extensions](07-extensions.md) |
| `mcp` | Model Context Protocol servers — [MCP](08-mcp.md) |
| `custom_providers` | providers added without a rebuild |
| `web_search` | whether the agent can search the web, with which provider — managed only, [below](#web-search) |
| `web_fetch` | whether the agent can read a web page, and from which hosts — managed only, [below](#web-fetch) |
| `retrieval`, `rag` | the local index, and external corpora |
| `k8s`, `ssh` | infrastructure tools, off by default; `k8s.clusters` names the only servers `k8s_login` sends a token to — [Clusters and machines](../ops/infrastructure.md) |
| `additional_dirs` | directories outside the workspace the agent may reach |
| `memory` | `auto`: whether the agent may keep notes (off); `import_depth`: how deep `@` imports go (5, at most 10) — [Memory](19-input-and-memory.md#memory) |
| `rules`, `commands` | `dirs`: directories of rule files and of custom commands — [Input, memory and commands](19-input-and-memory.md) |
| `tools` | `syntax_check`: whether an edit that breaks a file is refused, reported or allowed — [Tools](05-tools.md#an-edit-that-would-break-the-file) |
| `suggest` | the next prompt suggested after a turn in the terminal, the workbench, the console and Abhed Studio — [below](#suggestions) |
| `cli` | the interactive terminal: `title` (`true`), `notify` (`auto`, `bel`, `osc9` or `off`) and `copy` (`true`) — [The terminal](20-terminal.md#the-title-notifications-and-the-clipboard); `mode_cycle`, managed only, narrows Shift-Tab |

## Context

```json
"context": {
  "offload_at": 0.60,
  "compact_at": 0.80,
  "memory_files": ["ABHED.md"]
}
```

Two things happen as the window fills, in this order.

**`offload_at` — nothing is lost.** Past this fraction, old and large tool
results are replaced *in the window* by a short stub: what the call was, how
the result began, and its `call_id`. The full text is already in the session
record and stays there. The agent gets it back with the `recall` tool — by
`call_id` for one result in full, or by `query` to search everything said and
returned in the session. The four most recent results are never touched, and
neither is anything under 2,000 characters, since a stub costs about as much.
It costs no model call, and it all happens in one pass per crossing, because
rewriting old messages invalidates the endpoint's prefix cache and that is
worth paying once rather than every turn. `0` turns it off; unset means 0.60.

This matters most on a local model with a 16–32k window, where a few file
reads fill the context and the alternative is to compact early and often. A
session that offloads well may never need to compact at all.

What this is and is not: nothing is lost from the record, and anything dropped
from the window can be retrieved. It is not "lossless context" — the window is
still finite, and the model still cannot see everything at once. `recall` reads
this session's record and no other; the session is fixed when the tool is
built, not passed as an argument.

**`compact_at` — the summary.** At this fraction the history is summarized. The
check reserves headroom for the turn about to happen, so a large tool result
cannot take a session from under the threshold to over the hard limit in one
step. The headroom is a quarter of the window, or half of `compact_at`'s
share of it when that is less, so a low value still waits for history. Below 1.0 with real margin: hitting the limit mid-turn is unrecoverable
and the token estimate is approximate. Not much below 0.5 either: a
compaction keeps recent turns up to about half the window, so below roughly
0.3 what it keeps is already over the threshold and it compacts on almost
every turn, each a summary call and a lost prefix cache. The summary keeps
your first message word for word beside it (up to 4,000 characters), so a
name or code word given there survives however the summary restates it.

`ABHED.md` in the workspace is loaded into every session and re-injected whole
after compaction. Project conventions belong there. The full order, imports
and rules are in [Memory](19-input-and-memory.md#memory).

**Size `context_window` for what the model can actually hold.** A local server
reports the size it chose at startup; asking for more does not fail loudly, it
simply stops fitting once a long session fills it.

**Each request to the model is bounded twice.** `call_timeout_seconds` (600 by
default) is the most one request may take, from sending it to the last byte of
the reply. `stall_timeout_seconds` (300 by default) is the longest it may go
with no byte arriving, the first one included; a reply that keeps streaming
resets it. Either one, set on a provider, stops the call and ends the turn with
a model error, recorded on its `model.call` with `"retryable": true`: nothing
was wrong with the request, so sending the message again retries it. A timed-out
call is not retried on its own, so a hung endpoint costs one wait, not four.
Raise `stall_timeout_seconds` for a local model whose prefill of a long prompt
takes longer than five minutes. 0 keeps the default; a negative value is
refused.

## Limits

```json
"limits": {
  "max_turns": 100,
  "max_budget_tokens": 2000000,
  "max_subagents": 8,
  "nested_subagents": false
}
```

`nested_subagents` is a boolean, off by default: while it is off, a subagent
cannot start one of its own, and its `task` or `tasks` call is refused.

`max_budget_tokens` caps the whole session: the primary agent and every
subagent it spawns draw on one allowance, so a fan-out cannot multiply spend
invisibly. A session that exhausts it ends with the terminal reason
`max_budget`, checked at a turn boundary so a turn already in flight
finishes. Zero means no cap.

## Background tasks

```json
"limits": {
  "max_background_subagents": 4,
  "background_max_minutes": 60,
  "background_shells": 4
},
"subagents": {
  "wake": "auto",
  "max_wakes_per_hour": 4,
  "wake_max_turns": 8
}
```

`max_background_subagents` bounds a session's background tasks alive at once,
across its runs; zero allows none. `background_max_minutes` is each task's
lifetime, at most 480, and a background shell's too.
`background_shells` bounds the commands started with `run_in_background`
running at once; zero allows none (see [Tools](05-tools.md#background-commands)).
`wake` is `off`, `notify` or `auto` (the default):
what a result arriving while the session is idle does. `auto` runs the agent
on it, up to `wake_max_turns` turns and `max_wakes_per_hour` times an hour
(zero never wakes); `notify` only records it for your next message. A
surface may allow less: `-p`, eval and unattended runs are always `off`, and
rpc and the SDK join their tasks unless the caller asks for more. The managed
configuration can hold it at `notify` or `off`, and an untrusted workspace
file may only lower these limits and tighten `wake`. See
[Parallel subagents](14-parallel-subagents.md#background-tasks).

## Agents

```json
"agents": {
  "dirs": ["~/.abhed/agents", "/srv/team/agents"],
  "disabled": false
}
```

`dirs` are directories of subagent definitions, `*.md` files; a later
directory wins a name. Unset, it is `~/.abhed/agents`. `disabled: true` loads
only the organisation's `/etc/abhed/agents`. An untrusted workspace file may
set `disabled: true` but not `dirs`. Definitions are read when a session
starts; on a server, `POST /v1/admin/agents/reload` reads them again for the
sessions started after it. See [Agent definitions](17-agent-definitions.md).

## Sandbox

```json
"sandbox": { "allow_network": false }
```

Shell commands run in the sandbox. On the process tier a command may write the
workspace and a few shared folders, not only the workspace:

- **Linux:** the workspace, and a private `/tmp` that is gone when the command ends.
- **macOS:** the workspace, `/private/tmp`, `/private/var/tmp`, your `TMPDIR`, and
  the toolchain caches `~/.cache`, `~/Library/Caches`, `~/.npm`,
  `~/.cargo/registry` and `~/go/pkg/mod`. These are the real folders, shared
  with the rest of your machine, so a command can leave a file there that a
  program outside the sandbox later reads.

Each command on the process tier is kept apart from every other: on Linux
it has its own PID namespace, and on macOS the Seatbelt profile lets it
signal only processes in its own sandbox, so `kill $PPID` cannot stop Abhed.
One consequence, on macOS as on Linux: a command cannot `kill`, or check
with `kill -0`, a process an earlier command left running, such as a server
started with `&` in one call and stopped with `kill` in the next; the
`kill` fails with "Operation not permitted". Start and stop it within one
command, or run it with `run_in_background` and stop it with `shell_kill`.

On the container and vm tiers a command writes only the workspace and a
throwaway `/tmp`. The process tier is a boundary, not a jail: it is not
sufficient for genuinely hostile code.

`"max_procs"` (512 by default) bounds how many more processes a command can
start: on the process tier, as a limit of what your user runs plus this many
(the kernel counts all of the user's processes, and does not bound root), and
on the container and vm tiers, as the container's own limit. The none tier
ignores it. `"max_memory_mb"` (4096) is applied on the container and vm tiers
only; the none and process tiers do not bound memory. `abhed doctor` warns when
you set `max_memory_mb` on a tier that ignores it, when `max_procs` meets the
none tier, and when root runs the process tier, whose processes the kernel
does not bound.

`"terminal": "lines"` makes the workbench terminal run each line as a
policy-checked command of its own instead of an interactive shell; the
[workbench guide](16-workbench.md) says what each mode checks.
`"terminal_idle_minutes"` is how long a workbench shell nobody is watching
stays open; unset means 30.

`"network": "allowlist"`, set only in the managed configuration, sends
commands' traffic through a per-session proxy that reaches only what the
`egress` rules allow, and records each decision; it takes the place of
`allow_network`. Only the process tier enforces it. The `egress` section
(`rules`, `default`, `mode`, `record_paths`, `idle_seconds`) is managed only
too. See [Network policy](21-network-policy.md).

With `allow_network` false, the `bash` tool's description tells the model
that commands cannot reach the network, and a command that fails for that
reason (a name that does not resolve, no route to a host) ends with a note
saying so and pointing at `web_search` and `web_fetch`. A command that exits 0
gets the note only when it ran a network client (`curl`, `wget`, `git fetch`,
`npm`, `pip` and the like) and the failure is in its last five lines, so
output that merely mentions such an error, a log being read, does not.

### The fence tier (preview, Linux)

```json
"sandbox": { "tier": "fence", "allow_network": false, "max_memory_mb": 4096, "max_procs": 512 },
"fence": { "cpu_percent": 200 }
```

The fence is a preview. It is off unless `sandbox.tier` is `"fence"`, it runs
on Linux only (amd64 and arm64), and it fails closed: when anything it needs
is missing, Abhed refuses to start and names the check that failed. It never
runs a command under another tier in its place. For `sandbox.min_tier` it
counts as the process tier.

**Requirements.**

- Linux 6.7 or later with the default `allow_network: false`, since refusing
  TCP takes Landlock ABI 4; with `allow_network: true`, Linux 6.2 or later
  (Landlock ABI 3). Landlock must be turned on (`landlock` in
  `/sys/kernel/security/lsm`).
- cgroups v2, with a cgroup delegated to you. Abhed asks systemd whether the
  scope it runs in has `Delegate=yes`, and refuses when it does not; a
  cgroup you can merely write is not enough, since systemd still manages it.
- An ordinary user. The fence refuses root in this release.

**How to run it.** Start Abhed in a delegated scope:

```sh
systemd-run --user --scope -p Delegate=yes abhed ...
```

`abhed doctor` shows the fence's probe check by check when the fence tier is
configured: the kernel, Landlock, seccomp, no_new_privs, capabilities and the
delegated cgroup, each pass or fail with what it measured.

**What it does.** Abhed re-executes itself as a small launcher for every
command. In order, the launcher checks it holds no privileges and that this
run's probe qualified the host, joins a cgroup made for the tool call, confines
itself with Landlock and a seccomp filter, reports what it applied, and waits.
Abhed writes `process.launched` to the record, and only then lets the launcher
run the command in its place. A launcher that cannot do any step exits 126
without running anything.

- **Files.** A command reads and runs the system folders (`/usr`, `/bin`,
  `/lib`, `/etc`, `/opt` and the like) and `sandbox.read_only_paths`, reads
  `/proc` and `/sys`, and writes only the workspace and a private temp folder
  of the session's. `HOME`, `TMPDIR`, `XDG_CACHE_HOME` and `npm_config_cache`
  point into that folder. The rest of your home, `~/.abhed`, the record and
  the secrets are out of reach.
- **Abhed's state in the workspace.** Landlock cannot keep a command from
  making a folder inside the writable workspace, so Abhed checks instead:
  after every command, before the next one and when the session closes. A
  `.abhed` at the top of the workspace, in any spelling of its case, is first
  renamed in place to `.abhed-planted-<id>`, a name Abhed never reads, and
  then moved to `~/.abhed/quarantine/` where it can be (not when the
  workspace is on another filesystem than your home). It is recorded as
  `fence.state_planted`, saying whether it was moved, renamed in place or is
  still present. The session's commands still running are ended, its
  cgroup killed (`session_killed` in the event), since one may be the
  command that made it, and the session's fence runs no further command. A
  workspace a command made unlistable, by its mode say, counts as holding
  one: the session is refused the same way. When a `.abhed` is still present
  or the workspace cannot be listed at close, the session ends with an
  error saying so, and `abhed -p` exits 1 for a run that otherwise
  completed, as it does for any failure to close the fence; remove it, or restore the folder's permissions and check
  it, before Abhed runs there again.

  The check is not a guard. A file a command writes into the workspace's
  `.abhed` is there while it runs and until the check that follows it, and
  Abhed can read it then (for example its users or memory files). After an unclean exit (Abhed killed,
  the machine down) nothing is checked or moved, so look for a `.abhed` in
  the workspace yourself. A `.abhed` in a subfolder of the workspace or in a
  folder added with `--add-dir` is not checked.
- **Terminals.** Of the devices, a command opens only `/dev/null`,
  `/dev/zero`, `/dev/full`, `/dev/random`, `/dev/urandom` and `/dev/tty`,
  which reaches only its own terminal. Each command runs in a session of
  its own, so a command not started on a terminal, such as those `abhed
  doctor` runs, has none, and `/dev/tty` never reaches Abhed's. It keeps
  stdin, stdout and stderr through the descriptors it was given, but cannot
  open `/dev/ptmx` or any `/dev/pts/*`, so it can neither read nor write your
  other terminals.
- **System calls.** The seccomp filter (profile `command/3`) refuses ptrace,
  namespaces, mounts, bpf, kernel keyrings, loading modules and a filter of
  the command's own. It also refuses signals aimed at Abhed: `kill`,
  `tkill`, `tgkill`, `rt_sigqueueinfo` and `rt_tgsigqueueinfo` naming Abhed's
  process id, `kill` of its process group or of every process (`kill -1`),
  and `pidfd_send_signal` altogether, since a filter cannot see whom a pidfd
  names. Each command also runs in a process group apart from Abhed's.
- **Network.** All or nothing. With `allow_network` false every `socket()` is
  refused, and Landlock refuses TCP as well. With it true, commands get the
  host's network, unfiltered. Unix sockets are refused either way.
- **Limits.** Each session's commands share a cgroup bounded by
  `max_memory_mb` (with no swap), `max_procs` (processes and threads) and
  `fence.cpu_percent` (percent of one core; unset is unbounded), and each tool
  call gets a cgroup of its own with the same bounds. When a command exits,
  whatever it left running is ended.
- **The record.** `fence.qualified` holds the probe's report at the start of
  the session, `process.launched` each command's launch with its call id
  before it runs, `fence.limit` a command that ran into its memory or
  process limit, and `fence.state_planted` a `.abhed` found in the workspace.
  `process.launched` names its `source`: `call` for a tool call or a `!`
  command, `harness` for one Abhed runs itself within a session. When
  `fence.qualified` cannot be recorded, the command line closes the fence
  and runs no command, and an SDK agent is not built.

**What it does not cover.** Abhed itself and what it runs in-process (the
file, web and MCP tools, extensions) stay outside the fence, as on every tier.
There is no network filtering. Below Landlock ABI 6 (Linux 6.12) a command can
signal your other processes, and can reach Abhed by one of its thread ids,
which the filter does not know; Landlock ABI 6 keeps signals inside the
command's own processes. With no PID namespace a command can read other
processes' command lines in `/proc` (not their environment or memory). A hard
link inside the workspace to a file of Abhed's state, made before the
session, stays readable through the link: Landlock checks the path a file is
opened by, and the fence makes no such link, but does not look for one. The
planted-state check above has the limits it states: a file written into the
workspace's `.abhed` is visible while its command runs, nothing is moved
after an unclean exit, and a `.abhed` in a subfolder or an added folder is
not checked.

**What breaks under it.**

- Debuggers and tracers such as `gdb` and `strace` (no ptrace).
- Clients of unix sockets: Docker, ssh-agent, gpg-agent, D-Bus.
- Programs that open a terminal of their own: `script`, `expect`, `ssh -t`,
  `tmux` and `screen`, and anything else that allocates a pty.
- Programs that signal through a pidfd (`pidfd_send_signal`), such as
  util-linux `kill --timeout` and Python's `os.pidfd_send_signal`; Go
  programs and most others use `kill`.
- Tools that sandbox themselves, such as a headless browser with its sandbox
  on, and anything that makes a namespace or mounts.
- Toolchains kept in your home (`~/go`, `~/.cargo`, `~/.nvm`) unless listed
  in `sandbox.read_only_paths`, and scripts that write a fixed path in `/tmp`.
- Every kernel below 6.2, and below 6.7 with the network off.
- A workspace with a `.abhed` folder, or a read-only path inside the
  workspace (Abhed Studio's git and editor protections, `abhed serve`): the
  fence cannot keep part of a writable folder read-only, so it refuses.
- A command Abhed runs outside any session, which is only `abhed doctor`'s
  check, runs fenced but leaves no `process.launched`, since there is no
  session record to write it to.

## Web search

```json
"web_search": {
  "enabled": true,
  "provider": "searxng",
  "base_url": "https://search.internal.example",
  "max_results": 5
}
```

Off by default. Web search is the one tool that sends what the agent is
working on, its queries, to a service outside the machine, so it is an
administrator's setting: **only the managed configuration**
(`/etc/abhed/config.json`) turns it on or says where the queries go. The
same holds for [`web_fetch`](#web-fetch).

Every other layer, `~/.abhed/config.json`, `-settings`, a workspace's
`.abhed/config.json` trusted or not, an SDK `ConfigDir` and the environment,
may only turn it off or narrow it:

| Key | Below the managed file |
|---|---|
| `web_search.enabled`, `web_fetch.enabled` | `false` applies, even when the managed file says `true`; `true` is set aside |
| `web_search.max_results`, `web_fetch.max_chars` | a lower number applies |
| `web_fetch.allowed_hosts` | only hosts the managed list already allows apply; with no managed list, the list is set aside, since its hosts would be fetched unasked |
| `web_search.provider`, `base_url`, `api_key`, `api_key_env` | set aside, so no file can send a managed search's queries to another endpoint |

A value set aside is named in a startup warning, as other managed-only
settings are, and each one is recorded in the session as a `config.refused`
event: the layer, the file or flag, the key, the value asked for with any key
redacted, and who asked (the account, or the agent's command that ran a
nested `abhed`). A value the managed file replaces is recorded as
`config.refused` with the decision `overridden`, and a narrowing as
`config.narrowed`. `session.started` says whether web search and web fetch
are on and who decided. `abhed doctor` prints "enabled by the managed
configuration" or "off; only the managed configuration can enable it".
`/config set web_search.enabled true` is refused and recorded the same way.
On a server accounts sign in to (`auth.mode` other than `none`), the
operator's own attempts are not put in every user's session: `abhed serve`
logs each once at startup, as `configuration attempt`, and hands it to the
admin audit hook as `config.refused` or `config.narrowed`.

**On your own machine** the administrator is you with `sudo`:

```sh
sudo abhed admin web-search on --provider searxng --base-url http://127.0.0.1:8888
sudo abhed admin web-search off
```

`abhed admin web-search` edits only the managed file, keeping everything else
in it, and refuses unless it can write there. `--provider`, `--base-url`,
`--api-key-env` (the name of the variable holding the key, in capitals,
digits and `_`, such as `SEARCH_API_KEY`; something shaped like a key is
refused, and the key itself is never taken on the command line) and
`--max-results` go with `on`. Every attempt, done or refused, is appended to
`admin.jsonl` beside the managed file: when, the command as checked (a value
that failed its check shows as `(refused)`, and a URL without its
credentials), the account and uid (and `SUDO_USER` under sudo), the section
before and after with any key left out, and the result. The managed file is
replaced whole, keeping its owner, group and mode, so a `root:abhed 0640`
file stays readable by the server. A refused attempt by
someone who cannot write there is logged in their own `~/.abhed/admin.jsonl`
instead. It is refused inside an agent's command, as other commands that
administer Abhed are. New CLI sessions take the change; restart
`abhed serve` for the server.

**The system log.** A user can erase their own `admin.jsonl`, so every
attempt, changed, unchanged or refused (one refused inside an agent's command
included), also goes to the operating system's log, which an administrator
can read and an ordinary user cannot erase. Each entry is one line of fixed
fields, every value quoted with newlines and control characters escaped, so a
value cannot forge an entry:

```text
abhed-admin time="2026-10-06T04:40:00Z" uid="0" user="root" sudo_user="ana" action="web-search on" from="{\"enabled\":false}" to="{\"enabled\":true,\"provider\":\"searxng\",...}" result="changed" reason=""
```

It holds no key: a section shows `api_key_set` in place of one. Find the
entries with:

| System | Where | Query |
|---|---|---|
| macOS | the unified log, through `/usr/bin/logger` | `log show --last 7d --predicate 'process == "logger" AND eventMessage CONTAINS "abhed-admin"'` |
| Linux, journald | the journal, through its native socket, identifier `abhed-admin`, facility auth | `journalctl -t abhed-admin` (as root or a member of `adm` or `systemd-journal` to see every user's attempts); `journalctl -t abhed-admin -o verbose` shows the `_UID` journald attached itself |
| Linux, no journald | syslog, tag `abhed-admin`, facility auth | `grep abhed-admin /var/log/auth.log` (Debian, Ubuntu) or `/var/log/secure` (RHEL, Fedora) |
| Windows | not written yet | the attempt is in `admin.jsonl` only, and the command says so on stderr |

**Telling real entries apart.** The tag is not a credential: any local user
can add a line tagged `abhed-admin` to the system log with `logger -t
abhed-admin ...`, with whatever `uid=` and `result=` they like in its text. The fields inside the
message are what `abhed` wrote only when the log itself says who sent it:

- **Linux, journald:** `journalctl -t abhed-admin -o verbose` shows the
  trusted fields journald adds itself, beginning with `_`. A real entry has
  `_COMM=abhed` (a forged one usually `_COMM=logger`, though a user can
  name their own program `abhed`) and a `_UID` that
  matches the `uid=` in the message; for a change, `_UID=0`.
- **macOS:** every entry is sent by `/usr/bin/logger`, so the process name
  does not tell them apart. `log show --last 7d --style json --predicate
  'process == "logger" AND eventMessage CONTAINS "abhed-admin"'` gives each
  entry's `userID`, the uid the system recorded for the sender; it must match
  the `uid=` in the message, and is `0` for a change.
- **syslog files** carry no sender the system vouches for; trust them only as
  far as `admin.jsonl` beside the managed file, which only root can write.

A system log that cannot be written is reported on stderr and does not change
the command's result. The unified log keeps entries for days, not forever;
forward it if you need them longer.

**Providers.**

| `provider` | Needs | Notes |
|---|---|---|
| `searxng` | `base_url` of your instance | Recommended: free, self-hosted metasearch. The queries stay on infrastructure you run, and it is the one an air-gapped site can put behind its own egress proxy |
| `brave` | a key, in the variable `api_key_env` names | Hosted API |
| `tavily` | a key | Hosted API built for agents |
| `serper` | a key | Hosted API returning Google results |
| `duckduckgo` (`ddg`) | nothing | The default name, and unofficial: it scrapes DuckDuckGo's HTML results page, which is not an API. It may break when the page changes and may be rate-limited, and its terms do not cover automated use |

A key belongs in an environment variable named by `api_key_env`, not in the
file. A query holding a stored secret is refused before it is sent.

**Upgrading from 1.2.5 or earlier.** Web search or web fetch turned on in
`~/.abhed/config.json`, a `-settings` file or a workspace's file is now set
aside, with a warning at startup, and the tool is no longer offered. Ask your
administrator to turn it on in the managed configuration, or, on your own
machine, run `sudo abhed admin web-search on` with the provider and endpoint
you used (for web fetch, add the `web_fetch` section to
`/etc/abhed/config.json` with sudo). Then remove the section from your own
file; a copy that only repeats the defaults, as `abhed init` writes, is
quietly accepted.

## Web fetch

Managed only, as [web search](#web-search) is: only the managed file turns it
on or names `allowed_hosts`.

```json
"web_fetch": {
  "enabled": true,
  "allowed_hosts": ["docs.python.org", "*.github.com"],
  "max_chars": 20000
}
```

Off by default, and separate from `web_search`: turning search on does not
let the agent send a request to any site, and turning this on does not give
the shell a network. `web_fetch` reads one http or https page through Abhed
and returns its text. It never reaches a loopback, private, link-local,
metadata or reserved address, whatever the host name resolves to.

`allowed_hosts`, when set, is every host the agent may fetch: a name, or
`*.` and a domain for any host under it (not the domain itself). An entry
with a scheme, port or path is refused at load, and so is one that could
never match a host as web_fetch writes it: a name ending in a number, an
IPv4 address not written as four plain decimal numbers, or a wildcard over
address numbers such as `*.216.34`. A listed host runs without asking only
on its scheme's default port; a URL naming another port asks ("web_fetch
asks: the URL names a port…") unless an allow rule names it.

Without `allowed_hosts`, any public site can be fetched, and a URL can carry
whatever the model puts in it, so every call asks in the `default`,
`accept-edits`, `auto` and `plan` modes (the reason reads "web_fetch asks:
no allowed_hosts configured") unless an allow rule such as
`"allow": ["web_fetch(https://docs.python.org/*)"]` matches.
`plan` asks too, because a console client can narrow any session to it.
`bypass` runs it. A headless run, which has no one to ask, needs such an
allow rule or `allowed_hosts`; `abhed eval` approves every ask, so an eval
run with `web_fetch` on and no host list fetches any public URL.

A wildcard over a single label, such as `*.com`, is refused. Be careful with
wildcards over shared hosting — `*.github.io`, `*.vercel.app`,
`*.s3.amazonaws.com`, `*.githubusercontent.com` — where anyone can publish a
site: listing one lets any of those sites receive, without asking, whatever
the model puts in a URL. `max_chars` is the most text
one call returns; unset means 20,000, and the most is 100,000. A longer page
is read in parts. See [Tools](05-tools.md#reading-a-web-page).

## Suggestions

```json
"suggest": {
  "enabled": true,
  "model": "small"
}
```

After a turn completes in the interactive terminal, the workbench, the console
or Abhed Studio, one small model call guesses what the person may ask next,
and the input shows it dimmed until they take it (Tab) or type. It is never
sent on its own. The call reads the person's last message, the agent's final
reply and the names of the tools used, as the record holds them (stored
secrets redacted), and asks for one line of at most 80 characters in the
person's language. Its reply is cleaned of control and format characters, and
dropped if it is empty, a `/` command, a `!` shell line, would repeat a
stored secret (checked on the whole reply, before it is cut), or tells you or
the agent to ignore, bypass or override a policy, an approval, a rule, the
sandbox or safety, suggests something destructive or outward (delete,
remove, reset, revert, force, push, merge, deploy, publish, install, drop,
wipe, disable, …), consents (yes, ok, approve, allow, accept, confirm,
proceed, trust, go ahead, do it, …), holds a character a shell reads
specially (`` $ ` | ; & < > ``), names a secret-like variable (`STRIPE_KEY`,
`GH_TOKEN`), or asks to print, show, read or send a secret (a stored
secret's name, or a key, token, password or credential). The lists fail
closed: an ordinary suggestion that uses one of these words is not offered
either, since better none than a risky one. A
suggestion is the model's text, which what the agent read can shape; it is
never sent unless you choose to send it. The call is
made after the turn has ended, so nothing waits for it; the next prompt, a
wake, typing or closing the session cancels it. The record keeps the
suggestion as `suggestion.offered` after the run's `session.ended`, then the
call as a `model.call` with `purpose: suggestion`, counted in the session's
tokens and budget. A call cancelled by closing the session offers nothing,
and its `model.call` is still recorded, since the request went out.

Suggestions are on by default, so each completed turn costs one extra model
call. If `suggest.model` names a provider on a different endpoint from the
session's, that endpoint receives those excerpts of the conversation: the
person's last message and the agent's final reply, redacted. On an
air-gapped setup, name a provider inside the same boundary or turn
suggestions off.

| Key | Default | |
|---|---|---|
| `enabled` | `true` | `false` turns suggestions off. A workspace file may only turn them off, and a managed `false` binds |
| `model` | the session's | a configured provider to ask instead, such as a smaller, cheaper model. Not taken from a workspace file until it is trusted. A name the managed file's model pin does not allow turns suggestions off |

None is made for `-p`, `abhed rpc`, a scheduled or unattended run, or an
embedded agent unless it sets `Options.Suggest`; nor after an error or a
stop, during a wake, while an approval waits, or while the person is typing;
an ask that arrives while it is being made stops it, and one that arrives
after it was offered takes it away.

## Storage

```json
"storage": {
  "driver": "postgres",
  "dsn": "postgres://abhed:...@localhost:5432/abhed",
  "tenant": "default"
}
```

Without `postgres`, the command line keeps sessions in the local record under
`~/.abhed/records` (see [Sessions and the local record](12-records.md)), and
`abhed serve` keeps them in memory, which loses them when the process exits.
`postgres` makes them durable and replayable for a server and for teams.

Two settings about the local record are taken only from the managed
configuration:

```json
"record": { "dir": "/srv/abhed/records", "retention_days": 90 }
```

`record.dir` moves the record, and is state the agent cannot reach.
`record.retention_days` prunes sessions last used longer ago than that when
the record is opened, leaving a tombstone for each. Unset, nothing is removed
unless you run `abhed record prune`.

**Two roles, not one.** The audit record is only as protected as the role that
writes it. Database triggers refuse an `UPDATE`, a `DELETE` or a `TRUNCATE` on
`events` — but the role that *owns* a table may disable its triggers or drop
it, and nothing inside the database can stop an owner. So:

```sql
CREATE ROLE abhed_owner   LOGIN PASSWORD '…' NOSUPERUSER NOBYPASSRLS;  -- owns the tables
CREATE ROLE abhed_runtime LOGIN PASSWORD '…' NOSUPERUSER NOBYPASSRLS;  -- the server runs as this
GRANT ALL ON SCHEMA public TO abhed_owner;
```

```bash
# Once, and after each upgrade — ideally from somewhere other than the server host:
ABHED_MIGRATE_DATABASE_URL='postgres://abhed_owner:…@db/abhed' abhed migrate

# The server only ever sees the runtime role:
ABHED_DATABASE_URL='postgres://abhed_runtime:…@db/abhed' abhed serve
```

`abhed migrate` applies the schema as the owner and grants the runtime role
exactly what the server uses: `INSERT` and `SELECT` on `events`, and nothing
that changes or removes one. Keep the owner's credentials off the host that
runs the server; whoever holds them can alter the record. After an upgrade
that adds columns, run `abhed migrate` again before starting the server: on a
schema older than the binary it refuses to start and names what is missing.

**Abhed refuses to start** if the role in `storage.dsn` could alter the record
— if it owns `events`, or holds `UPDATE`, `DELETE` or `TRUNCATE` on it — and
says which. It asks the database what the connection can do rather than
trusting the configuration.

**`"single_role": true`** turns that refusal off: the server connects as the
role that owns the tables and applies the schema itself, as every version up to
0.2 did. It is the simple setup for a laptop or a trial. It is also weaker, and
the difference is exact: the record is then protected against application bugs
and stray statements, and **not** against anyone holding the server's database
credentials. `abhed doctor` and the startup banner say which mode is in force.

**Do not connect as a superuser,** in either mode. Row-level security is what
isolates tenants, and Postgres does not apply it to a superuser or a
`BYPASSRLS` role, not even with `FORCE`. Abhed checks and refuses, because a
control that is silently off is worse than one that is visibly missing.

## Stopping the server

```json
"server": { "drain_seconds": 20 }
```

On SIGTERM the server stops taking turns (a message that would start a
turn, steer a running one or send it now gets 503 with `Retry-After`),
waits up to `drain_seconds` for running turns to finish, then ends those
still running, recorded as `shutdown`, including a turn stopped before the
model's first reply, and kills the command each is running with everything
it started. A message steered into a turn that is ended this way is recorded
as `message.dropped`, not delivered. The terminals of an idle workbench
session are closed and their results recorded before its end. It then waits
up to five seconds for turns to record their end, and up to ten more for
open HTTP requests. The worst case is `drain_seconds` + 15 seconds, so give the
process at least that much grace: with Kubernetes' default
`terminationGracePeriodSeconds` of 30, keep `drain_seconds` at 15 or less.
Without `drain_seconds`, running turns are ended at once. SIGINT and a
hang-up (SIGHUP) stop the server the same way; a hang-up is not a reload.
Signals that arrive while it drains are ignored, so a second SIGTERM does
not cut the drain short; SIGKILL, as an orchestrator sends at the end of its
grace period, is what ends it sooner.
Started with hang-ups ignored, as under `nohup`, the server keeps ignoring
them.

A second SIGTERM, or a SIGINT (Ctrl-C) sent after the first, does not cut the
drain short: once shutdown has begun, both are ignored, and the server exits
when the drain budget and the waits above have run out. To stop it sooner,
send SIGKILL, and accept that turns still running then record no end.

## Accounts

With `auth.mode` set to `local` and no database, accounts live in a file:
`<workspace>/.abhed/users.json` by default, or wherever `auth.users_file`
points (`ABHED_USERS_FILE` overrides it unless the managed file sets it). A server deployment sets it to a
directory outside every workspace, so accounts never sit in a tree an agent
is pointed at. With Postgres, accounts are rows and the file is not used.
`abhed user add`, `passwd` and `import` refuse a `users_file` that `serve`
would refuse to start with, with the same message.

A users file inside the workspace, the default one included, is read only
when the workspace is trusted (`abhed trust grant`, `-trust-workspace`, or a
trusted `.abhed/config.json`). In an untrusted workspace `serve` starts with
no accounts from it, `abhed user list` warns that it is ignoring the file, and
`add`, `passwd`, `remove` and `import` refuse. The managed `auth.users_file`,
and one outside the workspace, are read either way. See [Accounts in the
workspace](../architecture/workspace-trust.md#accounts-in-the-workspace).

A change to an account reaches its live sessions on their next request,
whichever process made it: an account removed with `abhed user remove` is
signed out, and a group added or removed applies at once. Removing
administrator rights through `POST /v1/admin/users/admin` also ends that
person's sessions, on every server sharing the account store: at once on the
one that removed them, and within about 2 seconds on the others over
Postgres. An event or terminal stream already open is authorised again while
it runs, and ends when its sign-in would now be refused: at once for a change
made on the same server, within 10 seconds otherwise.

Signing out ends sign-ins, not work. After Sign out everywhere, or a removal
of administrator rights, a background shell or task the person started keeps
running, and its result still starts a woken run in their session, since the
account is still active. Stop the session's tasks, or remove the account, to
end it; see [background tasks](14-parallel-subagents.md#background-tasks).

`auth.require_group` names a group everyone must be in to use the server.
It is checked once someone has signed in: the sign-in page, sign-in,
sign-out, `/v1/whoami` and `/v1/health` stay reachable. A signed-in person
outside the group is signed out and told why, on the front page in a browser
(the workbench says so where it is, and a reload keeps the reason) and as
`403` with the reason to an API client.

### Keys for the paid editions

The Community Edition checks these when it loads a config, so a mistake is
reported at once, and otherwise ignores them: GitHub sign-in is part of the
paid editions.

| Key | Meaning |
|---|---|
| `auth.github.orgs` | admit members of any of these GitHub organisations |
| `auth.github.teams` | admit members of any of these teams, each written `org/team-slug` |
| `auth.github.allow_any` | admit every GitHub account; cannot be combined with `orgs` or `teams` |

```json
{ "auth": { "github": { "orgs": ["acme"], "teams": ["acme/platform"] } } }
```

`auth.proxy_logout_url` is the authenticating proxy's own sign-out, in `proxy`
mode: with it the console offers Sign out and `/logout` redirects there;
without it there is no Sign out, since the proxy owns the session.

Signing out of Abhed's own sessions is `POST /logout`, from a page on this
server; `GET /logout` shows a page with a Sign out button and ends nothing,
so another site cannot sign anyone out with a link or an image.

## Where settings come from

Later sources win, except that an org-managed file cannot be overridden:

1. built-in defaults
2. `~/.abhed/config.json`
3. a `-settings` file or inline JSON, for one run
4. `.abhed/config.json` in the workspace (whole only when trusted)
5. environment (`ABHED_DATABASE_URL` and similar), except for a key the managed file sets
6. command-line flags and the SDK's `Options`
7. **managed settings**, which nothing below can loosen

Some settings are **managed only**: a lower layer's value is set aside with a
warning and a record event. They are `record.dir`, `record.retention_days`,
`hooks.disabled`, `hooks.managed_only`, `cli.mode_cycle`,
`studio.disable_host_terminal`, and the whole of `web_search` and
`web_fetch`, which a lower layer may only turn off or narrow
([Web search](#web-search)). A lower layer's value the managed file replaces
is recorded too, as `config.refused` with the decision `overridden`.

Inside an agent's command, a nested `abhed` refuses `-settings` and
`-mcp-config` whatever they hold: either can name an endpoint the nested
session would send the code or its queries to. A nested run narrows with
`-mode plan`, `-disallowedTools` or `-max-turns`.

### The managed file

`/etc/abhed/config.json` belongs to the organisation. It is read last, so each
key it sets replaces what the user and project files said. A list it sets,
such as `permissions.deny`, replaces the lower files' list; so does a map
entry, such as one provider under `model.providers`. Its presence makes the
policy engine managed: `bypass` mode is refused wherever it comes from.

When the managed file sets any `permissions` setting but not
`permissions.allow`, the allow rules `~/.abhed/config.json` and a trusted
workspace's `.abhed/config.json` add are left out, and Abhed warns at startup
naming each rule and its file; the built-in allow rules stay. Put rules the
organisation accepts in the managed `permissions.allow`. When the managed file
sets `permissions.allow`, exactly its list applies. The same holds for
`permissions.git_extensions`, the git extensions that run without the
destructive step's question ([Permissions](04-permissions.md#the-order)).

Two effects to plan for. A call that a dropped rule approved now asks, and in
`-p`, `rpc` and other runs with no one to approve, it is refused. And once
any file rule is dropped, every built-in allow rule comes back, even one the
file had left out of a shorter list. To keep a built-in rule from applying
under a managed file, add an ask or deny rule for it; a shorter allow list
does not do it.

What a caller sets over the files, the CLI's flags and the SDK's `Options`,
may tighten what the managed file set and never loosen it:

| Setting | A flag or option may |
|---|---|
| `permissions.mode` | choose `plan` or the managed mode; `bypass` is refused even when the file does not set a mode |
| `tools.syntax_check` | make it stricter only (`off` < `report` < `refuse`) |
| `limits.max_turns` | lower it |
| `permissions.allow` | add nothing when the file sets any `permissions` setting (the `-allow` and `-allowedTools` flags, `/permissions allow`, `Options.Allow`, rpc `start`); the user's and workspace's files' allow rules are dropped with a warning |
| `additional_dirs` | add nothing |
| `permissions.deny` | add rules; the managed ones stay |

A refused override stops the command with an error naming the setting. The
console already lets a client narrow its session's mode to `plan` and nothing
else. Without a managed file, flags and options apply as they always have.

`config.Config.ManagedKeys` lists what the managed file set, as dotted paths
(`permissions.mode`, `model.providers.onprem`), and `ManagedSets` asks about
one. `Config.Apply` lays overrides over a configuration by these rules, so a
program that loads configuration itself can honour them the same way; a
`Config` built by hand, not by `config.Load` or `config.LoadManaged`, records
no managed keys and is bound by nothing. A managed `limits.max_turns` of zero
or less binds nothing. A permission mode that is not one of `default`,
`accept-edits`, `plan`, `auto` or `bypass` is refused, managed file or not.

The interactive `/mode` command is bound as `-mode` is. `abhed resolve` runs
in `auto` unless told otherwise, but under a managed file that pins a mode it
runs in that mode; an explicit `-mode` is judged as given. `abhed eval`
approves every prompt with nobody to ask, so it refuses to run under any
managed file.

A managed file that exists but cannot be read, including one in a directory
that cannot be searched, or a link at the managed path that points nowhere,
is an error: Abhed stops rather than run unmanaged.

The environment variables in step 4 never change a setting the managed file
makes. Each one is ignored where the managed file sets its key, and Abhed
warns at startup naming the key, without the value:

| Variable | Setting it changes |
|---|---|
| `ABHED_BASE_URL` | `model.providers.<default>.base_url` |
| `ABHED_MODEL` | `model.providers.<default>.model` |
| `ABHED_API_KEY` | `model.providers.<default>.api_key` |
| `ABHED_DATABASE_URL` | `storage.dsn`, and `storage.driver` to `postgres` when it is `memory` |
| `ABHED_MIGRATE_DATABASE_URL` | `storage.migrate_dsn` |
| `ABHED_USERS_FILE` | `auth.users_file` |

A managed provider entry under `model.providers` binds every field of it,
those it leaves out included, since the entry is one setting. Where the
managed file does not set the key, the variable applies as before, over the
user's and workspace's files. An administrator who means to pin the endpoint
sets the provider in the managed file; one who sets only `model.default` leaves
that provider's endpoint to the lower files and the environment.

The model is not pinned by the managed file alone: `-model` and the console's
picker choose among the providers any file defines, and the SDK's `Provider`
names any endpoint.
An editor over `abhed acp` and the SDK's `SwitchModelNamed` are the
exception: a managed `model.default` pins them to that model.

Run `abhed doctor` after any change. It reports what is actually in effect,
which is not always what the file appears to say. With Postgres storage it
opens the event store and sign-in; if either cannot be opened, as before
`abhed migrate` has made the schema, it says "Not ready" and exits 1. With
memory storage it names both stores: memory for `abhed serve`, whose sessions
do not survive a restart, and the local record the command line keeps.

## Keys nothing reads

A key that no setting reads, such as `model.provider` where `model.default`
was meant or a misspelt `sandbox.allow_networks`, is ignored: a configuration
that loaded before still loads. It is not silent, though. Each such key is
written to standard error once per process, with the file, its path in the
JSON and, when a known key is close, the one that was probably meant:

```text
abhed: warning: /srv/repo/.abhed/config.json: unknown key model.provider is ignored (did you mean model.default?)
```

`abhed doctor` lists the same keys under `config` and fails, so a check in a
deployment pipeline catches them. Keys match regardless of case, as JSON
decoding does, so `Model` is read as `model` and is not reported. A key that
starts with `_` or `$`, such as `_comment` or `$schema`, is an annotation for
people and is never reported. An unknown key in the managed file
(`/etc/abhed/config.json`) is marked as such, since only its owner can correct it.

## Trusting the workspace configuration

A `.abhed/config.json` that came with a repository could turn on bypass mode,
send your code to another model server, or start processes. Until you trust
it, Abhed applies only its deny and ask rules, a narrower mode (`plan` or
`default`), a stricter sandbox and lower limits. It ignores the rest, and
prints a warning naming each ignored setting. `abhed doctor` lists them too.
`serve`, `user` and `migrate` refuse to start when an untrusted file sets
`auth`, `storage` or `server`, since running without those would leave the
server open.

- **Interactive `abhed`** asks once, listing what the file would change: trust,
  don't trust, or view the file.
- **Headless runs** (`-p`, `acp`, `rpc`, `resolve`, `serve`) never ask. They
  use your stored decision, or trust the file for one run with
  `-trust-workspace` or `ABHED_TRUST_WORKSPACE=1`.
- **`abhed trust`** shows the file and what it would change. `abhed trust
  grant` trusts it, `abhed trust revoke` forgets the decision, and `abhed trust
  list` lists every decision.

A workspace's subagent definitions in `.abhed/agents/*.md` are covered by the
same decision, with a hash of their own: until you trust them they are not
loaded, and the warning names each one as `agents/<name>`. A definition can
choose a model, and so where your code is sent. Declining new definitions
keeps a file you already trusted. See [Agent
definitions](17-agent-definitions.md).

A workspace's `.abhed/users.json` is read only when the workspace is trusted;
`abhed trust grant` covers it, and an untrusted workspace's accounts cannot
sign in (see [Accounts](#accounts)).

Trust is for the file's exact contents: after an edit it is asked about again.
`abhed init` trusts the file it writes. Your own `~/.abhed/config.json` and
the managed `/etc/abhed/config.json` are not affected, and the managed file
still wins. The full classification of every setting is in [Workspace
trust](../architecture/workspace-trust.md).
