# Workspace trust

Status: 2026-09-29

A repository can ship its own `.abhed/config.json`, and earlier releases
applied it whole. Cloning a repository and running `abhed` in it could turn on
bypass mode, add allow rules, point the model at another server so the code
went there, start extension processes and MCP servers, and weaken the sandbox.
The same applied to `abhed -p`, `acp`, `rpc`, `serve` and `resolve`.

This page describes how Abhed now treats that file. Editors call the same idea
Workspace Trust.

## The rule

**A workspace's configuration file is untrusted until the person trusts its
exact contents.** Until then, Abhed applies only the settings that make it
stricter. It ignores every other setting and names each one it ignored.

Four sources of configuration are unaffected:

- the built-in defaults
- the user's own `~/.abhed/config.json`
- the environment
- the managed configuration in `/etc/abhed`, which still wins over everything

The order is unchanged: defaults, then the user file, then the workspace file,
then the managed file, then the environment.

## Trust is keyed by path and content

A decision is stored in `~/.abhed/trust.json` under two keys: the workspace's
canonical path, with links resolved, and the SHA-256 of the file's bytes.

- **A grant covers the content that was reviewed.** An edit makes the file
  untrusted again, and it is asked about again.
- **A "don't trust" answer is also stored.** It holds for that content only, so
  the person is not asked on every run. The warning still prints each time.
- **The hash is of what was shown.** A grant records the hash of the bytes
  that were classified and displayed, not of a second read. A file swapped in
  the meantime therefore stays untrusted.

The store sits in the user's home state directory. The file tools refuse that
directory, and every sandbox tier keeps commands out of it. The agent cannot
grant itself trust. Tests hold this for both paths: the file tools, and the
process sandbox. On the `none` tier nothing confines a command, which is
already true of every other file in that directory.

A store with a second hard link is refused, as the configuration is. So is a
store that cannot be read. In either case no workspace is trusted and a
warning says why. Writes to the store hold a lock file beside it, so two
decisions made at once both land.

The path is compared as spelled after links are resolved. On a volume that
ignores case, `/Users/x/Proj` and `/users/x/proj` are two keys. That fails
closed: the second spelling is asked about again.

`abhed init` writes the starter file and trusts it, since the person asked for
exactly that content. Abhed's own settings page never writes this file, so no
other file is trusted automatically. When the workspace is the home directory,
the file is the user's own and is trusted as before.

## Untrusted: what applies

Each setting in the file resolves to a rule, found by its dotted path or the
nearest section above it. A setting with no rule is ignored, and a test fails
for any field of `Config` that has none. An applied setting counts as set for
`Sets`; an ignored one does not.

| Setting | Untrusted |
|---|---|
| `permissions.deny`, `permissions.ask` | **applied**, added to the rules already there, so the defaults stay |
| `permissions.mode` | **applied** only for `plan` or `default`, and only when that is narrower than the current mode |
| `permissions.allow` | ignored: an allow rule widens what runs without asking |
| `sandbox.min_tier` | **applied** only for a stronger tier (none < process < container < vm) |
| `sandbox.allow_network` | **applied** only when false |
| `sandbox.max_memory_mb`, `max_procs`, `terminal_idle_minutes` | **applied** only when lower than the value in effect; zero means the default (4096, 512 and 30) |
| `sandbox.terminal` | **applied** only for `lines` |
| `sandbox.read_only_paths` | ignored: it mounts more of the host into the sandbox |
| `limits.max_tokens`, `max_budget_tokens`, `max_subagents` | **applied** only when lower; zero means unlimited, so any positive value is lower |
| `limits.max_turns` | **applied** only when lower; zero means no turns, so nothing is lower |
| `limits.max_parallel_subagents` | **applied** only when lower; zero means the tool's cap of 8 |
| `limits.nested_subagents` | **applied** only when false |
| `tools.syntax_check` | **applied** only when stricter (off < report < refuse) |
| `web_search.enabled`, `k8s.enabled`, `k8s.allow_writes`, `ssh.enabled` | **applied** only when false |
| `telemetry` (all of it, `enabled` too) | ignored: turning the user's export off removes an audit feed |
| `skills.disabled` | **applied** only when true |
| `model` (`default`, `providers`, any `base_url`) | ignored: the provider receives the code |
| `custom_providers` | ignored: the same |
| `extensions` | ignored: each one is a process |
| `mcp` | ignored: a server is a process or an endpoint |
| `skills.dirs` | ignored: a skill is instructions to the agent |
| `additional_dirs` | ignored: it widens the directories the agent may reach |
| `context` | ignored: `memory_files` are read into the prompt. The thresholds wait for trust with the rest |
| `retrieval` | ignored: `embed_base_url` receives the code |
| `rag` | ignored: a corpus URL and its headers are egress |
| `web_search` (other keys), `k8s` (other keys), `ssh.hosts` | ignored: each names an endpoint, credentials or machines |
| `storage` | ignored, and `serve`, `user` and `migrate` refuse to run (below) |
| `auth` | ignored, and `serve`, `user` and `migrate` refuse to run (below) |
| `server` | ignored, and `serve`, `user` and `migrate` refuse to run (below) |
| `schedules` | ignored: prompts that the server runs on its own |
| an unknown key | ignored, and reported as before |

A deny or ask rule that does not parse is set aside and named with the parse
error; the file's other rules still apply. A bad rule in a trusted file, or in
any other file, stops every command from loading the configuration. Before,
`serve` and `resolve` dropped it and every rule after it without saying so.

### Deployment settings fail closed

For most settings, leaving the file's value out keeps the safer default. For
`auth`, `storage` and `server` the default is the loosest value: no sign-in,
an in-memory record, no group restriction. Ignoring an untrusted `auth`
section would therefore start `serve` as an open console. So when an
untrusted file sets anything under these three sections, `serve`, `user` and
`migrate` refuse to start. The error names the settings and says how to go
on: `abhed trust grant`, `abhed -trust-workspace serve`, or moving the
settings to `~/.abhed/config.json` or the managed file. Commands that do not
serve anyone, such as the CLI and `-p`, run with the settings ignored and warn.

## Asking

**Interactive CLI.** The prompt appears when stdin and stderr are both a
terminal, the file would change something, and there is no answer yet for this
content. The prompt:

- lists each setting the file would change, with its value
- lists the tightening settings that already apply
- offers **t**rust, **d**on't trust and **v**iew the file

Only an explicit `t` trusts the file. An empty line asks again, and the end of
input counts as no answer. Text from the file is shown with control characters
escaped, newlines and tabs included, so a file can neither drive the terminal
nor draw lines of its own in the prompt. Only the body of **v**iew keeps its
line breaks, between marker lines.

Values that carry credentials are redacted wherever ignored settings are
shown: the prompt, the warning, `abhed doctor`, `abhed trust`, ACP and rpc.
That covers fields named like a key, secret, password, token, DSN, header or
environment, and any password in a URL. A field naming an environment
variable (`*_env`) is shown.

**Headless** (`-p`, `rpc`, `acp`, `resolve`, `serve`, and every other
subcommand). These never prompt. Trust comes from one of:

- a stored decision
- `-trust-workspace`, for this run only
- `ABHED_TRUST_WORKSPACE=1`, for this run only

The flag goes before or after the subcommand: `abhed -trust-workspace serve`
and `abhed serve -trust-workspace` are the same. Neither the flag nor the
variable records anything. On `acp` and `rpc` they trust the file of every
workspace the client opens, not only the one named on the command line. When a file is untrusted,
a warning on stderr names every ignored setting, and `abhed doctor` prints one
line per ignored setting.

**Reporting.**

- ACP: `session/new` answers with `_meta.abhed.workspaceTrust`. It holds the
  file, its hash, `trusted`, a `reason`, and the `applied` and `ignored`
  settings, so an editor can ask the person and then run `abhed trust grant`.
  An editor may send `_meta.abhed.trust: "untrusted"` to take only what
  tightens. The wire cannot grant trust, and any other value is an error.
- `rpc`: the `ready` event carries `workspace_trust`.
- SDK: `Options.WorkspaceTrust` sets the choice, and `Agent.WorkspaceTrust()`
  reports it.

`reason` takes one of these values:

| Reason | Meaning |
|---|---|
| `none` | there is no file |
| `home` | the file is the user's own |
| `stored` | a stored grant for this content |
| `flag` | trusted by `-trust-workspace` |
| `env` | trusted by `ABHED_TRUST_WORKSPACE` |
| `new` | no decision yet |
| `changed` | the file changed since the last decision |
| `declined` | the person chose not to trust this content |
| `refused` | the caller chose to take only what tightens |

## `abhed trust`

| Command | What it does |
|---|---|
| `abhed trust [show] [dir]` | shows the file, its hash, the decision, and what it sets beyond tightening |
| `abhed trust grant [-sha256 H] [dir]` | trusts the current content; with `-sha256`, only if it is still the content with that hash, as `show` or ACP reported it |
| `abhed trust revoke [dir]` | forgets the decision |
| `abhed trust list` | lists every stored decision |

## What this does not cover

- **Other files in the repository** are not configuration. `ABHED.md` memory
  files are still read into the prompt as untrusted text (docs 03 §2, L1).
- **A trusted file is trusted whole.** Trust is a decision about content the
  person has read. It is not a sandbox for that content.
- **The variable and the flag are not inherited.** Commands the agent runs do
  not inherit `ABHED_TRUST_WORKSPACE`: the process tier passes only an
  allowlist, and a command on the host has it removed. A command can still set
  the variable or pass `-trust-workspace` itself, so the default configuration
  asks before any bash command that mentions either
  (`bash(*ABHED_TRUST_WORKSPACE*)`, `bash(*trust-workspace*)`). A
  configuration that replaces `permissions.ask` drops these defaults. Set the
  variable for a single CI step, not in a shell profile.
