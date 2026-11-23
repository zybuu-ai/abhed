# Tools

## Built in

| Tool | |
|---|---|
| `read`, `write`, `edit` | files, scoped to the workspace |
| `glob`, `grep` | find files and search contents |
| `bash` | shell, sandboxed, destructive commands always confirm; `run_in_background` starts a command and returns at once |
| `shell_output`, `shell_kill` | read a background command's new output, or stop it |
| `todo` | the agent's task list for multi-step work, recorded as `todo.updated` |
| `task`, `tasks` | run one subagent, or several at once, in the foreground or the background; `task` can resume a finished one; see [Parallel subagents](14-parallel-subagents.md) |
| `task_status`, `task_cancel` | report or cancel this session's background tasks, where the surface runs them |
| `skill` | load a procedure on demand |
| `web_search` | five providers: duckduckgo, brave, tavily, serper, searxng; off by default, and only the managed configuration turns it on ([Web search](02-configuration.md#web-search)) |
| `web_fetch` | read one web page as text, through Abhed rather than the shell; off by default |
| `ssh`, `ssh_connect` | remote execution, off by default except for your own `~/.ssh/config` hosts ([below](#your-own-clusters-and-machines)) |
| `ssh_put`, `ssh_get` | file transfer over SFTP, off by default (`ssh.transfers`) |
| `k8s_get`, `k8s_apply`, `k8s_login` | Kubernetes: your own kubeconfig contexts on your machine, each first use asked once; otherwise off by default and read-only |

A path given to `read`, `write`, `edit`, `glob` or `grep` may be absolute or
relative to the workspace root (never to where a `cd` left `bash`). Either
way it must stay in the workspace or an added directory, and the rules match
it in both spellings.

`edit` matches `old_string` exactly. When it matches nowhere exactly but
matches one place line for line once leading and trailing whitespace is set
aside, such as two tabs sent where the file has one, it is applied there:
`new_string` is re-indented to the file's indentation, and the result says it
was matched loosely and at which line. Its indentation must map onto the
file's one for one and in the same order, so no line moves into or out of a
block; a place nested differently is refused. Two such places are refused,
as an exact double match is, and any difference other than indentation is
not matched.

When a person approved an `edit` or `write` that then failed without
changing anything, a retry of the very same change in the same message
takes that approval instead of asking again, once: the same file, the same
`old_string` (whitespace at the end of a line before a newline aside), the
same `replace_all` and exactly the same new text, or for `write` the same
content. Anything else is asked about again,
and a carried approval never carries on. It is recorded with `by: carried`
and `carried_from` naming the call the person approved. A rule's, the
supervisor's or a preview's ask is asked again.

`write` creates a new file's missing folders, inside the workspace and never
in Abhed's state, so a new file in a new folder needs no shell approval in
`accept-edits` or `auto` mode. A write that fails takes back the folders it made,
and `/undo` of the new file removes them while they are empty.

`grep` also takes a glob in `path`, such as `docs/**/*.md`: the folder before
the first glob segment is searched, held to the workspace as any path, with
the rest as its `glob`.

`grep` also passes over the files a `.gitignore` excludes, the workspace's
own and those below it, as git reads them (`!` takes a file back), unless
`path` names the file or a folder inside an ignored one. It reads the files
left in parallel. When ripgrep (`rg`) is on `PATH` as Abhed starts, one
`rg` run beside the walk narrows which files are read: it is given only a
literal that every match must contain, as a fixed string, and no
configuration or environment of its own. An `rg` inside the workspace or an
added directory is never run, nor one any account but root could replace:
the program and every folder above it must be owned by root and writable by
neither their group nor anyone else, so an `rg` from Homebrew (whose folders
are yours) is not used. Install one from the system's packages for `grep`
to use it; there is no setting to take another. Either way, what reaches the model is
Abhed's own read and match: each file is read inside the workspace, never
through a link or into Abhed's state, a file a read deny rule covers is
left out with its matches (`deny: ["read(secrets/**)"]` keeps `secrets/`
out of `grep` too), and secret values are redacted. A file an ask rule
covers, such as the built-in ones for `.env` and keys, is left out too: no
one can be asked through a search. Each file is judged by its path before
it is read, so a withheld file is never opened or matched; the answer says
how many files were not searched for that reason, a count that is the same
whatever the pattern, and a `path` that names a withheld file is answered
`withheld by a read rule`. `search` leaves such files out before ranking,
as if they were not indexed, and says nothing of them. A pattern with no such literal, or one matched
without case beyond ASCII, is searched without ripgrep. The two searches
read `.gitignore` files with separate parsers, so on an unusual pattern they
can differ in which ignored files they skip; the read rules hold the same
on both.

`glob` and `grep` pass over vendored and build folders (`node_modules`,
`vendor`, `dist`, `.git` and others) unless `path` points into one. A glob that
names such a folder anywhere in its pattern, such as
`services/x/node_modules/**`, says so in its no-match and gives the `path` to
pass. On a disk that ignores case, such as the default macOS one, a glob
matches without case, as the disk opens names: `core/Vault/**` finds
`core/vault`.

`bash` takes an optional `secrets` list: names from the secrets store (on a
server people sign in to, the account's own),
handed to that one command as environment variables when a
`secret(NAME)` rule allows it. The model never sees a value; see
[Secrets](04-permissions.md#secrets).

A command run in the foreground ends with everything it started: a job
left behind with `&` or `nohup`, a daemon, a double fork. Before its result
is given, Abhed ends the processes in the session the command leads and
every descendant it followed while the command ran; one that only carries
its `ABHED_COMMAND_ID` is named in the result, not ended (on macOS, which has no process namespace for it, the residual
case is in [Sandbox](02-configuration.md#sandbox)); under bubblewrap on
Linux the command's process namespace ends them all. The result names
what was ended. To keep a server running, start it with
`run_in_background`; to keep an app open (a new editor window started by
`code .`, say), open it with `open -a`, which hands it to the system
rather than to the command.

### Background commands

`bash` with `run_in_background: true` starts the command and returns at once
with a shell id (`sh_…`), so the agent can start a server, a watcher or a long
build and keep working. The call goes through exactly the same steps as a
foreground one: deny, ask and allow rules, the destructive-command check,
extension screening, approval, the sandbox and `secrets`. Plan mode refuses
it. `timeout_ms`, when given, bounds the command's life; otherwise
`limits.background_max_minutes` does. Its approval is the same one too, so
**Always allow** is offered for the same commands as in the foreground (none
for a chain such as `sleep 40; echo done`); in `/ide` the card also says the
command keeps running after the turn. In the terminal, Ctrl-B moves a
foreground command that is running to the background the same way: its
output so far is the call's result, and `shell.started` says
`from_foreground`. A subagent `task` started in the foreground moves the
same way, recorded as `subagent.backgrounded`.

A timeout ends the command with everything it started. Each command's
processes inherit an unguessable `ABHED_COMMAND_ID`, and a timeout ends every
process of yours that carries it, so a daemon that forked twice and left its
session is ended too. On the none tier and the macOS process tier, a process
that clears or replaces its environment is not found, nor on macOS one still
running a program Apple ships in the system (`/bin/sh`, `/bin/sleep`), which
hides its environment; bubblewrap ends everything in its namespace. See
[Security posture](../trust/security-posture.md).

- `shell_output` returns what the command wrote since the last read, with its
  state (`running`, or `exited` / `killed` and the exit code). One read returns
  at most the last 30,000 bytes of what is new; `wait_ms` waits up to that
  long (at most ten minutes) for the command to end first. While secrets are
  stored and the command is still running, a read holds back the last few
  hundred bytes, a fixed length whatever they say, in case a secret goes on
  in what comes next; they are shown once the command has written nothing
  for a second, or when it ends. So a secret a program prints in two writes
  more than a second apart can show its first part to a read between them;
  a value printed whole is redacted as before. After a gap (output dropped,
  or more than one read returns), the same length is skipped, and the read
  says how many bytes were skipped after a gap; reads show nothing new until
  twice that has arrived, or the command goes quiet or ends, so a secret
  across the skip is seen whole.
- `shell_kill` stops it and every process it started (its process group), and
  returns its last output.
- A shell keeps the last 1 MiB of its output; a read that fell behind says how
  many bytes were dropped.
- When a shell ends, the agent is told the way it is told of a background
  task's result: a `task_status` result with the exit code and the last line.
  A shell the agent stopped itself with `shell_kill` leaves no such notice.
- `limits.background_shells` (default 4) bounds the shells running at once in
  a session; zero allows none. A workspace file may only lower it.
- Shells are listed with the background tasks (`/tasks`, `task_status`, the
  server's tasks endpoint, `_abhed/tasks/list` over ACP, the SDK's
  `Background()`), with `kind: "shell"`, the command, exit code, output size
  and last output line, and stopped the same way. In the CLI, server and SDK
  listings, a running shell's last line is redacted but has no hold, so it
  can show the first part of a secret the command has only partly written.
  They end, process group and all, when the session closes, on a stop of all
  background work, and when Abhed exits. Output the agent reads is recorded
  as an `observation`, redacted like any other; `shell.started` and
  `shell.ended` record the rest.
- Once the shell's own command has ended, what it started and left running
  (`server &`, then the shell exits) is still ended by `shell_kill`, the
  session closing, a stop or Abhed exiting: by the process group the shell
  led, which outlives it while a member is left, and by `ABHED_COMMAND_ID`
  for one that left the group. On the none tier and the macOS process tier,
  one that both left the group (`setsid`, a daemon) and cleared its
  environment, or on macOS runs a program Apple ships, is not found and runs
  until it ends, within the sandbox. If Abhed itself is killed, its shells
  are left running. Bubblewrap on Linux ends everything with the shell: its
  process namespace ends with the shell, and the shell with Abhed.
- A subagent cannot start one, and neither can a surface that runs no
  background work (`abhed eval`).

`bash` says in its description whether commands can reach the network. When
the sandbox has none and a command fails because of it, the result ends with
a note that says so, so the model reports the reason or uses a web tool
instead of retrying.

### git remotes and your credentials

The sandbox hides your SSH keys, SSH agent and credential helpers from
commands, yet `git fetch`, `git pull`, `git push`, `git clone` and `git
ls-remote` work on the process tier and the fence with nothing to configure,
where the sandbox holds git's configuration (`sandbox.protect_git`, the
default): run alone, in plain words, each has its network part go through
git on the host, as you, with your own SSH agent, keychain, credential
helpers and `~/.gitconfig`, to the repository's configured remotes or a
clone URL you approve; a pull's merge and a clone's checkout run in the
sandbox. The agent sees git's output, with any token in a URL removed, never
a key. With the network off it is
asked first and reaches only the repository's own remote; a forced push, a
clone and a remote added during the session are asked each time. bash's
description tells the model so from its first turn. Details, what is refused
and how it is recorded: [Permissions](04-permissions.md#git-fetch-pull-push-and-clone-through-your-own-git).

Your `user.name` and `user.email` reach git in the sandbox, so its commits
carry them. On the process tier `~/.gitconfig` is readable by commands, and
nothing under `~/.ssh` is. The fence and a container give commands a home of
their own, without your git configuration, so Abhed reads your name and email
on the host as the session starts (the repository's own first, then your
global and system ones, as git would) and gives them to commands as
`GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME` and
`GIT_COMMITTER_EMAIL`. If an `include.path` in your configuration names a
file this account cannot read, git refuses to read any configuration; Abhed
then reads each file on its own and skips only that include.

### Your own clusters and machines

On your own machine, in the terminal, in Abhed Studio and under `abhed rpc`,
the cluster and SSH tools work with the setup you already have, with nothing
to configure:

- **Kubeconfig contexts.** `k8s_get` and `k8s_apply` reach the contexts in
  your kubeconfig (`KUBECONFIG`, every file it lists merged as `kubectl`
  merges them, or `~/.kube/config`). Their descriptions list the contexts by
  name, and the model names one as `context`, or as `cluster`: a name that is
  no cluster in `k8s.clusters` or kept login is that context, and
  `k8s.clusters` wins a clash. The server, the CA and the credential (a token,
  a token file, a client certificate, or a credential helper's token) are
  read in Abhed's own process. None of them reaches the sandbox, the record
  or the model; the approval names the context, its server and how it signs
  in, never the credential.
- **First use asks once.** The first call to each context asks, in every
  mode but bypass, plan and auto included, even for a read. Approving it
  covers the rest of the session; *always for this cluster* keeps it for
  you in `state/machine-grants.json` in your own account's folder (never a
  guest's, which keeps nothing), outside every workspace, so
  later sessions do not ask. A grant covers the context as it was approved:
  if its server, CA, user or credential helper changes, it is asked about
  again. An allow rule such as `k8s_get(context:lab/*)` answers the ask as
  it answers any; a deny rule still refuses. Writes (`apply`, `delete`,
  `scale`, `restart`, `rollback`) ask every time, after a dry run, as before.
- **Credential helpers.** A context whose user has an `exec` plugin (cloud
  sign-in helpers such as `gke-gcloud-auth-plugin` or `kubelogin`) makes
  Abhed run that program to get a token. Its first use names the program
  and its arguments and asks, even where `k8s.enabled` turned the tools on,
  and nothing runs until you approve it.
- **Refused sign-ins.** A context that signs in with a legacy
  `auth-provider` (`gcp`, `azure`, `oidc`) is refused with the reason: that
  flow refreshes tokens by writing them back into your kubeconfig, and
  Abhed never writes to it. Switch the context to its exec plugin, or log in
  with a token. A user name and password (basic auth) is refused too.
- **`~/.ssh/config` hosts.** `ssh`, `ssh_put` and `ssh_get` reach the hosts
  your `~/.ssh/config` names by their `Host` alias, with its `HostName`,
  `User`, `Port`, `IdentityFile`, `ProxyJump` and `UserKnownHostsFile`, and
  the files it `Include`s. The first value for a keyword wins and
  `IdentityFile` adds, as OpenSSH reads them; a `Match` block is not
  evaluated. They sign in with your ssh-agent (`SSH_AUTH_SOCK`) and the keys
  `IdentityFile` names that need no passphrase (add one that does to the
  agent), in Abhed's process only: the named keys first, then the agent's,
  each offered once, and with `IdentitiesOnly yes` only the keys
  `IdentityFile` names. Each hop of `ProxyJump` (at most four) is reached
  the same way, is bounded by `ssh.connect_hosts` when that is set, and the
  address reached through it is judged and recorded by the egress rules as a
  direct one is; each host's key, hops included, must already be in
  `known_hosts`: Abhed never adds one, and an unknown or changed key is
  refused with the `ssh` command to record it yourself. A host that needs
  `ProxyCommand` is listed as not usable, with the reason. The first use of
  each host asks once, as for a context, and every command still asks as
  `ssh` always does. `ssh_connect` naming one of these hosts says it is
  ready by name. An operator's `ssh.hosts` name wins a clash, and
  `ssh.connect_hosts`, when set, bounds these hosts too.

This is on when no configuration file sets `k8s.enabled` (or `ssh.enabled`)
and no managed configuration is in force, and the machine has a kubeconfig
or kept logins (or an `~/.ssh/config` naming a host). With neither, your own
surface still has `k8s_login`, offered to the model from the moment you
paste a login, so a pasted `oc login` works on any machine and costs nothing
until then; `k8s_get` and `k8s_apply` are offered once a login exists. `k8s_apply` is offered
unless a file sets `k8s.allow_writes`. Setting `k8s.enabled` or
`ssh.enabled` to `false`, in any file a workspace's included, turns it off.
A managed configuration or org policy changes none of this unless it
decides for the organisation: it sets a `k8s` key, an `ssh` key, or
`machine_setup: false` (which covers both). A managed file that only turns
on web search, say, leaves your own setup and your kept logins as they are.
Where it sets any `k8s` key, only what it configures counts on the
machine: its own `k8s.enabled` (a person's or workspace's `true` is not
taken), its `k8s.clusters`, which take a token through `k8s_login`, and a
kubeconfig only where it names one in `k8s.kubeconfig`; otherwise no
kubeconfig is read at all. No login is kept, and a pasted login is still
withheld, the model is told logins are off and why, and a login it tries
anyway is refused with the reason ("your organisation's configuration sets
k8s…; ask your administrator"), never as an unknown tool. Where it sets `ssh`, only `ssh.hosts` and `ssh_connect` are
offered, never the person's `~/.ssh/config`. `abhed serve` never offers its
operator's own setup to the sessions it runs.

`abhed doctor` lists the contexts with their servers (and the program a
credential helper runs), the `~/.ssh/config` hosts and what is kept, by name,
never a credential. `abhed machine list` shows what is kept for you, and
`abhed machine forget KIND NAME` (`k8s-context`, `ssh-host` or `k8s-login`)
drops one, so its next use asks again.

### A pasted login

When you paste a credential into a message, such as
`oc login --token=sha256~… --server=https://api.lab.example:6443`, a
`Bearer` token, or a key of a known shape (a private key block, a GitHub,
GitLab, Slack, AWS or cloud API key, a JSON web token), the model and the
record get `[pasted:P1]` in its place, and you are told it was withheld. The
value is held in the session's memory only. The model is told what was
pasted (an OpenShift token for that server), never the value, and offers to
log in: `k8s_login` with `pasted: "P1"` and a short cluster name. That one
approval:

1. reads the server's certificate, sending nothing: one a public CA vouches
   for is verified as any site is; otherwise the CA your kubeconfig holds for
   that server; otherwise the approval shows the certificate the server
   presents now by its SHA-256 fingerprint, and approving pins it, as
   `oc login` asks you to. A login that pins is always put to you, with the
   fingerprint: bypass mode, an allow rule, a runbook or a remembered
   "always" does not answer it, and where no one can be asked (`-p`, a
   scheduled run) it is refused;
2. sends the token to that server only after that check, and stores nothing
   if the server refuses it;
3. stores the token as a secret (`store_as`, by default `K8S_<CLUSTER>_TOKEN`),
   keeps the cluster with its server, its pinned certificate and that secret
   for you, outside every workspace, and logs the session in.

The server is the one you pasted the token with; the model cannot name
another. Pasting the same token again with another server holds it under a
new id for that server, so a corrected `oc login` is the one used. A value
you pasted is withheld wherever it appears again in the session, in a later
message or a tool's output, and a pasted credential's shape is kept out of
the terminal's prompt history, a server's session list and what an editor's
completion or commit message requests send. A later session reaches the cluster by name in `k8s_get` and
`k8s_apply`, which sign in with the kept token themselves: no paste, no
`token_secret` and no `secret(NAME)` rule, since you bound that secret to
that cluster when you approved it. A deny rule on the secret still refuses
it. A name `k8s.clusters` declares, or that is a kubeconfig context, cannot be
taken by a pasted login. Where Abhed does not run on your own machine
(`abhed serve`), a pasted token is withheld the same way and cannot be kept:
store it with `abhed secret set`.

### Cluster and machine logins

`k8s_login` and `ssh_connect` take credentials the same way: by name, from
the store, under a `secret(NAME)` rule. `k8s_login` takes `cluster` and
`token_secret`; `ssh_connect` takes a key path or `password_secret`. Neither
takes a token or password, so none is recorded, shown for approval, or sent
back to the model; one sent anyway is dropped.

Where a credential goes is the operator's or the person's choice, not the
model's. `k8s_login` sends a token only to a cluster named in `k8s.clusters`,
or one you kept from a pasted login, over TLS
verified against the system roots and the configured CA; a URL is refused
before any request, and the approval prompt names the cluster and its server.
`k8s_get` and `k8s_apply` then take `cluster` to use that login.
`k8s_apply` takes one object per call; a JSON list is refused with a message
that says to apply each item in its own call. Each write is dry-run on the
cluster before the approval, which shows what would change; `rollback`
returns a Deployment to an earlier revision, and `k8s_get` with
`wait_seconds` waits for a rollout. `secret_from` builds a Secret from
stored secrets, each under its own `secret(NAME)` rule, and workspace
files, so a TLS key never passes through the model. `k8s_get` shows a
Secret's keys with each value's length and a fingerprint that compares
only while that Abhed runs, never the value or its plain hash.
`ssh_connect` sends a password only to a host whose key is already in
`known_hosts`. `ssh` takes `stdin_secret`, a stored secret's NAME, to give a
remote command a password or key on stdin, and `ssh_put` sends a workspace
file or a stored secret to a host, so neither passes through the model.

A login or a connected host belongs to the session that made it and that
session's subagents, and ends with it. Another session on the same server,
another user's included, never uses it: it keeps the operator's kubeconfig,
`ABHED_K8S_TOKEN` and `ssh.hosts`. Details in
[Clusters and machines](../ops/infrastructure.md).

### Reading a web page

`web_fetch` takes a `url` and returns the page's text: HTML reduced to
headings, paragraphs, list items and links, with scripts and styles removed.
Plain text, JSON, XML and other text types come back as they are; images,
PDFs and other binary types are refused. It reads up to 5 MiB of a page and
returns up to `web_fetch.max_chars` characters per call, with the `start`
to pass to read on. Each part is a new request, so a page that changes
between parts can shift. The result is tagged untrusted like any tool output.

The request is made by Abhed, not the sandboxed shell, and every call is
judged by policy and recorded like any other. With no
`web_fetch.allowed_hosts`, each call asks unless an allow rule matches, in
plan mode too. With it, a listed host runs without asking only on its
scheme's default port (80 for `http`, 443 for `https`); a URL that names
another port asks, since that is another service on the host. In either
case the approval offers "always allow" for the site
(`web_fetch(https://host/*)`), which covers any URL on it for the session,
and whatever such a URL carries.
What it refuses:

- a scheme other than `http` or `https`, and a URL with a user name or
  password;
- any loopback, private, link-local, cloud metadata (`169.254.169.254`,
  `fd00:ec2::254`), carrier-grade NAT, multicast or reserved address. The
  address is checked where the connection is made, on every hop, so a name
  that resolves to a public address once and an internal one the next time
  is refused;
- a URL that holds a value from the secrets store, as written,
  percent-encoded or in any case, and for a value of 12 characters or more
  also base64- or hex-encoded. A value split across the URL, or encoded
  another way, is not caught: the ask, or the host list, is the control for
  that;
- a host not on `web_fetch.allowed_hosts`, when that is set;
- a URL written in any but its one form (see
  [Permissions](04-permissions.md#rules)), including a port with leading
  zeros, an IPv4 address written as IPv6 or as one number, and a path
  segment of dots, a control character or a doubly encoded `.`, `/` or `\`.
  Characters a reader cannot see (format characters, variation selectors,
  the grapheme joiner, Hangul fillers) are dropped before a segment is
  judged, so they cannot make one that reads as `..` pass.

An encoded slash (`%2F`) in the path, such as GitLab's
`projects/group%2Fproject` or npm's `@scope%2Fname`, is fetched only on a host
that no deny or ask rule in the configuration names a path on: a rule such
as `web_fetch(https://gitlab.com/api/*)` refuses it there, since a server
reads `group%2Fproject` as another spelling of a path the rule names, while a
rule on the whole host (`https://gitlab.com/*`) does not. It is written
upper-case (`%2F`), and each part between slashes, encoded or not, is checked
as a segment, so `a%2F..%2Fadmin` is refused as `a/../admin` is. A URL that
carries another URL in its path, as `web.archive.org` links do, is fetched
with that URL's slashes encoded
(`https://web.archive.org/web/2020/https:%2F%2Fexample.com%2F`); written
with plain slashes it has an empty segment and is refused.

It follows a redirect only to the same URL or its upgrade from `http` to
`https`, up to five. Any other redirect, including to another path on the
same host, is handed back to the model, without any user name or password
it carried, and the model fetches it as a new call if it needs it, judged by
the rules again. It ignores `HTTP_PROXY` and
the other proxy variables, since through a proxy it could not check where
the connection goes.

`web_search`'s description points at `web_fetch` only when both are on, and
the system prompt names only the web tools the session has. A search query
that holds a stored secret, in the same forms, is refused before it reaches the provider, as a URL holding one is for
`web_fetch`.

### An edit that would break the file

`edit` and `write` parse the result before they write it, for Go, JSON and
Python. A change that would leave a file that parsed no longer parsing is not
applied: the file stays as it was and the model is told the parser's error and
the line, so it fixes its own text on the next turn. JSON files that allow
comments and trailing commas by convention — `.jsonc`, `tsconfig*.json`,
`jsconfig*.json`, `.eslintrc.json`, `.babelrc.json`, `devcontainer.json`,
`deno.json`, `turbo.json`, `biome.json`, `tslint.json`, `api-extractor.json`,
`cspell.json`, `settings.json`, `launch.json`, and anything in a `.vscode` or
`.devcontainer` directory — are parsed that way; other `.json` files are
strict JSON. A trailing comma is allowed only after a value, so `{,}` or
`[1,,]` still fails.

An `edit` whose new text is a pasted diff hunk — every non-empty line starts
with `+` or `-`, there is at least one of each, and the old text has no such
lines — is refused the same way, except in files where such lines are ordinary
content (Markdown, reStructuredText, YAML, text, CSV, diffs). A file that did
not parse before can still be edited, so a refactor is never blocked half-way,
and a new file is written even if it does not parse, with a warning, unless it
is a pasted diff.

Python is compiled, never run, by the `python3` on the path — the real
interpreter behind it, never one inside the workspace or an added directory,
with site packages and the environment switched off. When `python3` is a
pyenv, asdf or mise shim, the manager is asked which interpreter it runs
(`pyenv which python3`), from a temporary folder, so a version file in the
workspace does not choose it; that interpreter's version then decides. With
no `python3`, or a shim the manager cannot resolve to an interpreter outside
the workspace, Python is not checked. The host's interpreter decides what
parses, so one older than 3.12, which could reject newer syntax the project
accepts, warns instead of refusing.

Edits and saves made by a person in the [workbench](16-workbench.md) are
never refused: they are saved, with the warning. Refused changes appear in
[HawkEYE](15-hawkeye.md) as `broken-edit`. `tools.syntax_check` sets the
behaviour: `refuse` (the default), `report` to apply and warn, or `off`.

The console and workbench, `abhed rpc`, `abhed acp` and `abhed eval` build
the same tool set as the CLI, from one place, together with the system
prompt and its `ABHED.md` memory files. Where one differs it is by design:
the SDK takes the configured tools only with `Options.ConfiguredTools`, and
`abhed eval` leaves out MCP servers, extensions, rag corpora, the code index
and the Kubernetes and SSH tools, so a score depends on the harness and the
task rather than on what those reach.

## Adding your own

Four routes, none of which needs a rebuild. Pick by where your tool already
lives.

| Route | Use it when | Guide |
|---|---|---|
| **MCP server** | the tool exists, or you want the industry-standard interface | [MCP](08-mcp.md) |
| **Extension** | you want a tool in any language, with no protocol to learn | [Extensions](07-extensions.md) |
| **Skill** | it is a procedure, not a program | [Skills](06-skills.md) |
| **SDK** | you are embedding Abhed and can write Go | [SDK](09-sdk.md) |

### The shortest path

An extension declares its tools once and answers when they are called. Any
language; this one is a shell script:

```bash
#!/bin/bash
while IFS= read -r line; do
  case "$line" in
    *'"event":"list_tools"'*)
      echo '{"tools":[{"name":"ticket","description":"Look up a ticket by id",
             "schema":{"type":"object","properties":{"id":{"type":"string"}}},
             "mutates":false}]}' ;;
    *'"event":"invoke_tool"'*)
      id=$(jq -r '.args.id' <<<"$line")
      echo "{\"result\": \"$(fetch-ticket "$id")\"}" ;;
    *) echo '{}' ;;
  esac
done
```

```json
"extensions": [
  { "name": "tickets", "command": "bash", "args": ["/opt/abhed/tickets.sh"] }
]
```

## What a provided tool is, and is not

A tool you add is a tool like any other: it appears in the model's list, its
call goes through the policy engine, and its call and result are recorded as
events. **Adding a tool adds a capability, never a way around the rules** — a
deny rule naming it still wins, and one that mutates is subject to approval
exactly as `write` is.

Two defaults are deliberately strict:

- A tool that does not say whether it **mutates** is assumed to. Abhed cannot
  know what someone else's tool does, and the safe answer is the one that asks.
- A **duplicate name is refused** rather than resolved by load order. Otherwise
  which tool ran would depend on which extension started first, and a policy
  rule naming it would be ambiguous.

## Writing a description the model will act on

The description is the only thing the model sees before deciding to call
something, and it is where most tool integrations fail. Say **when to use it**,
not what it is:

> ❌ "Ticket lookup API client."
> ✅ "Look up a ticket by id when the user names one, or when a commit message
> references one and its details would change what you do."

A vague description produces a tool that is never called, or called for
everything.
