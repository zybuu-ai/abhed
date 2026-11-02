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
| `model` | providers and which one is default — [Models](03-providers.md); `ollama.assist` (`ask`, `local` or `off`): what Abhed offers when a chosen local Ollama model is not working yet — [A local Ollama model](03-providers.md#a-local-ollama-model); `completion`: the provider Studio's inline completions use, and `next_edit` and `commit` for its next-edit suggestions and commit messages (each falls back to `completion`) — [Inline completions](34-inline-completions.md); `allowed`: the only models any session may use — [Allowed models and MCP servers](#allowed-models-and-mcp-servers) |
| `completion` | `enabled` and `local_only`: turn Studio's model features (inline completions, next-edit suggestions, commit messages) off, or keep them to a model on this machine — [Inline completions](34-inline-completions.md) |
| `permissions` | what runs unattended — [Permissions](04-permissions.md) |
| `mode` | `default`: the mode new sessions start in where `permissions.mode` names none, such as the CLI's and Studio's new chats in this workspace; a `-mode` flag is chosen over it (`plan`, `default`, `accept-edits` or `auto`; never `bypass`, which is chosen for a session each time). `permissions.mode`, when set, wins, and a managed mode binds. An untrusted workspace file may only set it narrower than sessions would start in |
| `context` | compaction threshold and memory files |
| `limits` | turn, token and subagent budgets |
| `sandbox` | process isolation and network access; `toolchains`, `toolchains_preapproved` and `toolchains_registries`: languages installed on demand for commands and their package registries — [Toolchains](32-toolchains.md); `tier`: `fence` chooses the fence preview — [below](#the-fence-tier-preview-linux); `terminal`: whether the workbench terminal is a shell (`shell`, the default) or checks each line (`lines`) — [The workbench](16-workbench.md); `listen`: whether commands on macOS may listen on loopback for a dev server (`ask`, `allow`, `off`) — [below](#sandbox) |
| `fence` | `cpu_percent`: the fence tier's CPU bound; `socketcall`, and the managed `egress_port_only` and `allow_root` — [below](#the-fence-tier-preview-linux) |
| `storage` | in-memory or Postgres |
| `control_plane` | `enabled` serves `/v1/sandboxes` from `abhed serve` (off); its limits and restart policy; `per_session` gives each session a sandbox of its own (off, but on by itself for an `abhed serve` with more than one account); `docker` and `podman` register the container drivers — [Sandboxes over the API](22-sandboxes.md) |
| `auth` | who may use a server deployment; `cli`: `abhed login` for local accounts — [TLS and command-line sign-in](26-tls-and-cli-login.md) |
| `server` | how `abhed serve` is reached: `tls` (certificates, client certificates, reload), origins, drain; `shared_sessions` keeps every account in the server's one workspace instead of the sandbox per session a multi-account server gets by default (off) — [TLS and command-line sign-in](26-tls-and-cli-login.md), [below](#stopping-the-server), [what accounts share](../ops/enabling-auth.md#what-accounts-share) |
| `skills` | where skills are loaded from — [Skills](06-skills.md) |
| `agents` | where subagent definitions are loaded from, or `disabled` — [Agent definitions](17-agent-definitions.md) |
| `subagents` | what a background task's result does while the session is idle — [Parallel subagents](14-parallel-subagents.md#background-tasks) |
| `extensions` | processes that can intercept — [Extensions](07-extensions.md) |
| `mcp` | Model Context Protocol servers — [MCP](08-mcp.md); `allowed` and `denied`: which servers may ever start — [Allowed models and MCP servers](#allowed-models-and-mcp-servers) |
| `custom_providers` | providers added without a rebuild |
| `network` | `upstream_proxy`: a corporate HTTP proxy all of Abhed's connections, and the allowlisted commands' traffic, go out through, with a CA bundle, credentials by secret name, a `no_proxy` list and `resolve_via_proxy` (off); never from a workspace, and the managed file can force it — [Network policy](21-network-policy.md#a-corporate-upstream-proxy) |
| `web_search` | whether the agent can search the web, with which provider — managed only, [below](#web-search) |
| `web_fetch` | whether the agent can read a web page, and from which hosts — managed only, [below](#web-fetch) |
| `retrieval`, `rag` | the local index, and external corpora. Each session builds the index at start; with `retrieval.embed`, the embeddings are kept in `~/.abhed/index`, so a start embeds only chunks that changed, and `abhed index` builds them ahead of a session |
| `k8s`, `ssh` | infrastructure tools, off by default; `k8s.clusters` names the only servers `k8s_login` sends a token to — [Clusters and machines](../ops/infrastructure.md) |
| `additional_dirs` | directories outside the workspace the agent may reach |
| `memory` | `auto`: whether the agent may keep notes (off); `import_depth`: how deep `@` imports go (5, at most 10) — [Memory](19-input-and-memory.md#memory) |
| `rules`, `commands` | `dirs`: directories of rule files and of custom commands — [Input, memory and commands](19-input-and-memory.md) |
| `tools` | `syntax_check`: whether an edit that breaks a file is refused, reported or allowed — [Tools](05-tools.md#an-edit-that-would-break-the-file); `bash_checkpoints` (`on`, `off` or `required`), `bash_checkpoint_mb` (32) and `bash_checkpoint_files` (20000): what is kept before each `bash` command so `/undo` takes its changes back — [Records](12-records.md#changes-made-through-bash) |
| `suggest` | the next prompt suggested after a turn in the terminal, the workbench, the console and Abhed Studio — [below](#suggestions) |
| `verify` | whether each answer is checked against the session's record (`off`, `label`, `strict`), the critic, and the completion contract — [below](#verified-answers), [Verified answers](29-verified-answers.md) |
| `monitor` | the supervisor, which reviews each call after policy and may only tighten the decision: on with the rules judge by default; `judge: "model"` adds a model judge — [The supervisor](04-permissions.md#the-supervisor) |
| `plan` | `runbooks` (`true`): whether an accepted plan's runbook answers its steps' asks. `false` turns it off, and a plan with a runbook is refused; a workspace file may only turn it off, and a managed value binds either way — [Runbooks](28-runbooks.md#turning-runbooks-off) |
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
share of it when that is less, so a low value still waits for history. The
first check after a compaction leaves the headroom out, so a small window
is not summarized twice in a row. Below 1.0 with real margin: hitting the limit mid-turn is unrecoverable
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

- **Linux:** the workspace, and a private `/tmp` that is gone when the command ends. The host's
  `/tmp` and `/var/tmp` are out of every sandboxed command's reach, so state
  kept there (a `HOME`, `auth.users_file`) is not refused as writable.
  Under bubblewrap commands read and run the system's `/usr`, `/bin` and
  `/lib`; a toolchain installed elsewhere (under `/opt` or your home) is
  found once its folder is listed in `sandbox.read_only_paths`.
- **macOS:** the workspace, `/private/tmp`, `/private/var/tmp` and your
  `TMPDIR`. The toolchain caches commands write are folders of the
  sandbox's own, in `TMPDIR` and removed when the session ends:
  `XDG_CACHE_HOME`, `GOCACHE`, `GOMODCACHE`, `npm_config_cache`,
  `PIP_CACHE_DIR` and `CARGO_HOME` point there, so nothing a command writes
  reaches the real `~/.cache`, `~/Library/Caches`, `~/.npm`,
  `~/.cargo/registry` or `~/go/pkg/mod`, which stay readable. Go modules
  already in your module cache are read from it through `GOPROXY`
  (`file://…/cache/download` first), so a build that worked offline still
  does; npm, pip and cargo start from an empty cache and need the network
  for what they fetch. Commands read in home only the workspace, `read_only_paths`, the
  toolchains, the caches and the files git, editors and shells read (see
  [security posture](../trust/security-posture.md));
  `"readable_home": true` lets them read the rest of home but the credential
  paths again, and `read_only_paths` names a folder they need. `"shared_caches": true` lets commands write the real
  folders again, shared with the rest of your machine, where a command can
  leave a file a program outside the sandbox later reads; a workspace file
  can only turn it off. Even then, the toolchain cache, the Linux helper's
  copies and every `read_only_paths` folder stay unwritable, except a
  `read_only_paths` folder that holds the workspace: it gets no write deny,
  so where a temp or cache area lies inside it, that part stays writable.

On macOS a command cannot listen for connections until you agree for the
workspace (`"listen": "ask"`, the default): a dev server refused its port
in Abhed Studio's terminal (`listen EPERM`) brings the offer. Once you
agree, every command in every session of the workspace, the agent's
included, may listen, so `npm run dev` opens in your browser. `"allow"`
lets commands listen without asking and cannot be taken back from Studio;
`"off"` never lets them. Both `listen` and `listen_ports` are an operator's
settings, read from your own or the managed file: a workspace file, even a
trusted one, can only make `listen` stricter and cannot set `listen_ports`.
Taking it back from Studio reaches this window's sessions at once, and
another window's within two seconds, before its next command starts.

Listening is TCP only, and only on the ports in `"listen_ports"`: by
default `["3000-3009", "4200", "4321", "5173-5179", "8000-8009"]` (Next.js,
Rails and Express, Angular, Astro, Vite, Django and `python3 -m
http.server`), chosen to leave out databases, caches and consoles (5432,
3306, 6379, 27017, 8080, 8090, 8888) and macOS's own AirPlay ports (5000,
7000). Each entry is a port or a range above 1024, at most 512 in all. A
UDP bind, or any other port, is refused,
and so are Abhed's own reserved ports (47600-47855, or
`ABHED_LOOPBACK_PORTS`) and a listen port one of your programs already
listens on (another user's ports the kernel itself keeps). Commands that
listen still connect to nothing on loopback but the egress proxy.

A command can take a listen port before one of your programs starts on it.
Your program then still starts (Go, Node and Python servers bind every
address with `SO_REUSEADDR`), but connections to `127.0.0.1` would reach the
command. Abhed reads the Mac's listeners every two seconds. When one of
your programs now listens on a port a command holds, it ends every process
of that session's sandbox, its terminals and the agent's running commands
among them, so nothing there can listen on the port again (a new command
is not given a port your program holds), and tells you and records it in
that session alone (`sandbox.listener_ended`). Until then, up to two
seconds, the command can still take what is sent there. Abhed ends only a
process it can prove is one of that sandbox's: each such sandbox's profile
carries a mark of its own, which the system's sandbox check confirms for
the running process, whatever program it runs; a process confined by any
other profile, Apple's or another tool's, is never touched. If the Mac's
listeners can no longer be read, Studio says so, and commands that already
listen keep their ports. A program of yours that binds `127.0.0.1` itself
instead fails to start, as the port is taken. Keep your own servers off
the listen ports, or narrow them.

The sandbox cannot keep a command, the agent's included, from binding
every address of a listen port, `0.0.0.0` too. In tests connections to the
Mac's network address were not delivered to such a server, but bind
`127.0.0.1` or `localhost` to be sure. Listening needs a build that can tell
a sandboxed process from yours (the release builds can) and needs `lsof`
to read the Mac's listeners; where either fails, commands do not listen
and Studio says so. An interactive terminal opened before you agreed keeps
its old rules. On Linux, the container and the vm tiers a server inside the
sandbox is not reachable from the host.

`/private/tmp` and `/private/var/tmp` are writable to commands on macOS by
design: tools expect a shared temp area there, so keep nothing in them a command
should not read or change.

On macOS the profile denies the LaunchServices and Apple event services
(`com.apple.coreservices.*`, `com.apple.CoreServices.*` and `com.apple.lsd.*`, the `coreservices` and `lsd` part matched in any case; the whole family is denied, which also cuts Handoff's clipboard and the shared file lists), whatever the network
setting, since an app they start runs outside the sandbox. `open` still runs
there but exits non-zero without opening the URL, file or app, and `osascript`
runs a script but cannot send Apple events to another app. Run those outside
Abhed.

Each command on the process tier is kept apart from every other: on Linux
it has its own PID namespace, and on macOS the Seatbelt profile lets it
signal only processes in its own sandbox, so `kill $PPID` cannot stop Abhed.
One consequence, on macOS as on Linux: a command cannot `kill`, or check
with `kill -0`, a process an earlier command left running, such as a server
started with `&` in one call and stopped with `kill` in the next; the
`kill` fails with "Operation not permitted". Start and stop it within one
command, or run it with `run_in_background` and stop it with `shell_kill`.

Environments are not kept apart on macOS. A command gets only an allowlist
of variables, but the kernel returns the arguments and environment of any
process of the same user (the `kern.procargs2` sysctl) that is not a
restricted program, as Apple's own are, and Seatbelt has no rule that
refuses it. So a command, a workbench terminal or a stdio MCP server can
read what an MCP server or an extension was given in its `env`, and what a
concurrent command was handed: its egress token and its
`secrets`, and any process's arguments, Abhed's included (an inline
`-settings` among them). This cannot be closed on the process tier. What
limits it: each egress token is the call's own and is revoked when the call
ends; a provider key kept as `api_key` in the configuration, not exported,
is in no environment; and the container and vm tiers give each command a
process space of its own, so use one of those where commands must not see
one another's secrets.

Abhed hides its own environment: as it starts on macOS it runs itself
again with only `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, `PWD`,
`TERM`, `TERM_PROGRAM`, `COLORTERM`, `LANG`, `LC_*`, `TZ`, `NO_COLOR`,
`__CF_USER_TEXT_ENCODING` and the Go runtime's `GODEBUG`, `GOGC`,
`GOMAXPROCS`, `GOMEMLIMIT` and `GOTRACEBACK` in the environment the kernel
shows, under the same
process id, and takes the rest back through a pipe, so a provider key
exported to it is no longer readable there. `ABHED_SHOW_ENV=1` turns this
off; Abhed also leaves its environment as it is when the rest is over
16 KiB, and warns when it cannot run itself again. Still, put a provider
key in `api_key` in `~/.abhed/config.json`, which commands cannot read,
rather than in the environment, and use the container or vm tier where commands must not
see one another's secrets. On Linux each command's PID namespace hides
other processes' environments.

On the container and vm tiers a command writes only the workspace and a
throwaway `/tmp`. The process tier is a boundary, not a jail: it is not
sufficient for genuinely hostile code.

When Abhed runs as root on Linux, the process tier drops every capability from
the command (`--cap-drop ALL` in a user namespace, with `no_new_privs`) and
binds the writable root-owned `/proc` files read-only (`core_pattern`,
`modprobe`, `binfmt_misc` and the rest), so it cannot run code as host root. The
command still runs as uid 0, so it keeps owner rights on the root-owned files it
can already write (the workspace and temp dirs), and a peer-credential check
sees uid 0; it holds no capability. The tier refuses to start if it cannot drop
the capabilities, enumerate the writable `/proc` files, or mount a private
`/proc` and `/dev` (binding the host `/dev` would expose block devices), and it
refuses `allow_network` as root — with the network on, the command would share
the host's network namespace and its abstract unix sockets, where host services
that trust uid 0 take commands. Use the container or vm tier for those cases.
Running Abhed as an ordinary user is still the recommendation.

`"max_procs"` (512 by default) bounds how many more processes a command can
start: on the process tier, as a limit of what your user runs plus this many
(the kernel counts all of the user's processes, and does not bound root), and
on the container and vm tiers, as the container's own limit. The none tier
ignores it. `"max_memory_mb"` (4096) bounds a command's memory on the
container and vm tiers and the fence. On the Linux process tier, where Abhed
runs in a cgroup delegated to you (`systemd-run --user --scope -p
Delegate=yes abhed ...`), each command runs in a cgroup of its own bounded by
`max_memory_mb` and `max_procs`, and what it leaves running is ended with
it; without one, each process's data is bounded (its private writable
memory, `RLIMIT_DATA`), a hard limit the command cannot raise, but not the
command's total, and `max_procs` is a limit on your user's processes. With
`max_memory_mb` set in a configuration file and no delegated cgroup, the
process tier is refused rather than run with memory unbounded. The macOS process tier and the none
tier do not bound memory; macOS has no cgroups and does not enforce the data
limit. `abhed doctor` warns when you set `max_memory_mb` on a tier that ignores it, when `max_procs` meets the
none tier, and when root runs the process tier, whose processes the kernel
does not bound.

`"max_cpu_percent"` bounds each container tier command's CPU, in percent of
one CPU (200, two CPUs, when unset); the fence's own bound is
`fence.cpu_percent`. A workspace file can only lower it.

`"protect_git"` keeps commands from writing what git reads in every git
folder in the workspace (config, hooks and the like; see
[Permissions](04-permissions.md)). Unset, it is on wherever the tier can hold
those files; `true` also refuses a tier that cannot (the fence without a
mount namespace); `false` turns it off for the terminal CLI, `abhed -p` and
`abhed rpc`. Studio and `abhed serve` hold them whatever it says. A workspace
file you have not trusted cannot set it.

`"terminal": "lines"` makes the workbench terminal run each line as a
policy-checked command of its own instead of an interactive shell; the
[workbench guide](16-workbench.md) says what each mode checks.
`"terminal_idle_minutes"` is how long a workbench shell nobody is watching
stays open; unset means 30.
`"language_servers"` names, per editor language, the language server the
workbench editor runs in the session's sandbox for go to definition, hover
and diagnostics (`{"go": ["gopls"]}`); none runs unless named, and a
workspace file cannot name one, trusted or not. See [The workbench](16-workbench.md#language-servers).

`"network": "allowlist"`, set only in the managed configuration, sends
commands' traffic through a per-session proxy that reaches only what the
`egress` rules allow, and records each decision; it takes the place of
`allow_network`. The process, container, vm and fence tiers enforce it, the
fence in `mount_namespace` mode and otherwise only when the managed
`fence.egress_port_only` is `true` ([below](#the-fence-tier-preview-linux)).
The `egress` section
(`rules`, `default`, `mode`, `record_paths`, `idle_seconds`, `inspect_tls`) is managed only
too. See [Network policy](21-network-policy.md).

`"microvm_socket"`, set only in the managed configuration, is the socket of
an `abhed-microvm` driver. With it, a sandbox may ask for the `microvm`
driver by name; the default driver stays `process`, and nothing else
changes. `abhed doctor` shows whether the driver answers, and is not ready
when it does not. See [The microVM driver](24-microvm.md).

With `allow_network` false, the `bash` tool's description tells the model
that commands cannot reach the network, and a command that fails for that
reason (a name that does not resolve, no route to a host) ends with a note
saying so and pointing at `web_search` and `web_fetch`. A command that exits 0
gets the note only when it ran a network client (`curl`, `wget`, `git fetch`,
`npm`, `pip` and the like) and the failure is in its last five lines, so
output that merely mentions such an error, a log being read, does not.

### The container tier's image

The container tier runs each command in `docker.io/library/debian:bookworm-slim`
unless told otherwise. It has a shell, coreutils and little else: no Go,
Node, Python or Rust, so a project's build or tests fail there with
`command not found`. This is what most Macs get, since the container tier is
picked over the process tier when an engine is running. Name an image that
has the toolchains the project needs:

```json
"sandbox": { "image": "docker.io/library/golang:1.26-bookworm" }
```

or, for one run, `ABHED_SANDBOX_IMAGE=docker.io/library/golang:1.26-bookworm abhed`.
`sandbox.image` wins over the variable, so an image the managed file sets
cannot be changed with it. A workspace file cannot set it, trusted or not:
a repository does not choose what its commands run in.

| Project | Image that has its toolchain |
|---|---|
| Go | `docker.io/library/golang:1.26-bookworm` |
| Node | `docker.io/library/node:22-bookworm-slim` |
| Python | `docker.io/library/python:3.12-slim-bookworm` |
| Rust | `docker.io/library/rust:1-slim-bookworm` |

A project that needs more than one toolchain needs an image of your own with
all of them. Whatever the image, the tier runs it the same way: no network
unless `allow_network` or the allowlist says so, a read-only root, every
capability dropped, the workspace mounted and a throwaway `/tmp`. The image
needs `/bin/sh`. Pull it once (`podman pull …`) before a session, since a
command has no network to pull it with; pin it by digest where a tag must
not move under you. The MCP servers the container and vm tiers confine run
from the same image.

`abhed doctor` looks at the workspace's top folder for `go.mod`/`go.work`,
`package.json`, `pyproject.toml`/`requirements.txt`/`setup.py`/`Pipfile`
and `Cargo.toml`, and when the container tier's image lacks `go`, `node`,
`python3` or `cargo`, says so on the `sandbox` line with the image to use.
It knows the default image has none of them; any other it looks in only
when the engine already has it, in a container with no network, no
capabilities, a read-only root and an unprivileged user, and never pulls one
to look.

### The fence tier (preview, Linux)

```json
"sandbox": { "tier": "fence", "allow_network": false, "max_memory_mb": 4096, "max_procs": 512 },
"fence": { "cpu_percent": 200 }
```

The fence is a preview. It is off unless `sandbox.tier` is `"fence"`, it runs
on Linux only (amd64, arm64 and s390x; see [Linux on Z](01-getting-started.md#linux-on-z-s390x)
for what differs there), and it fails closed: when anything it needs
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
- An ordinary user. Root only where the managed configuration sets
  `"fence": { "allow_root": true }` (a user's or workspace's file cannot),
  on Linux 6.12 or later (Landlock ABI 6). As root, each command keeps uid
  0 but drops every capability, empties its capability bounding set and
  locks the securebits that would give them back, so it cannot override
  file permissions, bind a low port or change its uid; Landlock's signal
  scoping keeps it from signalling the host's other root processes, which
  below ABI 6 it could, so the fence refuses root there. Keeping uid 0, it
  still reads every file root's own permission bits allow wherever Landlock
  grants reading, which an ordinary user's command cannot. So under `/etc`
  the fence grants root only what every user may read: `shadow`, `gshadow`,
  `sudoers`, `sudoers.d`, the SSH host keys and anything not world-readable
  (a folder: not world-searchable) are hidden, and listing `/etc` itself is
  refused. What root can still read that a user could not: root-only files
  under `/usr`, `/opt`, `/lib`, `/bin` and `/sbin`, under `/sys` (such as
  ACPI tables in `/sys/firmware`), in the host's `/proc` in `landlock_only`
  mode, and under any `read_only_paths`. `fence.socketcall` does not apply
  as root. Running Abhed as an ordinary user is still the recommendation.
- For `abhed serve` and Abhed Studio, a host that lets an ordinary user make
  a user namespace (see the modes below). Ubuntu 24.04 and later restrict
  this with AppArmor (`kernel.apparmor_restrict_unprivileged_userns`), and a
  zero `user.max_user_namespaces` turns it off.

**How to run it.** Start Abhed in a delegated scope:

```sh
systemd-run --user --scope -p Delegate=yes abhed ...
```

`abhed doctor` shows the fence's probe check by check when the fence tier is
configured: the kernel, Landlock, seccomp, no_new_privs, capabilities and the
delegated cgroup, each pass or fail with what it measured.

**Two modes.** The probe checks whether an ordinary user can have a user and
mount namespace of its own (`userns_mounts` in `abhed doctor`), and the fence
picks its mode from that; `fence.qualified` records it as `mode`, and each
`process.launched` too.

| Mode | When | What it adds | Surfaces |
|---|---|---|---|
| `mount_namespace` | the host allows unprivileged user namespaces, and a `/proc` of the namespace's own | each command in a user, mount and PID namespace of its own (and a network namespace under the egress allowlist): git's config and hooks and the surface's protected files bound read-only, the folders holding them pinned so they cannot be renamed away, the workspace's `.abhed` and other state there covered, a `/proc` showing only its own processes | the command line, the SDK, `abhed serve`, Abhed Studio (ACP) |
| `landlock_only` | it does not | nothing: Landlock and seccomp alone | the command line and the SDK; Studio and `serve` are refused, saying why |

The fence never moves from one mode to the other on its own: a surface that
needs read-only paths is refused where the mounts are not available, and a
command whose mounts cannot be made exits 126 having run nothing.

**What it does.** Abhed re-executes itself as a small launcher for every
command. In order, the launcher checks that this run's probe qualified the
host; in the `mount_namespace` mode, makes its mounts, with `CAP_SYS_ADMIN`
(and `CAP_SETPCAP`, for the next step) in its own user namespace and nothing
else, then empties its capability
bounding set, locks the securebits that keep uid 0 from gaining any, and
drops that capability (in `landlock_only` an ordinary user cannot change
the bounding set, which stays full; no_new_privs and holding no capability
keep anything in it from being gained); checks it holds no privileges; joins a cgroup made for the tool call, confines
itself with Landlock and a seccomp filter, reports what it applied, and waits.
Abhed writes `process.launched` to the record, and only then lets the launcher
run the command in its place. A launcher that cannot do any step exits 126
without running anything.

- **Files.** A command reads and runs the system folders (`/usr`, `/bin`,
  `/lib`, `/etc`, `/opt` and the like) and `sandbox.read_only_paths`, reads
  `/proc` and `/sys`, and writes only the workspace and a private temp folder
  of the session's. `HOME`, `TMPDIR`, `XDG_CACHE_HOME` and `npm_config_cache`
  point into that folder. `~/.abhed/skills` and the folders in `skills.dirs`
  are readable and their scripts run, but cannot be written, so a skill's
  script works. A `skills.dirs` folder that holds or sits inside Abhed's state
  (your home, say, or `~/.abhed/records`), or one inside the workspace, is
  left out, and `fence.qualified` lists it under `skills_left_out`. The rest
  of your home, `~/.abhed`, the record and the secrets are out of reach.
- **Read-only paths in the workspace** (`mount_namespace` only). Abhed
  Studio keeps git's config and hooks and its editor's files read-only; each
  is bound read-only over itself in the command's namespace, and the folders
  holding them (`.git`, `.vscode`) are bound over themselves, so they cannot
  be renamed or removed to get around it. A command can still commit: only
  the config and hooks are held. The protected paths are those that exist
  when each command starts, and git folders are found then, as the process
  tier finds them: one a command makes while it runs is not held during that
  command, only from the next. A protected path that is a symbolic link, a
  file with a second name (a hard link), or a protected folder holding a file
  with a name outside it (a hook hard-linked to a file in the workspace, say)
  refuses the command, since the file would stay writable through the other
  name.
- **Other mounts of the workspace's filesystem** (`mount_namespace` only).
  The same files can be mounted at a second path: on an ostree host (Fedora
  CoreOS, Silverblue, Kinoite) `/var`, and so `/home`, is also mounted at
  the deployment's `var` folder under `/sysroot/ostree`, and a bind mount
  does the same. The
  launcher reads the namespace's mounts and applies the same read-only binds
  and the same tmpfs at every other mount of the workspace's filesystem that
  shows the workspace or part of it, so the protected files stay read-only
  and `.abhed` stays covered through each; `fence.qualified` lists them as
  `aliases`. One behind a folder you cannot search and do not own is left
  as it is and listed as `aliases_unreachable`: a command, running as you,
  cannot pass that folder either. It stays out of reach only while the
  folder's owner (or root) keeps it closed. If they open it while a command
  runs, that command can reach the real `.abhed` and the protected files
  through it until it ends; the next command covers it. A long-lived
  command (the terminal, `abhed serve`'s commands) is not checked again
  while it runs: its namespace can be changed only from inside it, after it
  has dropped every capability, and a mount made later would not take back
  what it had already opened. One the fence cannot check, such as a second mount
  whose path now leads somewhere else, refuses the fence when it qualifies,
  or the command (exit 126) when it starts, saying which. A view of the
  workspace through another filesystem, which the covers cannot reach, is
  refused the same way, when the fence qualifies and again before each
  command: a FUSE mount (bindfs, sshfs to this host), an overlay with the
  workspace, a folder in it or one holding it as a layer, or a network
  share (NFS, CIFS, 9p, virtiofs) of this host's folder, each found by what
  its mount names as its source or layers. A FUSE program that serves the
  workspace's files without naming the folder in its mount, or a share
  named by another host's address that is this one, is not found; `abhed
  doctor` shows the refusal for those it finds. A command cannot make such
  a mount itself: it holds no capability and cannot gain one.
- **Abhed's state in the workspace, `mount_namespace`.** The workspace's
  `.abhed` is covered by an empty tmpfs in each command's namespace: what
  Abhed keeps there is out of sight, and whatever a command writes there is
  gone when it ends, so nothing it writes there ever reaches Abhed. Where the
  workspace has no `.abhed`, the fence makes an empty one to mount over and
  leaves it in place while other fences on the same workspace (two Studio
  tabs, Studio beside the command line) may mount over it too, since
  removing it would uncover their commands. Each fence holds a shared lock
  in `~/.abhed/fence/` while it lasts, and the last to close removes the
  folder when it is still the same one and was empty from the start, so none
  is left behind. A fence of 1.2.7 or earlier takes no lock, so run it on its
  own workspace. A `.abhed` that was
  empty when the session started must stay empty while it runs: anything
  that appears in it is taken out of it one entry at a time, each first
  renamed inside that `.abhed`, where every fence's tmpfs still hides it,
  then moved to `~/.abhed/quarantine/`, or removed where it cannot be moved
  (one that can be neither stays there renamed, is reported as still
  present, and refuses the next fence until you remove it);
  the folder itself is left in place for the other fences, and a folder made again
  in its place is taken out whole; either way the session runs no further
  command. So put state there (`abhed user add`, say) before a fenced
  session starts. A `.abhed` that cannot be listed when the session starts
  refuses the fence, since it is not known to be empty. One that already
  held state is Abhed's own, and stays covered. Abhed refuses to start with a state file
  (`auth.users_file`, the secrets file) inside the workspace outside `.abhed`,
  so there is none to hide; another configuration folder's `.abhed` inside
  the workspace is covered as the workspace's is. Where the workspace's disk
  ignores case, every spelling of `.abhed` (`.ABHED`, `.Abhed` and so on) is
  covered by the same empty tmpfs; on a disk that keeps case apart, a folder
  of another spelling is not state, and one a command makes is still taken
  out as below.
- **Abhed's state in the workspace, `landlock_only`.** Landlock cannot keep a command from
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

  In `mount_namespace` the same check runs as defence in depth, for the
  spellings the tmpfs does not cover. In `landlock_only` the check is not a guard. A file a command writes into the workspace's
  `.abhed` is there while it runs and until the next check, and Abhed can
  read it then (for example its users or memory files); in `landlock_only`
  the check also runs every second while the session is open, so that
  window is about a second. After an unclean exit (Abhed killed, the machine
  down), the next fenced session on the workspace looks at what the last one
  left: each session keeps a mark in `~/.abhed/fence/` that a clean close
  removes, and where a session's process is gone, it had no `.abhed` when
  it started, and the `.abhed` there now last changed (by its ctime, which
  a command cannot set) no later than its last command, that `.abhed` is taken out as above and recorded with `when:
  after_unclean_exit`. Nothing is taken while another session on the
  workspace still runs. A `.abhed` you make after such an exit, before the
  next session, is left. A folder added with `--add-dir` is read-only to
  fenced commands, so none can be made there. A `.abhed` in a subfolder of
  the workspace is not checked: it is not this session's state, but it would
  be a later session's started in that subfolder, so look before starting
  one there.
- **Terminals.** Of the devices, a command opens only `/dev/null`,
  `/dev/zero`, `/dev/full`, `/dev/random`, `/dev/urandom` and `/dev/tty`,
  which reaches only its own terminal. Each command runs in a session of
  its own, so a command not started on a terminal, such as those `abhed
  doctor` runs, has none, and `/dev/tty` never reaches Abhed's. It keeps
  stdin, stdout and stderr through the descriptors it was given, but cannot
  open `/dev/ptmx` or any `/dev/pts/*`, so it can neither read nor write your
  other terminals.
- **System calls.** The seccomp filter (profile `command/4`) refuses ptrace,
  namespaces, mounts, bpf, kernel keyrings, loading modules and a filter of
  the command's own. It also refuses signals aimed at Abhed: `kill`,
  `tkill`, `tgkill`, `rt_sigqueueinfo` and `rt_tgsigqueueinfo` naming Abhed's
  process id, `kill` of its process group or of every process (`kill -1`),
  and `pidfd_send_signal` altogether, since a filter cannot see whom a pidfd
  names. Each command also runs in a process group apart from Abhed's.
  In `mount_namespace` mode, on Linux 5.7 or later, each refused call is
  sent to Abhed (seccomp user notification), which answers it with the same
  EPERM and counts it; the command, in its own PID namespace, cannot see
  the listener to answer its own. `process.launched` says
  `seccomp_notify: true` then. If Abhed is gone the kernel refuses the call
  with ENOSYS instead. In `landlock_only`, or on an older kernel, the filter
  refuses with EPERM itself and nothing is counted.
- **Network.** All or nothing. With `allow_network` false every `socket()` is
  refused, and Landlock refuses TCP as well (plain TCP only: since Linux 6.14
  Landlock leaves MPTCP and SCTP alone, so the `socket()` refusal is what
  stops those). With it true, commands get the host's network, unfiltered.
  Unix sockets are refused either way. On s390x the filter also refuses
  every socket made through `socketcall()`, which is how glibc and Go make
  them there, so most programs get no network, unless
  `"fence": {"socketcall": true}` is set with `allow_network` on a host with
  Landlock ABI 6 or later: then `socketcall()` sockets are allowed, and since
  the filter cannot read their family, unix sockets with them (abstract ones
  scoped by Landlock, named ones not). A workspace file cannot set it.
  Under `sandbox.network: "allowlist"` a command may make inet and inet6
  TCP stream sockets only (no UDP, unix, MPTCP or SCTP sockets, the last two
  being streams Landlock's TCP rules do not hold), and Landlock lets TCP
  connect only to the session's egress proxy's port. In `mount_namespace`
  mode each command also gets a network namespace of its own holding only its
  loopback, where the launcher relays that port to the proxy's socket on the
  host, so the proxy is the one way out; there UDP sockets are allowed too,
  for the session's resolver, as they reach only that loopback. In `landlock_only` there is no
  network namespace, and Landlock filters ports, not addresses, so a command
  could reach that port number on any host; the fence refuses the allowlist
  there unless the managed `fence.egress_port_only` is `true`, which accepts
  that. Names are resolved by the proxy, not inside the sandbox
  ([Network policy](21-network-policy.md)).
- **Limits.** Each session's commands share a cgroup bounded by
  `max_memory_mb` (with no swap), `max_procs` (processes and threads) and
  `fence.cpu_percent` (percent of one core; unset is unbounded), and each tool
  call gets a cgroup of its own with the same bounds. When a command exits,
  whatever it left running is ended.
- **Sessions.** Each Studio session and each `abhed serve` session has a
  fence of its own, with its own cgroup that its tool calls' cgroups sit
  under, closed with the session. A served session qualifies its fence at
  its first command, so a session opened only to be read records nothing.
  In `mount_namespace` mode each command runs in a PID namespace of its
  own, as the child of the launcher, which stays as the namespace's init: it
  passes on the signals sent to it (a stop, Ctrl-C), reaps what is orphaned
  and exits as the command does, which ends everything left in the
  namespace. The command sees, signals and traces only its own processes,
  through a `/proc` of its own. Where the kernel refuses a new `/proc`, as
  in a container that masks parts of it, the probe fails `userns_mounts`, so
  the fence runs in `landlock_only` or is refused, and a launcher whose
  mount is refused all the same does not run the command. In `landlock_only` there is no PID
  namespace: below Landlock ABI 6 (Linux older than 6.12) one session's
  commands can signal another session's and read their command lines in
  `/proc`, so an `abhed serve` that accounts sign in to (`auth.mode` other
  than `none`) refuses the fence there below ABI 6, saying so.
- **The record.** `fence.qualified` holds the probe's report and the mode
  at the start of the session (for `serve`, before the session's first
  command), `process.launched` each command's launch with its call id
  before it runs, `fence.limit` a command that ran into its memory or
  process limit (`pids_max` the forks refused in its own cgroup,
  `session_pids_max` those refused at the session's limit while it ran,
  which on Linux 6.14 and later are counted only there and may include
  another command's), `fence.denied` the system calls seccomp refused a
  command, counted by name (`calls`, such as `"socket(unix)": 2`), when it
  sends them to Abhed, and `fence.state_planted` a `.abhed` found in the workspace.
  `process.launched` names its `source`: `call` for a tool call or a `!`
  command, `harness` for one Abhed runs itself within a session. When
  `fence.qualified` cannot be recorded, the command line closes the fence
  and runs no command, and an SDK agent is not built.

**What it does not cover.** Abhed itself and what it runs in-process (the
file, web and MCP tools, extensions) stay outside the fence, as on every tier.
There is no network filtering beyond the egress allowlist above. In
`landlock_only` mode, below Landlock ABI 6 (Linux 6.12) a command can signal
your other processes, and can reach Abhed by one of its thread ids, which the
filter does not know; Landlock ABI 6 keeps signals inside the command's own
processes; and a command can read other processes' command lines in `/proc`
(not their environment or memory). The Landlock ABI 6 paths (signal
scoping, root) were tested only on Linux 6.9, where they are refused. Abhed
refuses to start when one of its state files has a second name
([Permissions](04-permissions.md#what-the-agent-may-touch)). A hard link
made before the session to another file in a state folder, one the state
check does not know, stays readable through the link: Landlock checks the
path a file is opened by. The
planted-state check above has the limits it states in `landlock_only`: a
file written into the workspace's `.abhed` is visible while its command
runs, nothing is moved after an unclean exit, and a `.abhed` in a subfolder
or an added folder is not checked. In `mount_namespace` the first two do not
arise, and a `.abhed` in a subfolder or an added folder is not covered. The
mounts rely on Landlock's own scoping for one thing: a command cannot reach
Abhed's view of the files through `/proc/<pid>/root`, since Landlock refuses
a sandboxed process access to processes outside its domain.

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
- In `landlock_only`: a workspace with a `.abhed` folder (the empty one a
  `mount_namespace` fence leaves included; remove it), and the surfaces
  that keep paths inside the workspace read-only (Abhed Studio's git and
  editor protections) or need the mounts (`abhed serve`). Landlock alone
  cannot keep part of a writable folder read-only, so they are refused.
- In `mount_namespace`: programs that expect files outside your own to show
  their owners (they show as `nobody`), and anything that writes git's config,
  such as `git config` without `--global` or a hook manager installing hooks.
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

Once on, every search asks in the default, accept-edits, auto and plan
modes, as `web_fetch` does without a host list: a query can carry what the
agent read to the search service. An allow rule runs searches unasked, such
as `"permissions": {"allow": ["web_search"]}` in the managed file, and
bypass mode runs them. Before 1.2.8 a search ran unasked as a read-only
tool.

Every other layer, `~/.abhed/config.json`, `-settings`, a workspace's
`.abhed/config.json` trusted or not and an SDK `ConfigDir`, may only turn it
off or narrow it. No `ABHED_*` variable sets either section:

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
| macOS | the unified log, written by `abhed` itself through `os_log`, subsystem `com.zybuu.abhed`, category `admin` | `log show --last 7d --predicate 'subsystem == "com.zybuu.abhed"'` |
| Linux, journald | the journal, through its native socket, identifier `abhed-admin`, facility auth | `journalctl -t abhed-admin` (as root or a member of `adm` or `systemd-journal` to see every user's attempts); `journalctl -t abhed-admin -o verbose` shows the `_UID` journald attached itself |
| Linux, no journald | syslog, tag `abhed-admin`, facility auth | `grep abhed-admin /var/log/auth.log` (Debian, Ubuntu) or `/var/log/secure` (RHEL, Fedora) |
| Windows | the Application event log, source `abhed-admin`, event id 1 | `Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='abhed-admin'}` |

**Telling real entries apart.** The tag is not a credential: any local user
can add a line tagged `abhed-admin` to the system log with `logger -t
abhed-admin ...`, with whatever `uid=` and `result=` they like in its text. The fields inside the
message are what `abhed` wrote only when the log itself says who sent it:

- **Linux, journald:** `journalctl -t abhed-admin -o verbose` shows the
  trusted fields journald adds itself, beginning with `_`. A real entry has
  `_COMM=abhed` (a forged one usually `_COMM=logger`, though a user can
  name their own program `abhed`) and a `_UID` that
  matches the `uid=` in the message; for a change, `_UID=0`.
- **macOS:** the system records the sender of each entry itself. `log show
  --last 7d --style json --predicate 'subsystem == "com.zybuu.abhed"'` gives
  each entry's `processImagePath`, which must be the `abhed` binary you
  installed, and `userID`, which must match the `uid=` in the message and is
  `0` for a change. Another program can use the same subsystem name, but not
  the installed binary's path.
- **syslog files** carry no sender the system vouches for; trust them only as
  far as `admin.jsonl` beside the managed file, which only root can write.
- **Windows:** any program can write an event under the source name
  `abhed-admin`, and the event carries no sender the system vouches for;
  trust it only as far as `admin.jsonl`, which only an administrator can
  write. The source is not registered, so Event Viewer notes that the
  description is missing and shows the message as the event's data.

A macOS `abhed` built without cgo (`CGO_ENABLED=0`, as `make build` does)
cannot call `os_log`: it writes through `/usr/bin/logger` instead, with tag
`abhed-admin`, and warns once that its entries are not attributable to
`abhed`. Find those with `log show --predicate 'process == "logger" AND
eventMessage CONTAINS "abhed-admin"'`, where only `userID` tells them apart.
The release binaries for macOS are built with cgo.

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
you used. Web fetch has no command: edit `/etc/abhed/config.json` by hand
with sudo and add the `web_fetch` section. Then remove the section from your own
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

One exception to "off by default": a web page a person links in Abhed
Studio is read as that person's own `web_fetch` call even while `web_fetch`
is off, under the same deny rules, address and secret checks. It asks as
above when no `allowed_hosts` names the site, unless an allow rule names
it. A user's or workspace's `web_fetch.enabled: false` does not stop it;
only the managed file's own `"enabled": false` does. See
[Input and memory](19-input-and-memory.md#in-abhed-studio).

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

## Verified answers

```json
"verify": {
  "mode": "strict",
  "answer_tool": true,
  "critic": {"enabled": true, "model": "reviewer"},
  "contract": {"require": ["go test ./..."], "protect": ["*_test.go", ".github/**"]}
}
```

Each answer's statements are checked against what the session observed and
labelled verified, unverified or contradicted. The whole of it is in
[Verified answers](29-verified-answers.md).

| Key | Default | |
|---|---|---|
| `mode` | unset | `off`, `label` or `strict`. Unset is `label` in the terminal, the workbench and console, and Abhed Studio, and `off` for `-p`, `rpc`, unattended runs and the SDK. `label` adds a model call only when a statement is contradicted, for one revision turn. `-verify`, rpc's `start`, a session's `verify` and `Options.Verify` set it per run, and may only make a managed value stricter. A workspace file may only raise it, never below `label` |
| `answer_tool` | on in `strict`, off otherwise | offers the agent the `answer` tool, which carries each claim's evidence. A workspace file may only turn it on |
| `critic.enabled` | `false` | a model checks each verified claim against its evidence and may only downgrade it; it fails closed. One model call per answer with a verified claim |
| `critic.model` | the session's | a configured provider to ask instead. Neither it nor `critic.enabled` is taken from a workspace file until it is trusted |
| `critic.timeout_ms` | `30000` | how long the critic may take |
| `contract.require` | none | commands the harness runs in the sandbox after the last change; each must exit 0 for the run to end `completed`. Not taken from an untrusted workspace file |
| `contract.protect` | none | globs of files a run must not change. A workspace file may add to them |
| `contract.timeout_ms` | the bash tool's | how long each required command may take |

A run that strict mode or a contract cannot verify ends `unverified`, and
`abhed -p` exits 8.

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

`record.ocsf` writes the local record to a rotated JSONL file of OCSF events
as it is recorded: `enabled`, `version` (`1.8.0`, `1.5.0`, `1.3.0` or
`1.1.0`), `max_mb` and `keep` may be set in your own file; `path` and
`locked`, which makes the managed `record.ocsf` the whole of it, only in the
managed file ([OCSF events and logs](30-ocsf-and-logs.md#the-ocsf-file)).

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
that changes or removes one. On `sessions` it may update only the columns a
session's life changes, never its id, tenant, parent or start; its owner
(`user_id`) only to the unclaimed form when the account is removed, which a
trigger enforces. A session's key in `session_keys` can be deleted only
once the session is marked deleted, and that mark is never cleared or
rewritten, so the runtime role cannot make a live session's record
unreadable. Every owner change is written to `owner_changes`, and every
change to an account's groups or tenant to `account_changes`, by triggers
that run as the tables' owner: the runtime role can read those tables but
neither write nor skip a row. The OCSF file's cursors in `ocsf_cursors`
(`record.ocsf` on `abhed serve`) are read, inserted and moved forward,
never removed. Keep the owner's credentials off the host that
runs the server; whoever holds them can alter the record (see
[the Postgres record](12-records.md#the-postgres-record) for the chain and
witness that show it). After an upgrade
that adds columns, run `abhed migrate` again before starting the server: on a
schema older than the binary it refuses to start and names what is missing.

The session owner move runs once. Its `--owners` (`local-only` or
`unclaim`; another value exits `2`) has no effect on a later run, which says
so. `local-only` refuses when it finds no account while old sessions exist,
and then records nothing: import the accounts first (`abhed user import`),
or pass `--owners=unclaim` or `--force-no-accounts`. Like `serve`, `migrate`
refuses an `auth.users_file` in a place commands can write, such as the
workspace or a temp folder.

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

**The runtime role must not create in schema `public`,** by any route: a
role that can add a function or operator there can stand in for one the
record's triggers call. Every role the runtime role belongs to counts,
with or without `INHERIT`, since `SET ROLE` reaches them all, and so does
owning the schema (on Postgres 15 and later, owning the database). Postgres
13 and 14 grant `CREATE` on `public` to `PUBLIC`, so there the owner role
usually creates in `public` only through that grant, and the schema
belongs to the superuser. The fix is two statements, in this order, and on
13 and 14 it is a superuser step:

```sql
GRANT CREATE ON SCHEMA public TO abhed_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
```

`abhed migrate` runs both itself where its role may (it owns `public` or
inherits from the role that does), and never revokes `PUBLIC`'s grant while
the owner role would be left without `CREATE`. Otherwise it refuses and
prints the statements to run, the first only when the owner needs it,
naming each route the runtime role has (`PUBLIC`, a grant to it, a role it
belongs to, ownership); a runtime server that can create there refuses to
start with the same message. The revoke is database-wide: if other
applications share the database on Postgres 13 or 14 and create in
`public` through `PUBLIC`, grant their roles `CREATE` first. On a managed
Postgres service where neither the schema's owner nor a superuser is
reachable, ask the provider to run the statements, or use
`storage.single_role` and accept the weaker guarantee it describes above.

**Do not connect as a superuser,** in either mode. Row-level security is what
isolates tenants, and Postgres does not apply it to a superuser or a
`BYPASSRLS` role, not even with `FORCE`. Abhed checks and refuses, because a
control that is silently off is worse than one that is visibly missing.

### Backing up and restoring

`scripts/backup/abhed-backup.sh` writes a logical dump of the database with
`pg_dump` and a tarball of the state folders you name (the server's
`~/.abhed`, `/etc/abhed`). Dump as a role that reads every tenant's rows past
row security, not as the runtime role:

```sql
CREATE ROLE abhed_backup LOGIN PASSWORD '…' NOSUPERUSER BYPASSRLS;
GRANT pg_read_all_data TO abhed_backup;
```

Give the connection string without its password and put the password in a
`PGPASSFILE` (or `~/.pgpass`) or `PGPASSWORD`: the scripts refuse a
connection string that carries one, since the database clients would show it
to every local user in the process list. Use client tools of the server's
major version. `abhed-backup.service` and `abhed-backup.timer` beside the
script are an example schedule.

`scripts/backup/abhed-restore.sh` restores a dump, as a superuser, into an
empty database, in one transaction that stops at the first error, and
refuses a database that already has tables. `ABHED_RESTORE_CREATE_ROLES`
names the owner and runtime roles to make if they are missing, as plain
login roles without a password; it changes no role that exists and makes no
other. A full dump creates the record's triggers after its rows are loaded,
so every chain position, hash and `commit_tx` is kept as it was. Never
restore with `--data-only` into a database that already has the schema: the
record's triggers then run on every row, and the restore stops at the first
event.

Then, before any server starts, run `abhed migrate` as the owner, and `abhed
record verify` and `abhed record audit`. The script runs all three when
`ABHED_BIN`, `ABHED_MIGRATE_DATABASE_URL` and `ABHED_DATABASE_URL` are set.
Restored into a cluster whose transaction counter is behind the one the dump
came from, `abhed migrate` raises `commit_shift` so the OCSF file's cursor
and an edition's SIEM delivery go on without skipping new events. A runtime
server started on such a database before the migrate refuses, naming
`abhed migrate`.

A physical copy (`pg_basebackup`, or WAL archiving) keeps transaction ids
as they were and needs none of this beyond the two checks. With
`record.witness`, a restore from before the witness's last head fails
`abhed record verify` for the sessions written since: that is the record
showing the restore.

`scripts/backup/restore-test.sh` checks all three ways (a logical restore
into the same cluster, into a fresh one whose counter is behind, and a
physical copy) in throwaway containers, with the restore script running the
migrate and both checks and restoring a state tarball. The OCSF file's
cursor is in the database and goes back with it; lines the file already
holds for events after the backup point stay in the file, though those
events are not in the restored database.

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
session are closed and their results recorded before its end. Alongside the
drain, idle sessions are ended and their claims given back all together,
within 15 seconds; a release the store refuses is tried again within ten
seconds, and one that never lands lapses when its claim goes stale. It then
waits up to five seconds for turns to record their end, and up to ten more
for open HTTP requests; the streams a page holds open (session events,
terminals, language servers and the workbench's shared stream) are ended
then rather than waited for, so an open workbench tab does not hold the
server up. The worst case is `drain_seconds` + 15 seconds, or
25 seconds when `drain_seconds` is under 10, so give the process at least
that much grace: with Kubernetes' default `terminationGracePeriodSeconds` of
30, keep `drain_seconds` at 15 or less.
Without `drain_seconds`, running turns are ended at once
([Sessions](11-sessions.md#continuing-a-finished-chat-on-any-node) says what is
recorded). Moving them to another replica instead is part of the Enterprise
edition's high availability. SIGINT and a
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

## Metrics and the API audit

```json
"server": { "metrics": { "enabled": true, "listen": "127.0.0.1:9464" } }
```

`abhed serve` serves Prometheus metrics only when `server.metrics.enabled` is
set; off, nothing is counted. With `listen` set they are on that address
alone, at `/metrics`, apart from the API; a `listen` other than a loopback
address needs `token_env`, the name of an environment variable holding a
bearer token the scraper sends, or the server refuses to start. Without
`listen` they are at `/metrics` on the API's own address, for administrators
only (the `auth.admin_group`), so a server with sign-in off has nobody who
can read them there and should use `listen`.

What is counted, all from the session record, the control plane's audit and
the API, never from a command, a path, a host or anything a person or model
wrote:

| Metric | Labels |
|---|---|
| `abhed_policy_decisions_total` | `decision` (allow, ask, deny), `step`, `tool` |
| `abhed_approvals_total` | `result` (approved, rejected, unanswered, interrupted, withheld, owner, ...), `by`, `tool` |
| `abhed_supervisor_verdicts_total`, `abhed_supervisor_judge_seconds` | `code`, `before`, `after` |
| `abhed_supervisor_corrections_total`, `abhed_supervisor_replans_total` | `code` |
| `abhed_egress_decisions_total`, `abhed_egress_bytes_total` | `kind`, `decision`; `direction` |
| `abhed_tool_duration_seconds` | `tool`, `outcome` (ok, error, not_run, unknown) |
| `abhed_model_calls_total`, `abhed_model_call_seconds`, `abhed_model_first_token_seconds`, `abhed_model_tokens_total` | `model`, `purpose`, `outcome`; `kind` (in, out, cached) |
| `abhed_plan_decisions_total`, `abhed_approval_scopes_granted_total`, `abhed_answer_claims_total` | `decision`, `via`; `verdict` |
| `abhed_sessions_started_total`, `abhed_sessions_ended_total`, `abhed_sessions_live` | `reason` |
| `abhed_sandbox_operations_total`, `abhed_sandbox_bound_total` | `action`, `outcome`; `driver`, `tier` |
| `abhed_fence_limits_total`, `abhed_sandbox_planted_total` | `limit`; `what` |
| `abhed_fence_denied_total` | `source` (the fence tier's system-call profile, as its `fence.denied` names it), `call` (a system call that profile refuses) |
| `abhed_lsp_starts_total` | `language` (`go`, `python`, `typescript`, `javascript`, `rust`, or `other`): language servers started for editors; a restart after a crash counts again — [Language servers](33-language-servers.md) |
| `abhed_lsp_messages_total` | `language`, `outcome` (`passed`, `refused`): editors' messages to language servers, counted when each server ends |
| `abhed_ollama_actions_total` | `op` (`start`, `install`, `pull`), `outcome` (`ok`, `failed`, `declined`, `refused`, `cancelled`, `manual`): a person's attempts to get a local Ollama model working — [A local Ollama model](03-providers.md#a-local-ollama-model) |
| `abhed_record_write_seconds`, `abhed_record_write_errors_total`, `abhed_record_repairs_total` | `record` (events, audit) |
| `abhed_audit_events_total` | `source` (api, sandbox), `outcome` |
| `abhed_http_requests_total`, `abhed_http_request_seconds` | `route` (the pattern, such as `POST /v1/sessions/{id}/messages`), `method`, `code` |
| `abhed_config_refused_total` | `kind` (refused, narrowed) |

A label takes the values its list names, or a configured name (a tool, a
model, a driver); anything else is counted as `other`, and each metric keeps
at most 500 label sets. Egress decisions Abhed makes for its own requests
outside any session, and anything an exporter outside this process does, are
not counted here.

Every API request that changes something (any method but GET, HEAD and
OPTIONS) and reaches its route is written to the control-plane audit record
with its principal,
before it runs and again with its outcome: its action is `api:` and the
route pattern, and its detail the method, the route, the tenant, the
caller's address and what the route's wildcards took, never the body or the
query. A request whose first event cannot be written is refused with 503
and nothing is changed. What is audited is what got past sign-in: a request
turned away before it reaches a route (refused sign-in, a cross-origin
request, the rate limit, a required group or password change) changes
nothing and is in the request log, not the audit. Sign-in routes record
`anonymous`, since nobody is signed in yet. The record is the tenant's `sandbox_audit` table on Postgres,
chained per tenant beside the sandbox operations, or
`<records>/<tenant>/audit/api.jsonl` (under `record.dir`, or
`~/.abhed/records`) without it, chained line by line the same way.
`abhed record audit` checks the chain, and with the control plane on
SQLite also the control plane's own record (its sandbox operations and the
reconciler's events), chained the same way since this release; rows written
before are counted as unchained. The outcome event of a POST that answers
with JSON names what it created, from the answer's `id`, `session_id` or
`name` field when that is a short identifier (`created.id`, ...), so a new
session or sandbox can be found by its id; nothing else of the answer is
kept.

Left out: the two routes that only open and close a channel of the
workbench's read-only stream, and requests to a sandbox's exposed service
(under `/v1/exposed/`), which are traffic to a person's own app rather than a
change to the control plane; the exposure itself is made and removed through
audited sandbox routes, and the app's paths and tokens are kept out of the
audit. The metrics count those requests by the prefix alone, and a request's
method is counted only if it is a standard HTTP method (`other` if not). Two more get one event a request, the
`requested` one, and no outcome, since a client calls them every few keys:
a terminal's input (`POST /v1/sessions/{id}/pty/{pty}/input`) and a message
to a language server (`POST /v1/sessions/{id}/lsp/{lsp}`). A refused write
still stops them. What was typed is not in the audit: a shell's lines are in
the session record as `terminal.input`, by the person; what a program reads
is recorded nowhere, since it may be a password typed with echo off.

What the audit costs on Postgres, measured with the opt-in
`ABHED_AUDIT_LOAD=1 go test ./store -run TestAuditWriteLoad` against
Postgres 16 in a podman VM on an Apple M3 Pro laptop, the server's default
pool of 10 connections, one tenant, 1,000 writes per client:

| Concurrent writers | Writes per second | p50 | p95 | p99 | max |
|---|---|---|---|---|---|
| 1 | 2,871 | 0.33 ms | 0.39 ms | 0.96 ms | 1.6 ms |
| 5 | 4,451 | 1.05 ms | 1.5 ms | 2.1 ms | 9.4 ms |
| 20 | 4,211 | 4.5 ms | 5.9 ms | 10.4 ms | 49 ms |

A tenant's writes share one chain lock, so they do not get faster past a few
writers: at 20 a write waits about 4.5 ms, and a request, which writes
twice, about 9 ms more than it did. That is one machine with no network
between server and database, not a promise; a database across a network
adds its round trip to each write.

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

A change to an account reaches its live sessions on their next request with
a users file, and within about 2 seconds over Postgres, whichever process
made it: an account removed with `abhed user remove` is
signed out, and a group added or removed applies at once. Removing
administrator rights through `POST /v1/admin/users/admin` also ends that
person's sessions, on every server sharing the account store: at once on the
one that removed them, and within about 2 seconds on the others over
Postgres. An event or terminal stream already open is authorised again while
it runs, and ends when its sign-in would now be refused: at once for a change
made on the same server, within 10 seconds otherwise.

Signing out ends sign-ins, not work. After Sign out everywhere, a background
shell or task the person started keeps running, and its result still starts
a woken run in their session, since the account is still active. Stop the
session's tasks to end it; see
[background tasks](14-parallel-subagents.md#background-tasks). Removing the
account, or its administrator rights, stops that work, recorded as
`owner_revoked`.

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
`record.witness`, `record.ocsf.path`, `record.ocsf.locked`, `hooks.disabled`, `hooks.managed_only`, `cli.mode_cycle`,
`studio.disable_host_terminal`, `sandbox.network` (which your own file may
use only to narrow `allow_network`), `sandbox.microvm_socket`,
`fence.egress_port_only`, every key under `egress`,
`control_plane.kubernetes`, `control_plane.interceptors`, every key under
`control_plane.docker` and `control_plane.podman` but `enabled`, `image` and
`name_prefix`
([Sandboxes over the API](22-sandboxes.md#the-docker-and-podman-drivers)),
and the whole of `web_search` and `web_fetch`, which a lower layer may only
turn off or narrow ([Web search](#web-search)), and, of the settings below
that the paid editions read, `auth.bound_claims`,
`control_plane.ha.relay_peer` and `control_plane.ha.relay_key_command`. A lower layer's value the managed file replaces
is recorded too, as `config.refused` with the decision `overridden`.

The Community Edition checks these settings and the paid editions act on
them; each was an environment variable first, which still applies where the
configuration leaves the key unset and never overrides it:

| Key | Variable | Meaning |
|---|---|---|
| `auth.bound_claims` | `ABHED_OIDC_BOUND_CLAIMS` | managed only: an object of claim names to lists of values a bearer token from the identity provider must carry, exactly or, for a value ending in `*`, by prefix; every claim needs a value, and `*` alone is refused |
| `control_plane.ha.relay_cert_file`, `relay_cert_key_file` | `ABHED_HA_RELAY_CERT_FILE`, `ABHED_HA_RELAY_CERT_KEY_FILE` | this replica's client certificate for relayed calls; both or neither. The key is state the agent cannot reach |
| `control_plane.ha.relay_peer` | `ABHED_HA_RELAY_PEER` | managed only: the identity the other replicas' certificates must carry; needs the certificate |
| `control_plane.ha.relay_key_vault` | `ABHED_HA_RELAY_KEY_VAULT` | the relay key from Vault KV, `entry#field` |
| `control_plane.ha.relay_key_command` | `ABHED_HA_RELAY_KEY_COMMAND` | managed only: a program and its arguments, the program an absolute path, that prints the relay key |

Inside an agent's command, a nested `abhed` ignores `ABHED_TRUST_WORKSPACE`
with a warning, and refuses `-trust-workspace`, `-settings` and
`-mcp-config` whatever they hold: either can name an endpoint the nested
session would send the code or its queries to. It also refuses a `-mode`
looser than the configured mode (`accept-edits` or `auto` when the
configuration says `default`), not only `bypass`. A nested run narrows with
`-mode plan`, `-disallowedTools` or `-max-turns`.

### The managed file

`/etc/abhed/config.json` belongs to the organisation. It is read last, so each
key it sets replaces what the user and project files said. An organisation can
also sign a policy on its server for every laptop that signs in to it; that
[signed org policy](35-org-policy.md) is laid under this file, which wins
where both make a setting, and the two together are the managed
configuration this section describes. A list it sets,
such as `permissions.allow`, replaces the lower files' list; so does a map
entry, such as one provider under `model.providers`. `permissions.deny` and
`permissions.ask` are the exceptions: the managed rules are added to the
deny and ask rules already in force (the built-in ones,
`~/.abhed/config.json`, `-settings` and a trusted workspace's file), so a
managed file can add a deny rule or a question but never take one away. `/permissions` shows each rule with the layer it came from. Its
presence makes the policy engine managed: `bypass` mode is refused wherever
it comes from. A call that bypass would approve asks as in default mode, and
the reason names the managed file; `abhed doctor` shows the mode as
`bypass (bypass disabled by /etc/abhed/config.json; …)` and the `abhed serve`
banner prints the same on a `mode` line.

Abhed refuses to start on a managed file with a key repeated in one object
(`"permissions"` twice, or `"permissions"` and `"PERMISSIONS"`, which are the
same setting; this applies at every level, to names under `model.providers`
and to the keys of maps such as an extension's `env` and a corpus's `headers`
and `headers_env`, so a managed file cannot set both `PATH` and `path` there)
or a number too large to hold, such as `1e400`,
even under a comment key. JSON leaves the meaning of such a file open, and an
organisation's rules must not depend on which reading wins.

When the managed file sets any `permissions` setting but not
`permissions.allow`, the allow rules `~/.abhed/config.json` and a trusted
workspace's `.abhed/config.json` add are left out, and Abhed warns at startup
naming each rule and its file; the built-in allow rules the files' list
still held stay. Put rules the
organisation accepts in the managed `permissions.allow`. When the managed file
sets `permissions.allow`, exactly its list applies. The same holds for
`permissions.git_extensions`, the git extensions that run without the
destructive step's question ([Permissions](04-permissions.md#the-order)).

Two effects to plan for. A call that a dropped rule approved now asks, and in
`-p`, `rpc` and other runs with no one to approve, it is refused. And a
built-in allow rule the files' list left out stays out: a shorter list
removes it under a managed file as it does without one.

What a caller sets over the files, the CLI's flags and the SDK's `Options`,
may tighten what the managed file set and never loosen it:

| Setting | A flag or option may |
|---|---|
| `permissions.mode` | choose `plan` or the managed mode; `bypass` is refused even when the file does not set a mode |
| `tools.syntax_check` | make it stricter only (`off` < `report` < `refuse`) |
| `limits.max_turns` | lower it |
| `permissions.allow` | add nothing when the file sets any `permissions` setting (the `-allow` and `-allowedTools` flags, `/permissions allow`, `Options.Allow`, rpc `start`); the user's and workspace's files' allow rules are dropped with a warning |
| `additional_dirs` | add nothing when the file sets `additional_dirs` |
| `memory.auto` | choose only the managed value, when the file sets it |
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
is an error: Abhed stops rather than run unmanaged. So is a managed folder
(`/etc/abhed`) that is itself a link to nothing; a folder that does not
exist at all means no managed file.

The environment variables in step 5 never change a setting the managed file
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

A managed `model.default` pins the model on every surface: `-model`,
`/model`, a custom command's `model`, the console's picker and the sessions
it starts, an editor over `abhed acp`, subagents, and the SDK's
`SwitchModelNamed`, `SetModel` and `Options.Provider` all refuse another
model. Fallbacks under a managed default are only those the managed
`model.fallback` names. Studio's inline completions are pinned too: under a
managed `model.default`, a `model.completion` (or `model.next_edit`,
`model.commit`) naming another provider is refused unless the managed file
sets that key itself
([Inline completions](34-inline-completions.md#turning-it-on)). Without a managed `model.default` or
`model.allowed`, any provider a file defines may be chosen, and the SDK's
`Provider` names any endpoint. To allow several models rather than pin one,
see [Allowed models and MCP servers](#allowed-models-and-mcp-servers).

### Allowed models and MCP servers

A managed `model.default` pins one model. To allow several, the managed
file lists them in `model.allowed`, and lists the MCP servers that may start
in `mcp.allowed` and `mcp.denied`:

```json
{
  "model": {"allowed": ["anthropic/claude-*", "ollama/qwen3:*", "openai-compatible/corp-*"]},
  "mcp": {
    "allowed": [
      {"name": "github", "digest": "sha256:9f2c…"},
      {"command": "/opt/mcp/bin/*"},
      {"host": "*.mcp.corp.example"}
    ],
    "denied": [{"name": "shell-*"}]
  }
}
```

**Models.** Each `model.allowed` entry is a pattern over a provider's
`type` and the `model` it serves, written `type/model`. `*` stands for any
run of characters, `/` included. The configured provider's own name is not
matched, since any layer can choose a name. A provider is admitted when a
pattern matches it, and every other one is refused on every surface:

- the default model when a session starts, `-model`, `/model` and a custom
  command's `model`
- the console's picker and the sessions it starts or switches
- Studio's model picker and `session/set_config_option` or `session/set_model`
  over `abhed acp`
- the SDK's `SwitchModelNamed`, `SetModel` and `Options.Provider`
- subagents, including a definition's `model`
- `model.completion`, `model.next_edit` and `model.commit`
- `suggest.model`, `verify.critic.model` and a supervisor `monitor.model`
- each `model.fallback` and `-fallback-model` name

The refusal names `model.allowed`, the model as `type/model` and the
patterns. A choice made in a session is recorded as `config.refused` with
the decision `refused`. The console's refusal also goes to the server log
and the admin audit hook. A fallback the list refuses is dropped, and a
`suggest.model` or `verify.critic.model` it refuses is left unset, so the
session's own model does that work. A completion, next-edit or commit model
it refuses is turned away when the editor asks for it. A supervisor judge
model it refuses is never asked, so each call it would judge gets
`monitor.on_unavailable`. Each of these is set aside at load, recorded as
`config.refused` with the decision `set_aside`, and listed by `abhed
doctor`. A default model the list refuses does not stop the configuration
loading. It stops the session from starting with the reason: choose an
admitted model with `-model`, or change `model.default`.

**Endpoints.** A pattern judges the provider's type and model name, not
where the requests go. When the managed file or the org policy sets
`model.allowed`, every input that decides where a model's requests go is
bound as well:

- `base_url`
- watsonx's `iam_url`, where the key is sent for a token
- a vertex type's `region` and `project_id`, which are written into the host
- a custom provider type's own `base_url`
- the supervisor judge's `monitor.endpoint`

Each input is bound when one of these holds:

- It is left unset, so the type's own endpoint is used. A vertex `region`
  and `project_id` must then be plain names of letters, digits and hyphens.
  The vertex adapters now refuse any other value with or without a list.
- It points at this machine (`localhost` or a loopback address).
- The machine's managed file sets it. That means its provider entry under
  `model.providers`, its `custom_providers`, or its `monitor.endpoint`. A
  managed provider entry is one setting, every field included, and
  `ABHED_BASE_URL` cannot change it. An org policy never sets these: they
  name endpoints, and the laptop sets them aside.

Anything else is refused, and the refusal says where the requests would
have gone. That covers:

- a `base_url` from a lower file or from `ABHED_BASE_URL`
- an `iam_url`
- a custom type a lower file defines
- a judge endpoint a lower file sets
- an endpoint an SDK embedder names off this machine

A lower file's explicit `base_url` is refused even when it equals the
type's public endpoint, such as `https://api.anthropic.com`. Leave it unset
instead. A user's or workspace's own list, without one from the
organisation, judges type and model only.

**This machine.** An endpoint on this machine is admitted because Ollama and
the other local servers live there. But a loopback listener can relay
anywhere: a LiteLLM proxy, an `ssh -L` tunnel, or Ollama's own
cloud-offloaded models. A pattern such as `ollama/*` admits those `-cloud`
tags. Abhed does not see where such a relay sends the requests. A managed
`"model": {"allowed_loopback": false}` refuses endpoints on this machine
under the list, including a type whose own endpoint is local (`ollama`,
`vllm`, `llamacpp`, `tgi`, `sglang`) and the built-in `local` provider. A
provider the managed file defines there is still admitted. An org policy may
set `allowed_loopback` to `false`, which only narrows, and never to `true`.

Defining the providers in the managed file does not by itself pin the
endpoint, since a lower file may still add a provider on this machine. Only
two things pin it outright:

- a managed `model.default`, which pins the model and therefore its endpoint
- `allowed_loopback: false` together with managed providers

The egress allowlist does not judge the model's endpoint: that endpoint is
allowed implicitly ([Network policy](21-network-policy.md)).

While a list is set, a model id holding `..` as a path segment, `?`, `#`,
`%` or `\` is refused, since some types put the id in the request's path.
A supervisor judge (`monitor.model`) is matched as
`openai-compatible/<monitor.model>`, because it is reached over that API
whatever serves it. A pattern such as `anthropic/claude-*` does not admit
it. Embedding models (`retrieval.embed_model` at `retrieval.embed_base_url`) are
not covered by `model.allowed`, though they too are sent workspace content.

**MCP servers.** Each `mcp.allowed` or `mcp.denied` entry is an object.
A server matches an entry when every field the entry sets matches it:

| Field | Matches |
|---|---|
| `name` | the configured name; `*` for any run of characters |
| `command` | the absolute path of the program a stdio server's command resolves to on its own `PATH`, as found or with links followed; `*` as above. While any entry matches by `command`, a server is started at exactly the path that was judged, with links followed, not looked up again. That includes a container engine whose image a `digest` pins. Started unconfined, the program is still given the configured name as its `argv[0]`. A confined start passes the resolved path as `argv[0]`, since the sandbox's launcher runs that path, so a multi-call program reached through a link sees its real name there |
| `digest` | the `sha256:` digest the server's configuration pins. Abhed checks the program, or the image the digest names, before it starts the server, so a server whose program does not match never runs |
| `host` | a remote server's URL host; `*.example.com` matches any subdomain. Hosts are compared in the ASCII form a connection dials, as IDNA maps them, so `ｅｖｉｌ.com` is `evil.com`. Under any list, a URL whose host cannot be mapped is refused, and so is a numeric form other than a plain IP (`2130706433`, `0x7f.1`, `127.1`). A redirect to another host is judged as the server reached there |

An entry with no field, a field Abhed does not know, a `command` that is not
an absolute path, a malformed `digest`, or a `host` with a scheme, port or
path stops Abhed at startup. So does an entry that sets `host` together
with `command` or `digest`. A `name` alone matches whatever program a lower
file runs under that name. To bind the program itself, use `command`,
`digest` or `host`. An empty `mcp.allowed` list allows no server.

What `command` and `digest` bind has limits:

- `command` matches the program, never its arguments. An interpreter or
  engine path (`npx`, `uvx`, `python3`, `node`, `docker`) admits whatever
  package, script or image it is told to run.
- For a container engine, `digest` pins only the image. The arguments still
  choose the entrypoint and the mounts (`--entrypoint`, `-v /:/host`).
- On the container tier a `command` entry judges the host's program, and
  the server is started at that path inside the sandbox.
- `digest` binds the file's contents; `command` alone binds only its path.

A server any `mcp.denied` entry matches never starts. When `mcp.allowed` is
set, a server no entry matches never starts either. This covers servers from
every source:

- the user's, a trusted workspace's and the managed `mcp.servers`
- `-mcp-config` and `abhed mcp add`
- one an administrator adds through the console (refused with 403, and
  audited as `mcp.refused`)
- one Studio adds
- a reconnect with `/mcp restart` or Studio's restart

The servers an editor names in ACP `session/new` are never started under any
configuration. The reply's `mcpServersRefusedReason` says so, and each is
recorded.

The lists are applied when the configuration loads. A server they refuse is
taken out and recorded as `config.refused` with the decision `set_aside`, the
reason naming the list. `/mcp`, Studio's MCP panel and `abhed doctor` show it
as refused, with why. The gateway that starts servers checks every server
again just before it starts, so one added later is judged the same way.

**Narrowing.** The lists only narrow. `~/.abhed/config.json`, `-settings`
and a workspace's file may set `model.allowed` and `mcp.allowed`
too, but each list that is set must admit a model or server. A workspace
file's lists apply whether or not it is trusted, since they can only take
away. A malformed list in an untrusted workspace file is ignored with a
warning rather than stopping Abhed. So a lower list
can leave out what the managed list allows, and cannot add to it. A lower
`mcp.denied` adds to the managed one. Without a managed file, a user's own
lists still bind their sessions. Setting `mcp.allowed` or `mcp.denied` in
the managed file does not make its `mcp.servers` the organisation's: the
user's and workspace's servers stay theirs to change, within the lists.

**Fail closed.** A list that cannot be read in the managed file, the user's
file, `-settings` or a trusted workspace's file stops Abhed at startup,
naming the file, rather than leaving the list unenforced. That covers:

- an empty `model.allowed`, which would refuse every model
- a pattern without a `/` or with whitespace
- an MCP entry as described above
- a value of the wrong type

**Where to see them.** `abhed doctor` prints each list with the file that
set it, and every model and server it left out. `/permissions` prints the
same lines. The policy views show them too: `allowLists` in Studio's policy
view (ACP `_abhed/capabilities`), and `allow_lists` in the console's
`/v1/sessions/{id}/permissions`.

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
  list` lists every decision. `abhed trust startup-files` is the same for
  your shell's start-up files (see the permissions guide).
- **`abhed serve`** says on its front page, under the console's examples,
  in the workbench's **Tools** panel, and in `GET /v1/overview` as
  `workspace_config`, whether the file is trusted, why, and the names of the
  settings it set aside (not their values), each with why: a trusted file's
  too, such as a language server it names. To a signed-in person, or to
  anyone where nobody signs in.

The prompt and `abhed trust` list a managed-only setting the file makes, such
as `sandbox.network` or the `egress` rules, apart from the rest, under "Not
applied whether or not you trust it", with the reason: trusting the file does
not apply it.

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
