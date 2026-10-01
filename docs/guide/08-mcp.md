# MCP

Abhed is an MCP client and gateway. A tool exposed by a Model Context Protocol
server appears in the model's tool list like any built-in one.

```json
"mcp": {
  "servers": [
    { "name": "github", "command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": { "GITHUB_TOKEN": "${GITHUB_TOKEN}" } },
    { "name": "corpus", "url": "https://retrieval.internal/mcp",
      "headers_env": { "Authorization": "CORPUS_TOKEN" } }
  ]
}
```

Both transports are supported: **stdio** for a local process, **HTTP** for a
remote service.

## Namespacing and policy

Remote tools are namespaced by server, so two servers offering `search` do not
collide, and a policy rule can name one precisely:

```json
"deny": ["github__create_issue"]
```

Every MCP tool routes through the policy engine, because Abhed cannot know what
someone else's tool does. A remote tool is subject to approval exactly as `bash`
is, and its result is recorded and tagged untrusted.

## Failure

A server that will not start is reported and skipped; the agent runs without it
rather than refusing to start. Tool lists are fetched lazily on first use, so an
unreachable server costs nothing until something needs it.

In a session, `/mcp` lists each enabled server, connected or with the reason
it is not, and its tools; `/mcp restart <server>` reconnects one, and the
tools already offered reach the new connection.

## Many tools

With more than 40 MCP tools across the servers, the model is not given each
one. It gets a `tool_search` tool instead, whose description lists each
server and its tool names (names only; a name with anything but letters,
digits, `_`, `.` and `-`, or past about 2.5 KB of names, is counted rather
than shown), and the system prompt says to use it. `tool_search` finds tools
by the words in their names and descriptions and loads the ones it returns,
with their parameters, from the next step. A loaded tool is policed, approved and
recorded like any other, and a tool the model never searched for can still
only run through policy.
