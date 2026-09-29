# Tools

## Built in

| Tool | |
|---|---|
| `read`, `write`, `edit` | files, scoped to the workspace |
| `glob`, `grep` | find files and search contents |
| `bash` | shell, sandboxed, destructive commands always confirm |
| `todo` | the agent's task list for multi-step work |
| `skill` | load a procedure on demand |
| `web_search` | five providers: duckduckgo, brave, tavily, serper, searxng; off by default |
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

`bash` says in its description whether commands can reach the network. When
the sandbox has none and a command fails because of it, the result ends with
a note that says so, so the model reports the reason or uses a web tool
instead of retrying.

### Reading a web page

`web_fetch` takes a `url` and returns the page's text: HTML reduced to
headings, paragraphs, list items and links, with scripts and styles removed.
Plain text, JSON, XML and other text types come back as they are; images,
PDFs and other binary types are refused. It reads up to 5 MiB of a page and
returns up to `web_fetch.max_chars` characters per call, with the `start`
to pass to read on. The result is tagged untrusted like any tool output.

The request is made by Abhed, not the sandboxed shell, and every call is
judged by policy and recorded like any other. With no
`web_fetch.allowed_hosts`, each call asks unless an allow rule matches; the
approval offers "always allow" for the site (`web_fetch(https://host/*)`).
What it refuses:

- a scheme other than `http` or `https`, and a URL with a user name or
  password;
- any loopback, private, link-local, cloud metadata (`169.254.169.254`,
  `fd00:ec2::254`), carrier-grade NAT, multicast or reserved address. The
  address is checked where the connection is made, on every hop, so a name
  that resolves to a public address once and an internal one the next time
  is refused;
- a URL that holds a value from the secrets store, as written or
  percent-encoded;
- a host not on `web_fetch.allowed_hosts`, when that is set;
- a URL written in any but its one form (see
  [Permissions](04-permissions.md#rules)).

It follows up to five redirects within the same site, and the upgrade from
`http` to `https`. A redirect to another host is handed back to the model,
which fetches it as a new call if it needs it. It ignores `HTTP_PROXY` and
the other proxy variables, since through a proxy it could not check where
the connection goes.

`web_search`'s description points at `web_fetch` only when both are on, and
the system prompt names only the web tools the session has.

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
