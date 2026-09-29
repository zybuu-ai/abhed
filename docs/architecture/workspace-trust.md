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
warning says why.

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
| `sandbox.max_memory_mb`, `max_procs`, `terminal_idle_minutes` | **applied** only when lower |
| `sandbox.terminal` | **applied** only for `lines` |
| `sandbox.read_only_paths` | ignored: it mounts more of the host into the sandbox |
| `limits.max_turns`, `max_tokens`, `max_budget_tokens`, `max_subagents`, `max_parallel_subagents` | **applied** only when lower; zero means unlimited, so any positive value is lower than zero |
| `limits.nested_subagents` | **applied** only when false |
| `tools.syntax_check` | **applied** only when stricter (off < report < refuse) |
| `web_search.enabled`, `k8s.enabled`, `k8s.allow_writes`, `ssh.enabled`, `telemetry.enabled` | **applied** only when false |
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
| `web_search` (other keys), `k8s` (other keys), `ssh.hosts`, `telemetry` (other keys) | ignored: each names an endpoint, credentials or machines |
| `storage` | ignored: it decides where the record goes, and with which credentials |
| `auth` | ignored: it decides who may sign in |
| `server` | ignored: deployment settings for `serve` |
| `schedules` | ignored: prompts that the server runs on its own |
| an unknown key | ignored, and reported as before |

## Asking

**Interactive CLI.** The prompt appears when stdin and stderr are both a
terminal, the file would change something, and there is no answer yet for this
content. The prompt:

- lists each setting the file would change, with its value
- lists the tightening settings that already apply
- offers **t**rust, **d**on't trust and **v**iew the file

Only an explicit `t` trusts the file. An empty line asks again, and the end of
input counts as no answer. Text from the file is shown with control characters
escaped, so a file cannot drive the terminal it is displayed on.

**Headless** (`-p`, `rpc`, `acp`, `resolve`, `serve`, and every other
subcommand). These never prompt. Trust comes from one of:

- a stored decision
- `-trust-workspace`, for this run only
- `ABHED_TRUST_WORKSPACE=1`, for this run only

Neither the flag nor the variable records anything. When a file is untrusted,
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
| `abhed trust grant [dir]` | trusts the current content |
| `abhed trust revoke [dir]` | forgets the decision |
| `abhed trust list` | lists every stored decision |

## What this does not cover

- **Other files in the repository** are not configuration. `ABHED.md` memory
  files are still read into the prompt as untrusted text (docs 03 §2, L1).
- **A trusted file is trusted whole.** Trust is a decision about content the
  person has read. It is not a sandbox for that content.
- **The variable is for one run.** Commands the agent runs do not inherit
  `ABHED_TRUST_WORKSPACE`: the process tier passes only an allowlist, and a
  command on the host has it removed. An `abhed` those commands start
  therefore trusts only what the person granted. Set the variable for a single CI step, not in a
  shell profile.
