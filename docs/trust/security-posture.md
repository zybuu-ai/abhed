# Security posture

A summary for a security reviewer. Every claim below names the file that
makes it true — read the file if you need more than the summary.

Abhed is at 1.x, built by a small company. No claim here should be read as a
certification; see "What is NOT in place" at the end.

## Trust boundaries

**Sandbox tiers.** Abhed executes agent-issued commands inside a sandbox
whose strength is explicit and self-reporting (`internal/sandbox/sandbox.go`):

| Tier | Mechanism | Notes |
|---|---|---|
| `none` | Runs directly on the host | Suitable only for a trusted single-user local run |
| `process` | Process-level confinement: macOS `sandbox-exec`; on Linux, bubblewrap with its own PID, IPC, UTS and (with no network) network namespaces and a read-only view of the system. No seccomp filter or Landlock ruleset is applied | The minimum to set on any shared server: `sandbox.min_tier: "process"` |
| `container` | OCI container: namespace isolation, shared kernel | |
| `vm` | gVisor (`runsc`): a user-space kernel that intercepts system calls, run as a container runtime. Not a microVM; the name is kept for compatibility | Strongest tier implemented |
| `fence` | Preview, Linux only, off unless `sandbox.tier: "fence"`: each command confined by Landlock and a seccomp filter, in a cgroup per tool call (`internal/sandbox/fence_linux.go`). Where an ordinary user may make a user namespace (mode `mount_namespace`), each command also gets a mount namespace of its own: git's config and hooks and an editor's protected files bound read-only, the workspace's `.abhed` under an empty tmpfs (`internal/fence/mountns`). Without one (mode `landlock_only`) the command line runs fenced and `abhed serve` and Studio are refused. Not a microVM; shares the host kernel | Counts as `process` for `min_tier`; fails closed, never falls back to another tier, and records its mode in `fence.qualified`. Requirements and limits: [Configuration](../guide/02-configuration.md#the-fence-tier-preview-linux) |

`Select` (`internal/sandbox/sandbox.go`) picks the strongest backend
available that meets the configured `MinTier`, and **refuses to start** if
none qualifies, naming what it tried and how to fix it — it never silently
downgrades (see the comment above `Select` and `README.md`'s "The sandbox
never silently downgrades"). Pin `sandbox.min_tier` explicitly in the config
of any shared deployment so that an empty or `none` value cannot ship
unnoticed; `abhed doctor` reports the tier actually in force.

**The agent does not administer Abhed.** Inside an agent's command, the
subcommands that change Abhed's own state are refused: `record prune`,
`trust grant`, `init`, `user add|passwd|remove|import`, `secret set|rm`,
`mcp add|remove` and `migrate`, as are `-trust-workspace`,
`-dangerously-skip-permissions` and `-mode bypass` on a nested session,
and bypass mode, an allow rule or a git extension that `-settings`, `-agents`,
`-mcp-config` or `-allowedTools` would add to its configuration.
A command counts as the agent's when `ABHED_SANDBOX` is set, or when an
`abhed` process is above it in the process tree, so unsetting the variable
is not enough. A daemon that detaches from the tree escapes the check, so it
is not the boundary: the sandbox's deny on `~/.abhed` and the state paths
is, and with the sandbox off (`none`) nothing is. Reading (`record list`,
`trust show`, `mcp list`) is unchanged.

Note: `docs/architecture/03-security.md` describes a more elaborate tier
design (gVisor by default, a microVM per session) as the target architecture.
That document is explicit that this is **engineering judgment, not verified
practice**, and the tiers actually implemented in
`internal/sandbox/sandbox.go` today are `none` / `process` / `container` /
`vm`, verified by the escape tests in that package, plus the `fence` preview
on Linux, which is off unless chosen, counts as `process` for `min_tier`,
fails closed and is not a microVM. Treat the design doc as direction, and
this document and the code as what runs today.

**Policy engine order.** Every tool call is evaluated in a fixed order,
documented at the top of `internal/policy/policy.go`:

```
Arguments → Hooks → Deny rules → Ask rules → Permission mode → Allow rules → Callback
```

**Deny rules are absolute for tool calls.** A matching deny rule blocks the
call even in `bypass` mode — the most permissive mode Abhed has
(`internal/policy/policy.go`, `ModeBypass` comment: "dangerous; refusable by
org policy") — whether the agent or a person makes it. One scope limit: what a
person runs inside the workbench's interactive shell is bounded by the sandbox,
and there deny rules are a best-effort screen on each line as typed (see "A
person's terminal is sandboxed" below). The workbench Explorer's New folder,
rename and delete are judged as actions of their own (`mkdir`, `rename`,
`delete`), not as command text, so `bash(...)` rules do not apply to them;
`write(...)` rules on their paths and rules naming the action do (a
`rename(...)` rule is matched against both names, and a `delete(...)` rule
against the old one, since a rename removes it). Hooks and extensions see
those action names, not `bash`. Rules are
scoped per-command, not per-tool: allowing `bash(npm test)` never allows
`bash(rm -rf /)`. A deployment's deny list should block reads of SSH keys,
cloud credentials and `.env` files, and no allow rule should pre-approve an interpreter or file-reading command that could be
used to exfiltrate one of those files under the cover of an approved rule.

**The managed configuration binds every entry point.** `/etc/abhed/config.json`
is read last, and `config.Load` records which keys it set
(`Config.ManagedKeys`). The CLI's flags and the SDK's `Options` pass through
one function, `Config.Apply` (`config/managed.go`), which lets them tighten a
managed setting and refuses, with an error, anything that would loosen one:
`bypass` mode, a mode other than `plan` when the file pins one, a weaker
`tools.syntax_check`, a higher `limits.max_turns`, an added allow rule or
directory when the file sets those lists. Deny rules from a caller are only
ever added. The SDK reads the managed file even without a `ConfigDir`, marks
its engine managed as the CLI and server do, applies the configured ask rules,
and wraps bash in the configured sandbox when the file sets a `sandbox` key.
The console lets a client narrow its session to `plan` and nothing else. Not
bound: the `ABHED_*` environment variables, which override the endpoint,
credentials and database over the managed file; the choice of model; and, in
the SDK, the organisation's `/etc/abhed/ABHED.md` (the SDK loads no memory
files, and `Options.SystemPrompt` replaces the prompt), `limits.max_budget_tokens`
and `limits.max_tokens`, which the SDK does not apply. A managed file that
exists but cannot be read, or a link at the managed path to nothing, stops
Abhed rather than being taken as absent. `abhed eval`, which approves every
prompt with nobody to ask, refuses to run under a managed file, and the
interactive `/mode` command is bound as `-mode` is. The binding covers the
shipped entry points and programs that load configuration with `config.Load`;
a `config.Config` built by hand carries no managed keys.

**The agent cannot reach its own configuration.** `.abhed/` in the workspace
and in the home directory holds the policy, the users file, the keys and the
workspace-trust decisions (`~/.abhed/trust.json`). One
check, `tools.StateSet` (`internal/tools/state.go`), is shared by the agent's
file tools, glob, grep, the code index and the server's download, viewer,
search and upload endpoints. It compares path components with `.abhed`
without case, judges both the path as given and the path with every link
followed, and asks the filesystem, by identity (`os.SameFile`), whether the
path or a folder above it is a state directory or a state file; the users,
config and secrets files are always known, and other files and folders there
up to 4,096. Files are opened under the workspace held open as an `os.Root`
(`internal/tools/confined.go`), so a link swapped in after the check leads
neither out of the workspace nor into the state, and what was opened is
judged again. A folder an upload needs is made one step at a time the same
way, so a link into the state is refused before anything is made there. The
process sandbox denies commands reading and writing
`.abhed/` (`internal/sandbox/process.go`), with `~/.abhed/skills` the one
readable part. That denial is by path, not by file: a hard link to a state
file elsewhere in the workspace is a path the sandbox does not guard. The
agent cannot make one on macOS, and one that already exists stops Abhed: every
entry point that builds the sandbox refuses to start, and `abhed doctor`
fails, when a state file (the users, config and secrets files and the other
files the state check knows, in the workspace and the home directory, and the
users, config and secrets files the user could rewrite in every folder above
the workspace, other than a world-writable sticky one) has more than one name, and `config.Load` refuses
a configuration file with more than one name (`internal/nlink`,
`internal/sandboxconfig`). Link counts are not read on Windows.

A users file set with `auth.users_file`, or a secrets file set with
`ABHED_SECRETS_FILE`, is refused by the same check. Abhed refuses to start
when one lies where commands can write (the workspace, an added directory, a
temp folder or a toolchain cache) unless it is under the workspace's or the
home directory's `.abhed/`, since a command there could move a folder above
it (`internal/sandboxconfig`). This is enforced before the rules are
consulted, because a rule that protects the file the rules live in can be
removed by editing that file. `internal/tools/state_test.go`,
`internal/sandbox/state_test.go` and `server/state_paths_test.go` try the
spellings, links, swaps and hardlinks, and fail if any succeeds.

Abhed's own git commands run on the host, outside the sandbox, in a repository
the agent can write: the branch and change count in each prompt, worktrees for
parallel runs, and a resolved issue's commit and push. They switch off, in
git's command-line scope (through `GIT_CONFIG_COUNT`, which names any driver),
the settings known to name a program git would run for these commands:
fsmonitor, hooks, clean and smudge filters, merge drivers, textconv and
external diffs, signing, credential helpers and every transport but https.
They do not enter submodules, refuse to commit a change holding a repository
of its own, and drop git's environment variables (`internal/hostgit`). The git
they run is the one on `PATH`, links followed, and is refused when it lies in
the repository or in a folder sandboxed commands may write (temp folders,
toolchain caches), where the agent could have planted one. This is
a list, so it is best effort: a setting a later git adds is not covered until
it is listed. Running these commands inside the sandbox is the complete
answer, and is tracked as follow-up work. A resolved issue's branch is pushed
to an https address built from the issue, from a temporary repository with no
configuration of its own that borrows the checkout's objects, so the
agent-writable repository configuration cannot choose where the token goes;
`GIT_ALLOW_PROTOCOL=https` holds against any per-protocol setting. The
operator's global configuration still applies, and is trusted only while the
agent cannot write it. Assuming the process sandbox tier, the push is refused
when `~/.gitconfig`, `~/.config/git/config` or `$XDG_CONFIG_HOME/git/config`,
or the folder it would be made in, lies in the run's worktree or a writable
area of the sandbox (temp folders, toolchain caches). The temporary repository
is made in `~/.abhed/push`, a private folder sandboxed commands cannot write;
when that folder is in a writable area, is a file or a link, or is not the
operator's own (not checked on Windows), the push is refused rather than made
elsewhere. Not checked: files the global configuration pulls in with
`include.path` or `includeIf`, and a system gitconfig under a git installed in
a writable folder, which the operator writes; and on the `none` tier, or in a
container that mounts the home directory, the run can write the global
configuration and `~/.abhed/push`, and the push is not refused.

The `container` and `vm` tiers mount an empty, throwaway folder over the
workspace's `.abhed/`, as bubblewrap does, and hide a configured state path
that the workspace or a read-only directory would show: a folder behind an
empty one, a file behind `/dev/null`. Where the workspace's disk ignores
case (a macOS workspace in Docker Desktop or a Podman machine), every case
spelling of `.abhed` is covered, since the engine's own kernel tells them
apart. The empty `.abhed` folder is made in the workspace, as the person,
when it is missing.

On macOS a command can stat the workspace `.abhed` directory and what is in
it, so `ls -R`, pytest's collection and `git add -A` (with a warning that it
cannot open the directory) pass it by; it cannot list it or read, write, link
or clone what it holds (`TestProcessSandboxWalksPastHarnessState`). A walk
that descends into every folder, such as `find .` or `du`, still reports
`.abhed` and exits 1. Nothing is written into the workspace to achieve this.
On Linux bubblewrap mounts an empty directory over it instead.

**A person's terminal is sandboxed; its line checks are best effort.** Each
tab of the `/ide` terminal is, by default, one interactive `bash` started
through the same sandbox backend as the agent's commands (`Shell` in
`internal/sandbox/process.go` and `container.go`), in the workspace root, under
the tier's filesystem, network and harness-state limits
(`TestProcessSandboxShellIsInteractiveAndConfined`). Opening it is the
person's `bash` call: judged by the policy, so plan mode refuses it, and
recorded with `actor: user`, as is the shell's exit and the last 64 KB of its
output. An interactive shell cannot be judged command by command, because the
shell decides what a line means after it is sent: history recall, tab
completion, aliases, functions, scripts and full-screen programs all happen
inside it. So, for the terminal:

- the sandbox is the enforcement boundary;
- `bash` deny rules are a screen: the server rebuilds each line from the keys
  it forwards and refuses a matching line before its Enter reaches the shell
  (`internal/termline`, `shellInput` in `server/pty.go`, and the same in
  `app/acp_shell.go` for Abhed Studio's interactive terminal), recording the
  refusal. It does not see what the shell makes of the line: history recall
  (arrow keys, `!!`, Ctrl-R, Ctrl-O), completion, variables and other
  expansions, a line continued with `\` (`rm -rf \` then `/` passes both
  lines), aliases, functions, scripts, or input to another program, including
  a nested shell;
- each line entered is recorded as `terminal.input`, as typed, marked `edited`
  when keys the server cannot follow were used. When the server cannot confirm
  the terminal showed the line as typed, the line is recorded without its text:
  at a password prompt (on the process and none tiers, read from the
  terminal's own mode), when its echo was not seen before the Enter, and for a
  line under four bytes where the terminal could not be asked. The whole line
  must be seen echoed, so keys a program took without an Enter (`read -s -n`)
  never prefix a recorded line, and an edited line is recorded without its
  text. On the container tier, where the terminal cannot be asked, a password
  that also appears in the prompt printed while it was typed can be recorded.
  Lines typed ahead while a command runs are not screened. While another
  program has the terminal they are not recorded; while the shell itself is
  busy (a builtin, the gap between commands) the terminal is in canonical
  mode, and they are recorded without their text. So is every line after
  `set +o emacs +o vi`, which makes bash read its prompt in canonical mode;
  those lines are still screened. Keys typed ahead
  while a command runs are echoed as they arrive, so they are in the recorded
  output as they were on screen;
- which program has the keys is asked of the terminal on the process and none
  tiers (its foreground process group against the shell's); while it is not
  the shell, keys are neither screened nor recorded. On the container tier
  the engine's CLI holds the terminal, and a switch to the alternate screen is
  the only sign; printing that sequence switches screening and recording off
  there until it is switched back;
- ending a shell (Kill, closing its tab, deleting the session, the idle or
  twelve-hour limit, or `exit`) hangs it up, bash hangs up its background
  jobs, and then every process left in the shell's session is stopped and
  killed, pass after pass until none is new (`Leader` in
  `internal/sandbox/leader.go`). That covers `nohup`, `disown`, `( cmd & )`,
  `trap '' HUP` and a job that keeps forking. The sweep runs only while the
  exited shell is unreaped, so its process id, and with it the session id,
  cannot belong to anything else, and only after the shell's recorded start
  time matches. On Linux each signal goes through a pidfd, so a reused process
  id is never signalled. On macOS a member is checked with `getsid` and then
  signalled by number, both the SIGSTOP and the SIGKILL. If the id were reused
  in between, which needs a pid wrap within microseconds and is not reachable
  in practice, the signal would land on a foreign process: a stop is found on
  the recheck and undone with SIGCONT, a kill is not. A strict fix
  there would need signalling by audit token. When the sweep cannot run (the
  start time unreadable, or no pidfd on a Linux kernel before 5.3) the server
  logs that containment did not run. A process that starts a session of its own
  (`setsid`, a daemon) escapes and runs until it ends, within the sandbox;
  bubblewrap and the container tier end everything regardless. A shell runs
  as the server's user; on the process tier it can start at most
  `sandbox.max_procs` more processes than that user ran when it opened (see
  "Resource limits" below), and on the none tier nothing bounds it. A pids
  cgroup per shell is the planned follow-up.

What the shell can reach is the tier's, as for the agent's commands, but a
person now has it interactively. On the macOS process tier, Seatbelt denies
writes outside the workspace and reads of `.abhed` and of the credential
paths in home listed in `HomeSecrets` (`internal/sandbox/process.go`): keys
and cloud credentials (`~/.ssh`, `~/.aws`, `~/.kube`, `~/.gnupg`,
`~/.config/gcloud`, `~/.azure`, …), git host and registry tokens
(`~/.netrc`, `~/.git-credentials`, `~/.config/gh`, `~/.npmrc`, `~/.pypirc`,
…), database and model tokens, shell history, the keychains and browser
profiles. Each is named both as given and with its links resolved, so a
home reached through a link is covered. The list is a deny list: another
file in home that holds a secret is readable, where Linux's bubblewrap
leaves home out altogether. Signals stay inside the sandbox: the profile
allows a command or shell to signal only processes in its own Seatbelt
sandbox (`(allow signal (target same-sandbox))`), so it cannot stop Abhed
(`kill $PPID`), the server, or another command's processes, as bubblewrap's
PID namespace keeps them apart on Linux (`TestSeatbeltSignalsStayInside`).
One consequence: a command cannot stop a server an earlier command left
running; Abhed's own stop for a background shell, sent from outside, does.
The profile starts from `(allow default)`, and the only Mach lookups it
denies are the network and system configuration services, with the network
off or under the allowlist; other `mach-lookup` services stay reachable: a command can ask the user's per-session services
(the pasteboard, Launch Services, which opens applications and URLs, and
others) to act for it outside the sandbox. Linux has no equivalent. A
deny-by-default profile for Mach services is not done yet. The environment is an allowlist
(`internal/sandbox/process.go`, `env`): no provider keys, no vault secrets, no
`ABHED_` settings. On the `none` tier the shell has the server's environment
without its `ABHED_` settings, which leaves anything else the operator
exported, and nothing contains it.

`sandbox.terminal: "lines"` returns the terminal to one policy-checked `bash`
call per line with no shell state, and a managed policy with deny rules for
`bash` or for every tool (`*`), or with a policy hook, gets that mode
automatically. Even then a rule checks the line, not what
a script the line runs does. On the `none` tier the shell runs on the host, and
the terminal banner and status bar say so.

**Resource limits.** On the process tier each command and each shell starts
with a process limit (`RLIMIT_NPROC`, set by `ulimit -u` before the sandbox
backend starts) of what the server's user runs at that moment plus
`sandbox.max_procs`, 512 by default (`procLimit` in
`internal/sandbox/process.go`, `TestProcessLimitHoldsForTheCommand`). The kernel
counts every process of the user (every thread, on Linux) against it, so a
fork bomb stops after that many more, not after that many of its own, and can
still crowd out the server's own processes until it is stopped; the kernel
does not bound root at all. The headroom is shared: the user's other programs
and concurrent commands use it too, and a long-lived workbench shell keeps the
limit it opened with. The limit never exceeds the one already in force, and
the server itself runs without it. `sandbox.max_memory_mb` is applied only on the
container and vm tiers; the none and process tiers do not bound memory, and
`abhed doctor` warns when it is set for one of them, as it does for
`max_procs` on the none tier or under root. The none tier bounds neither. A command past its
timeout is stopped with everything descended from it, including a child that
left its group with `setsid` while its parent still ran
(`TestBashTimeoutEndsADetachedChild`). A process whose parent had already
exited, such as a daemon that forked twice, is found by a variable each
command's processes inherit (`ABHED_COMMAND_ID`, unguessable, one per
command) and ended too (`TestBashTimeoutEndsADaemon`). That finds it on Linux
and on macOS, with three gaps on the none tier and the macOS process tier: a
process that clears or replaces its environment is not found; on macOS, a
program Apple ships in the system (`/bin/sh`, `/usr/bin/perl`, `/bin/sleep`)
does not show its environment to other processes, so a daemon still running
one of them is not found; and only the user's own processes are looked at.
Bubblewrap ends everything in its namespace.

**Extensions may only veto, never permit.** `internal/extension/extension.go`
states the rule directly: "An extension may VETO, never PERMIT." Hooks run
first in the policy order specifically so they can veto before anything
else executes (`internal/extension/host.go`), and an extension's refusal is
enforced the same way a deny rule is — it can turn an `allow` into a `deny`,
never the reverse. `internal/extension/extension_test.go` asserts this
directly (an extension can force `Decision: Deny`).

**Tool output is tagged untrusted at ingest.** `store/schema.sql`'s
`events` table carries a `trust` column (`CHECK (trust IN ('trusted',
'untrusted'))`) on every event, so provenance travels with the data rather
than being inferred later. File contents, tool output, and MCP responses are
tagged at the point they enter the system (`README.md`: "Everything untrusted
is tagged at ingest"). Policy decisions are made on the *action* requested,
never on the untrusted text that motivated it (`docs/architecture/03-security.md`
§2, "L2").

**A repository's own configuration is untrusted.** A workspace's
`.abhed/config.json` arrives with the repository, so it applies whole only
once the person has trusted its exact contents (path and SHA-256, kept in
`~/.abhed/trust.json`). Until then it can only tighten: add deny and ask rules,
narrow the mode to `plan` or `default`, strengthen the sandbox, turn network
and tools off, and lower limits. It cannot add allow rules, change a model
provider or `base_url`, start extensions or MCP servers, add directories,
widen the sandbox, or set skills, telemetry, auth, storage or server
settings; every ignored setting is named on stderr and in `abhed doctor`.
`serve`, `user` and `migrate` refuse to start when an untrusted file sets
`auth`, `storage` or `server`, because running without them fails open.
Headless runs never prompt; trust comes from a stored grant,
`-trust-workspace` or `ABHED_TRUST_WORKSPACE=1`. What this does not bind:

- A trusted file is trusted whole. Trust is a decision about content the
  person has read, not a sandbox for it.
- The `none` sandbox tier confines nothing, so a command there can write
  `~/.abhed/trust.json` as it can every other file of the user's.
- The default configuration asks before a bash command that mentions
  `ABHED_TRUST_WORKSPACE` or `trust-workspace`, so the agent cannot quietly
  start a nested trusted run. A configuration that sets its own
  `permissions.ask` replaces those rules, and a command that assembles the
  name or runs a script is not caught by them.

Design and the classification of every setting:
`docs/architecture/workspace-trust.md`.

## Data flow — what leaves the deployment

**Nothing, by default.** The shell tool gets no network access unless
`sandbox.allow_network` is set to true or `sandbox.network` to `"allowlist"`
(the default verified by
`TestProcessSandboxBlocksNetworkByDefault` on both the macOS and the Linux
backend — the Linux run needs a privileged CI job, since a hosted runner
cannot unshare a network namespace — per `docs/architecture/03-security.md`
§7), and a containerised server with no
bind mount has no route to the host filesystem.

With the network off, a command cannot see the host's network either. On
Linux the network namespace has only loopback. On the macOS process tier,
Seatbelt also denies the routing sysctls that list interfaces and addresses,
routing sockets, and the system configuration and network services, so
`ifconfig`, `netstat -rn`, `route -n get`, `scutil --nwi` and `ipconfig` fail
instead of showing the LAN address, the gateway or a VPN tunnel (`TestProcessSandboxHidesTheHostsNetwork`). What remains visible
on macOS: the hardware ports and their MAC addresses, which come from the I/O
registry (`networksetup -listallhardwareports`, `ioreg`), and the host name.
Programs that enumerate interfaces get an error rather than a loopback-only
list, as Node's `os.networkInterfaces()` does; Python, git, `go build` and
pytest are unaffected.

**Under `sandbox.network: "allowlist"`**, set in the managed configuration,
each session gets a proxy on loopback and the `egress` rules decide what
leaves, default deny, each decision recorded as an `egress.decision` event
with the call or session it came from, never bodies or credentials. Brokered
this way: `bash` commands (only the process tier accepts the setting; on
Linux a command reaches the proxy only through a relay in its own network
namespace, on macOS Seatbelt allows only the proxy's port), the model
client, `web_fetch`, `web_search` and MCP servers over HTTP (judged in
Abhed's process, request by request), and stdio MCP servers' direct sockets
(confined to a proxy of their own; not started where they cannot be, or as
root on Linux). Not brokered: `ssh`, the `k8s_*` tools, remote RAG, a stdio
server's files (it can plant something that runs later outside any
sandbox), and requests system services make on a command's or server's
behalf, outside the proxy and unrecorded: on macOS `trustd` fetches a
certificate's AIA and OCSP URLs, and on Linux a resolver reached over a
unix socket left visible looks names up. HTTPS is judged by host and port
only; there is no TLS inspection. Details:
`docs/guide/21-network-policy.md`.

**`web_search` and `web_fetch`, when enabled**, are the two narrow,
structured exceptions: a Go tool in the Abhed process makes the request, not
the sandboxed shell. `web_search` sends a query to the configured provider.
`web_fetch` sends only GETs with no body, to the one URL policy judged on a
public host (a redirect is followed only to that same URL or its https
upgrade, at most five times), and nothing else: no other method, no
connection the model holds. A port other than
the scheme's default asks unless an allow rule names it, even for a host on
`web_fetch.allowed_hosts`, except in bypass mode (unless a managed policy
disables it) and `abhed eval`, which approve every ask. Both are off by default (`web_search.enabled`
and `web_fetch.enabled` are false in `config/config.go`'s defaults), each is
enabled on its own, and neither enables shell networking. Both sections are
managed only: only the managed configuration turns them on or names the
provider, endpoint, key or hosts. The user's file, `-settings`, a workspace
trusted or not and the SDK may only turn them off or narrow them (no
`ABHED_*` variable sets either), and each attempt that did not take effect is recorded as a
`config.refused` event naming who made it (`config/websection.go`). `web_fetch`
fetches only the URL policy has judged: it refuses schemes other than
http and https, and any loopback, private, link-local, metadata or reserved
address, checked on the address it connects to, on every redirect hop
(`internal/webfetch/guard.go`). It follows a redirect only to the same URL
or its https upgrade, and hands any other back to the model as a new call.
It ignores proxy settings from the environment, refuses a URL that holds a
stored secret as written, percent-encoded or in another case (not one
encoded otherwise or split across the URL), and
can be held to an operator's host list (`web_fetch.allowed_hosts`). A URL is
a channel out: whatever the model puts in it reaches the site. Without a
host list every call asks, in plan mode too, unless an operator's allow rule
names it (`Engine.AskReadOnly` in `internal/policy/policy.go`); bypass mode
(unless a managed policy disables it) and `abhed eval` do not ask, and a run with no one to ask refuses it.

**The model endpoint the operator configured.** Prompts and context go to
whatever model endpoint is set in `model.providers`. Abhed is model-agnostic
by construction (`docs/vision.md`); a model served on the same machine means
prompts never leave it, and an operator who points Abhed at a third-party API
is sending data to that API. Either is a property of that deployment's
configuration, not a promise Abhed makes about every deployment.

## Storage

**Postgres**, with a schema in `store/schema.sql`. Two properties
the schema comment states directly: events are append-only (no `UPDATE`, no
`DELETE`), and tenant isolation is enforced by row-level security, not only
by query construction.

**Append-only by trigger and by privilege.** `abhed_events_immutable()` raises
on any `UPDATE`, `DELETE` or `TRUNCATE` against `events`. On its own that stops
a bug or a stray statement; it does not stop the role that owns the table,
which may disable a trigger or drop the table outright. So the schema is
applied by an owner role through `abhed migrate`, and the server runs as a
separate role holding `INSERT` and `SELECT` on `events` and owning nothing. The
server **refuses to start** as a role that could alter the record, asking the
database what the connection can do rather than trusting configuration.
`TestRuntimeRoleCannotAlterTheRecord` tries thirteen routes as the runtime role
— rewrite, delete, truncate, disabling or dropping the triggers, replacing the
trigger function, `session_replication_role`, taking ownership, dropping the
table, granting itself the privilege — and CI fails if that test skips.

*This paragraph used to claim the database refused "regardless of how the
application is compromised". That was wrong until 0.3: the application role
owned the tables, so it could truncate them or switch the triggers off. With
`storage.single_role` set it is still the case, by the operator's choice.*

**What is not covered.** The owner role and any database superuser can still
alter the record; that is what owning a database means. Keep those credentials
off the server host. Detecting tampering by them needs a hash chain over the
events with the head stored elsewhere, which is not built.

**`FORCE ROW LEVEL SECURITY`**, not just `ENABLE`. The schema comment explains
why this specific word matters: a table's owner bypasses ordinary RLS, and
the application role almost always owns the tables it created, so `ENABLE`
alone leaves the isolation policy silently inert. This was found by
`TestRowLevelSecurityIsolatesTenants`, which could read another tenant's rows
until `FORCE` was added (`README.md`'s evidence-discipline section tells the
same story). `sessions`, `events` and `checkpoints` all carry `ENABLE` and
`FORCE` together, with a `current_setting('app.tenant_id', true)` policy on
each.

**Two database roles, and the app refuses to run as the wrong one.**
Postgres does not apply row-level security to a superuser or a `BYPASSRLS`
role, not even with `FORCE`, so an Abhed connected as one has every
isolation policy in the schema and none of the isolation. Provision two
roles: a superuser used only to create the database and the application
role, and an application role (`NOSUPERUSER NOBYPASSRLS NOCREATEROLE
NOCREATEDB`) that owns the application's tables and is the only one in the
server's DSN. On connect, Abhed checks `rolsuper OR rolbypassrls` on the role
it connected as and **refuses to start** if that role is privileged
(`store/postgres.go`).

## Authentication

Three modes in this edition (`docs/ops/enabling-auth.md`): `none` (local dev
only), `local` (username/password Abhed holds), and `proxy` (identity from a
trusted reverse proxy's headers). OIDC sign-in against an IdP — full token
verification against the JWKS, PKCE, single-use `state`, browser sessions —
is part of the Enterprise Edition and documented with it.

- **Local accounts use bcrypt** at the library default cost
  (`auth/local.go`, `auth/filestore.go`). A wrong password
  and an unknown username return the same error in the same time — a missing
  user is still run through bcrypt against a dummy hash — specifically to
  prevent timing-based username enumeration, with a test asserting it.
- **No self-registration by default.** `allow_signup` defaults off, so
  accounts are created by an administrator with `abhed user add`. A new
  account lands with no groups — it can use the console and read its own
  sessions, and cannot administer the console or change policy unless an
  administrator puts it in the admin group (`-admin`). Invites, access
  requests and grants are part of the Enterprise Edition.
- **Sign-in is rate-limited** for anonymous callers (`throttle` in
  `server/server.go`).

## Container hardening

When the server runs in a container, run it with these flags. Each closes a
specific threat:

| Control | Flag | Why |
|---|---|---|
| No host filesystem access | no bind mount; workspace is a named volume | No host path is mounted, so there is no host path to escape to |
| Unprivileged user | `--user 10001:10001` | |
| No Linux capabilities | `--cap-drop ALL` | An agent's shell has no business with `CAP_NET_ADMIN` or `CAP_SYS_ADMIN` |
| No privilege escalation | `--security-opt no-new-privileges` | Blocks setuid-binary escalation |
| Immutable image | `--read-only` | A compromise cannot install a backdoor into the binary or persist outside the writable volumes |
| Ephemeral scratch space | `--tmpfs /tmp:rw,noexec,nosuid,size=512m` | Gone on restart, `noexec` so a dropped payload cannot run |
| Resource caps | `--memory 2g --memory-swap 2g --pids-limit 512 --cpus 2` | A runaway or hostile agent should exhaust its own limits, not the host's |
| Loopback-only bind | `--publish 127.0.0.1:8080:8080` | The reverse proxy is the sole route in |
| Config mounted read-only at the managed path | `--volume $CONFIG:/etc/abhed/config.json:ro` | Loading config at the managed path sets `Managed`, refusing `bypass` mode, and binds every key it sets against flags and SDK options from inside the container |
| Skills mounted read-only | `--volume $SKILLS:/workspace/.abhed/skills:ro` | Skills are instructions; the agent must not be able to rewrite its own operating rules |

The transport and browser layers — TLS and HSTS, a Content-Security-Policy,
`frame-ancestors 'none'` — belong to the reverse proxy in front of the
container, which should be the only route to the published port.

## Telemetry

**Off, and not shipped in this edition.** `config/config.go`'s
`TelemetryConfig.Enabled` is a bare `bool` with no default set, so it is
`false` unless explicitly turned on, and the Community Edition contains
nothing that sends telemetry anywhere. The OpenTelemetry exporter is part of
the Enterprise Edition; when enabled there, it ships events over OTLP/HTTP to
whatever collector the operator runs — there is no built-in Zybuu endpoint.
The event log stays the source of truth regardless; telemetry is a tap on
it, so a collector being down cannot slow or fail a session.

## What is NOT in place

Stated plainly rather than buried:

- **No SOC 2, ISO 27001, HIPAA, or FedRAMP certification.** Any mapping of
  Abhed's controls onto a compliance framework is engineering judgment to
  confirm with your compliance function, not an attestation.
- **No third-party penetration test yet.** `README.md`'s evidence-discipline
  section: "A human red-team engagement remains outstanding and is not
  substitutable" for the internal adversarial suite. The suite that exists
  (`internal/redteam`) proves the implemented controls resist the attacks its
  author thought of; it is not independent verification.
  `docs/ops/red-team-scope.md` says what to commission.
- **No automatic failover.** Several servers can share one Postgres
  database, and a finished session can be continued on any of them (see
  `docs/guide/11-sessions.md`). Nothing moves work off a server that stops:
  its running turns end with it and are recorded as interrupted, and the
  session can be continued from there. Its background subagents end with it
  too: the server that next takes the session over (by a message, or its
  sweep once the stopped server's claim is stale) records them as `lost`,
  and their work is not resumed on its own.
- **Agent-command detection can be escaped on the `none` tier.** Whether a
  process is the agent's is decided by `ABHED_SANDBOX` and by an `abhed`
  ancestor in the process tree. A command that unsets the variable and
  double-forks, so its process is reparented to init or launchd, has no
  `abhed` ancestor, and `abhed admin`, `trust grant`, `-settings` and the
  other refusals above no longer recognise it. On the `process`, `container`
  and `vm` tiers the sandbox's deny on `~/.abhed` and the state paths
  still holds; on `none` nothing stands behind the check. Run
  a tier other than `none` wherever the agent must not administer Abhed.
- **A small team.** Zybuu is a small company. There is no security team, no
  on-call rotation, and no bus-factor mitigation beyond what is written down
  in this repository. See `SECURITY.md` for the
  response times that implies.
