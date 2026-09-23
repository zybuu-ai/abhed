# Changelog

All notable changes to Abhed are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- `edit` and `write` parse Go, JSON and Python before writing, where a parser
  is available (see the tools guide for Python's conditions). A change that
  would leave a file that parsed no longer parsing is not applied, and the
  model is told the error and the line; an `edit` whose new text is a pasted
  diff is refused the same way. Workbench saves are warned, never refused.
  HawkEYE reports a refused change as `broken-edit`. `tools.syntax_check`:
  `refuse` (default), `report` or `off`. It comes from sessions in which a
  model wrote diff markers into Python files.
- The `monitor` package and a policy step for it: a judge that reads the
  remit, the agent's reasoning and where a call's arguments came from, and
  may only tighten a decision; unavailable means ask, or deny when headless.
  Verdicts are recorded as `monitor.verdict`. The local-model judge follows.

### Changed

- `SECURITY.md` supports the latest 1.x release; earlier 1.x releases are
  asked to upgrade, and 0.x is no longer supported.
- Sessions from the command line, the server and the Go SDK now refuse an edit
  or write that would break a file's syntax, where they applied it before.
  Set `tools.syntax_check` to `report` or `off` to keep the old behaviour;
  the SDK also takes `Options.SyntaxCheck`.

### Removed

- The benchmark (`bench/`, `docs/benchmarks`) is no longer part of this
  repository. The three multi-harness rig runs published so far are
  withdrawn: the rig did not give every harness the same conditions. The
  earlier single-file comparison of 2026-09-14 is no longer published either.
  Both remain in this repository's history. A result published later will
  come with its method and raw records, whatever it shows.

### Fixed

- HawkEYE's `cold-cache` finding no longer fires when the provider reports no
  cached-token figure at all, as some OpenAI-compatible endpoints do; absence
  had been read as zero. `model.call` events record `cache_reported`; a
  record written before this has none, so `cold-cache` is not raised on it.
  (#74)
- A turn that spends its whole output budget reasoning without a tool call
  is a stall, not an answer: the model is told its reply was cut off, the
  next call asks for low reasoning effort, the turn is marked `cut_off` in
  the record and HawkEYE reports `output-cap`. Sessions ended this way; some
  read as completed because a sentence had come out before the cut.

## [1.0.1] - 2026-09-22

### Added

- `auth.users_file` (`ABHED_USERS_FILE`): where local accounts are kept when
  there is no database. A deployment points it outside every workspace.
- `app.WithMigrateExtension`: an edition with tables of its own registers
  their schema and the runtime role's privileges, so one `abhed migrate`
  provisions the whole record under the owner and runtime roles.

## [1.0.0] - 2026-09-22

The first major release. From here the command line, the configuration
schema, the event record and the Go SDK (`sdk`) follow semantic versioning:
a change that would break any of them is a 2.0. The other public Go packages
(`server`, `auth`, `store`, `config`, `app`) are supported extension points
whose shape may still move in a minor release, and say so when it does.

### Added

- **The workbench at `/ide`.** An editor with syntax highlighting beside the
  agent, a file tree, a changes view that opens each file for review with
  accept and reject chunk by chunk, a terminal, and HawkEYE live. Edits and
  terminal commands made by the person go through the same policy, sandbox
  and record as the agent's, marked as theirs. `@path` attaches a file to a
  message; select code and press the focus-agent key to ask about it. The
  editor and terminal components are built into the binary, so nothing loads
  from the network.
- **HawkEYE.** `abhed hawkeye`, `/hawkeye` and `GET /v1/sessions/{id}/hawkeye`
  report what a session did from its record alone: per-turn tokens and
  context, the policy step behind every call, files touched, subagents,
  compactions, and deterministic findings — a call to a host that only tool
  output supplied, a credential path, a gap in the record, a redacted secret.
- **Context that keeps its record.** Old and large tool results leave the
  window for a one-line pointer once it passes `context.offload_at`; the full
  text stays in the record and the new `recall` tool reads it back by id,
  text or offset. Nothing is lost from the record; anything dropped from the
  window can be retrieved.
- **Secrets by name.** `abhed secret set NAME` stores a credential outside
  every workspace. The model sees the name, asks for it on one command, and
  gets it only under a `secret(NAME)` allow rule, in every mode. Every event
  is redacted before it is written and every tool result before the model
  reads it; HawkEYE reports a redaction.
- **`abhed resolve <issue-url>`** turns an issue on GitHub, GitLab or
  Gitea/Forgejo — self-hosted instances and their certificate authorities
  included — into a branch in its own worktree, a commit, a push and a pull
  request. Opening the request is its own policy-judged action, `forge_pr`.
- **`abhed acp`** speaks the Agent Client Protocol over stdio, so Zed,
  JetBrains and other editors run Abhed as their agent; their approval dialog
  answers asks and cannot lift a deny.
- **The benchmark rig** (`bench/rig`): several harnesses on one local model
  over multi-file tasks with a validity gate, self-tests, two context
  conditions and paired bootstrap intervals. Results are published whatever
  they say; the first published run is still owed.
- **Scale.** A session's node is recorded and requests routed to it,
  approvals are answered on any node, a node's claim is refreshed while its
  turn runs, and shutdown drains instead of ending turns in flight.
- **`abhed migrate`** and two database roles: an owner that migrates and a
  runtime role that can only append.

### Changed

- The permission mode no longer decides secrets, and a write or edit that
  could not succeed is refused before anyone is asked to approve it.
- A finished terminal command stays readable for a minute, so a short one is
  not lost to a reader that connects late.
- The gosec baseline update refuses a change that would silently drop triaged
  findings.

### Fixed

- Long files in Hindi, Japanese, Russian and other non-Latin scripts were
  refused as binary two times in three.
- A prompt label cut mid-character produced invalid UTF-8 that the database
  refused. Contributed in #38.
- bubblewrap hid a workspace under `/tmp` behind its own tmpfs, so every
  command there failed to start.
- The docs site fails the build when two pages claim one URL instead of
  writing one over the other.

### Security

- **The agent cannot reach its own configuration in any mode.** The file
  tools refuse any path with a `.abhed` component, and the process sandbox
  denies commands reading or writing it, in the workspace and in the home
  directory; `~/.abhed/skills` stays readable. Before this, `auto` and
  `bypass` mode let the write tool rewrite `config.json` or plant
  `users.json`.
- **`/download` withheld nothing.** Any signed-in user could download the
  server's own state (`users.json`, `config.json`) and files a deny rule
  covers. Both are refused now, as the viewer and the agent refuse them.
  Deployments on 0.2.0 should upgrade and treat local account passwords as
  exposed.
- **The audit record is protected from the server's own credentials.** The
  application role owned the tables, so it could truncate them or disable
  the append-only triggers. The runtime role now holds insert and select
  only, a `BEFORE TRUNCATE` trigger stands as well, and the server refuses to
  start as a role that could alter the record. Configure `storage.migrate_dsn`
  for the owner, or `storage.single_role` to keep one role knowingly.
- The egress, credential and configuration boundaries are verified on Linux
  under bubblewrap in a privileged CI job, not only on macOS.

## [0.2.0] - 2026-09-19

### Security

- Uploads no longer land in `.abhed/`, the directory the agent is denied. A
  deployment carrying the ordinary `read(**/.abhed/**)` rule refused every file
  a user attached, and the model reported it as a policy denial for a document
  it had just been handed. Attachments now go to `<workspace>/uploads/`.
- The upload endpoint requires session ownership. Any authenticated caller
  could previously write a file into another user's session directory, which
  the victim's agent then read as untrusted input.
- Policy rules can scope tools whose argument is not `command` or `path`.
  `deny k8s_get(secrets*)` compiled and never fired, and because `k8s_get` is
  read-only the read was auto-approved in auto mode.
- Pipeline tool arguments escape substitutions, so untrusted prior-step or RAG
  output cannot close a JSON string and change the arguments a tool receives.
- The legacy MCP SSE transport pins its POST endpoint to the configured
  origin, so a hostile server cannot redirect client posts — and the auth
  headers they carry — to an arbitrary URL.
- Extension `environ` layers on the inherited environment instead of replacing
  it, so setting one variable no longer drops `HOME`, `PATH` and the locale.

### Added

- `max_budget_tokens` is enforced on the primary agent, not only at
  subagent-spawn time. A session that exhausts its allowance ends with the
  terminal reason `max_budget`.
- Sessions record how full the context was, not only what they cost.
  `context_tokens` and `context_window` sit alongside the cumulative token
  totals in the session record and the `session.ended` event.
- The CLI shows a thinking indicator while the model works, completes slash
  commands as they are typed, and displays the model's reasoning, collapsed by
  default and expanded with `/think`.

### Fixed

- A session ended by server shutdown records the terminal reason `shutdown`
  rather than `user_interrupt`, so the audit log can tell a user stopping a run
  from the process going away.
- Every terminal reason is now either emitted in live code or marked reserved.
  Nine were defined, six reachable, and the README claimed eight.
- `exit` and `quit` end an interactive CLI session. Without a leading slash
  they were sent to the model as a prompt.

### Changed

- CI gates on CVEs, coverage and the adversarial suite. Scans cover the built
  binary and the container image, not only the source, and a scan that does
  not run fails the build rather than reading as clean. Coverage may not drop
  below the recorded floor.

### Documentation

- The `vm` sandbox tier is documented as gVisor, which is what ships, rather
  than a Firecracker or Kata microVM, which does not.
- The security document states that the opt-in network tools execute outside
  the sandbox boundary, and which of them auto mode approves without a prompt.
- Benchmark results separate token count from cost, with the input and output
  split, because tokens are only cost on a hosted per-token API.

## [0.1.0] - 2026-09-15

The first public release of the Community Edition.

### Added
- The agent loop with tool contracts, a tiered execution sandbox, and a
  permission engine whose deny rules hold in every mode.
- Twenty model providers over three wire formats, chosen per session.
- Five ways to run it: interactive, headless (`-p`), JSON events per line,
  JSONL RPC over stdio, and a single-tenant server with a web console.
- A Go SDK that gives your own program the same loop with the same policy
  and audit guarantees, including validated structured output.
- An append-only audit record with replay, fork from any step, and export,
  in memory or in Postgres with row-level security.
- Skills, extensions (JSONL over stdio, veto-only), MCP servers, and
  parallel subagents in git worktrees.
- `abhed doctor`, which proves the endpoint answers, tool calling works,
  and a command runs under the sandbox before you trust a run.
