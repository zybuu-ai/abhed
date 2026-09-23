# Tools

## Built in

| Tool | |
|---|---|
| `read`, `write`, `edit` | files, scoped to the workspace |
| `glob`, `grep` | find files and search contents |
| `bash` | shell, sandboxed, destructive commands always confirm |
| `todo` | the agent's task list for multi-step work |
| `skill` | load a procedure on demand |
| `web_search` | five providers: duckduckgo, brave, tavily, serper, searxng |
| `ssh`, `ssh_connect` | remote execution, off by default |
| `k8s_get`, `k8s_apply`, `k8s_login` | Kubernetes, read-only by default |

`bash` takes an optional `secrets` list: names from the operator's store,
handed to that one command as environment variables when a
`secret(NAME)` rule allows it. The model never sees a value; see
[Secrets](04-permissions.md#secrets).

### An edit that would break the file

`edit` and `write` parse the result before they write it, for Go, JSON and
Python. A change that would leave a file that parsed no longer parsing is not
applied: the file stays as it was and the model is told the parser's error and
the line, so it fixes its own text on the next turn. An `edit` whose new text is
a pasted diff hunk — every line starting with `+` or `-`, removing a line of the
old text or pairing a removal with an addition — is refused the same way,
except in files where such lines are ordinary content (Markdown, YAML, text,
CSV, diffs). A file that did not parse before can still be edited, so a
refactor is never blocked half-way, and a new file is always written, with a
warning if it does not parse.

Python is compiled, never run, by the `python3` on the path — the real
interpreter behind it, never one inside the workspace, with site packages and
the environment switched off. With no `python3`, Python is not checked. The
host's interpreter decides what parses, so one older than 3.12, which could
reject newer syntax the project accepts, warns instead of refusing.

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
