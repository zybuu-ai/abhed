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
| `ssh`, `ssh_connect` | remote execution, off by default |
| `k8s_get`, `k8s_apply`, `k8s_login` | Kubernetes, read-only by default |

`write` creates a new file's missing folders, inside the workspace and never
in Abhed's state, so a new file in a new folder needs no shell approval in
`accept-edits` or `auto` mode. A write that fails takes back the folders it made,
and `/undo` of the new file removes them while they are empty.

`bash` takes an optional `secrets` list: names from the operator's store,
handed to that one command as environment variables when a
`secret(NAME)` rule allows it. The model never sees a value; see
[Secrets](04-permissions.md#secrets).

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
  (`server &`, then the shell exits) is no longer reached: not by
  `shell_kill`, the session closing, a stop or Abhed exiting, nor by revoking
  the session in Enterprise. It keeps running in the sandbox until it ends.
  Keep a long-running process in the foreground of its background shell
  (`server`, not `server &`) so that stopping the shell stops it. If Abhed
  itself is killed, its shells are left running the same way. Bubblewrap on
  Linux is the exception to both: its process namespace ends with the shell,
  and the shell with Abhed.
- A subagent cannot start one, and neither can a surface that runs no
  background work (`abhed eval`).

`bash` says in its description whether commands can reach the network. When
the sandbox has none and a command fails because of it, the result ends with
a note that says so, so the model reports the reason or uses a web tool
instead of retrying.

### Cluster and machine logins

`k8s_login` and `ssh_connect` take credentials the same way: by name, from
the store, under a `secret(NAME)` rule. `k8s_login` takes `cluster` and
`token_secret`; `ssh_connect` takes a key path or `password_secret`. Neither
takes a token or password, so none is recorded, shown for approval, or sent
back to the model; one sent anyway is dropped.

Where a credential goes is the operator's choice, not the model's.
`k8s_login` sends a token only to a cluster named in `k8s.clusters`, over TLS
verified against the system roots and the configured CA; a URL is refused
before any request, and the approval prompt names the cluster and its server.
`k8s_get` and `k8s_apply` then take `cluster` to use that login.
`ssh_connect` sends a password only to a host whose key is already in
`known_hosts`.

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
- a URL that holds a value from the secrets store, as written or
  percent-encoded, in any case. A value encoded otherwise (base64, hex) or
  split across the URL is not caught: the ask, or the host list, is the
  control for that;
- a host not on `web_fetch.allowed_hosts`, when that is set;
- a URL written in any but its one form (see
  [Permissions](04-permissions.md#rules)), including a port with leading
  zeros, an IPv4 address written as IPv6 or as one number, and a path
  segment of dots, a control character or a doubly encoded `.`, `/` or `\`.
  A URL that carries another URL in its path, as `web.archive.org` links do
  (`https://web.archive.org/web/2020/https://example.com/`), has an empty
  segment and cannot be fetched. Nor can one with an encoded slash (`%2F`)
  in its path, such as GitLab's `projects/group%2Fproject` or npm's
  `@scope%2Fname`: written with `/` it names another resource, so no
  spelling is suggested.

It follows a redirect only to the same URL or its upgrade from `http` to
`https`, up to five. Any other redirect, including to another path on the
same host, is handed back to the model, without any user name or password
it carried, and the model fetches it as a new call if it needs it, judged by
the rules again. It ignores `HTTP_PROXY` and
the other proxy variables, since through a proxy it could not check where
the connection goes.

`web_search`'s description points at `web_fetch` only when both are on, and
the system prompt names only the web tools the session has. A search query
that holds a stored secret, as written, percent-encoded or in another case,
is refused before it reaches the provider, as a URL holding one is for
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
strict JSON.

An `edit` whose new text is a pasted diff hunk — every non-empty line starts
with `+` or `-`, there is at least one of each, and the old text has no such
lines — is refused the same way, except in files where such lines are ordinary
content (Markdown, reStructuredText, YAML, text, CSV, diffs). A file that did
not parse before can still be edited, so a refactor is never blocked half-way,
and a new file is written even if it does not parse, with a warning, unless it
is a pasted diff.

Python is compiled, never run, by the `python3` on the path — the real
interpreter behind it, never one inside the workspace or an added directory,
with site packages and the environment switched off. With no `python3`, or
when `python3` is a version-manager shim (pyenv, asdf) that needs its
environment, Python is not checked. The host's interpreter decides what
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
