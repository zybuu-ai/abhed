# Configuration

Abhed reads `.abhed/config.json` from the workspace. `abhed init` writes a
starter file; everything below is optional and has a default.

```json
{
  "model": {
    "default": "local",
    "providers": {
      "local": {
        "type": "ollama",
        "base_url": "http://127.0.0.1:11434/v1",
        "model": "qwen3-coder:30b",
        "context_window": 32768,
        "params": { "temperature": 0.2, "top_p": 0.9 }
      }
    }
  },
  "permissions": { "mode": "default" },
  "storage": { "driver": "memory" }
}
```

## Sections

| Section | What it controls |
|---|---|
| `model` | providers and which one is default — [Models](03-providers.md) |
| `permissions` | what runs unattended — [Permissions](04-permissions.md) |
| `context` | compaction threshold and memory files |
| `limits` | turn, token and subagent budgets |
| `sandbox` | process isolation and network access |
| `storage` | in-memory or Postgres |
| `auth` | who may use a server deployment |
| `skills` | where skills are loaded from — [Skills](06-skills.md) |
| `extensions` | processes that can intercept — [Extensions](07-extensions.md) |
| `mcp` | Model Context Protocol servers — [MCP](08-mcp.md) |
| `custom_providers` | providers added without a rebuild |
| `web_search` | provider and result count |
| `retrieval`, `rag` | the local index, and external corpora |
| `k8s`, `ssh` | infrastructure tools, off by default |
| `additional_dirs` | directories outside the workspace the agent may reach |

## Context

```json
"context": {
  "compact_at": 0.80,
  "memory_files": ["ABHED.md"]
}
```

`compact_at` is the fraction of the window at which history is summarized. The
check reserves headroom for the turn about to happen, so a large tool result
cannot take a session from under the threshold to over the hard limit in one
step. Below 1.0 with real margin: hitting the limit mid-turn is unrecoverable
and the token estimate is approximate.

`ABHED.md` in the workspace is loaded into every session and re-injected whole
after compaction. Project conventions belong there.

**Size `context_window` for what the model can actually hold.** A local server
reports the size it chose at startup; asking for more does not fail loudly, it
simply stops fitting once a long session fills it.

## Limits

```json
"limits": {
  "max_turns": 100,
  "max_budget_tokens": 2000000,
  "max_subagents": 8,
  "nested_subagents": 2
}
```

`max_budget_tokens` caps the whole session: the primary agent and every
subagent it spawns draw on one allowance, so a fan-out cannot multiply spend
invisibly. A session that exhausts it ends with the terminal reason
`max_budget`, checked at a turn boundary so a turn already in flight
finishes. Zero means no cap.

## Sandbox

```json
"sandbox": { "allow_network": false }
```

Shell commands run under process isolation with writes scoped to the workspace.
This is a boundary, not a jail: it is not sufficient for genuinely hostile code.

## Storage

```json
"storage": {
  "driver": "postgres",
  "dsn": "postgres://abhed:...@localhost:5432/abhed",
  "tenant": "default"
}
```

`memory` (the default) loses sessions when the process exits. `postgres` makes
them durable and replayable, and is what `/sessions`, `/resume` and audit need.

**Do not connect as a superuser.** Row-level security is what isolates tenants,
and Postgres does not apply it to a superuser or a `BYPASSRLS` role, not even
with `FORCE`. Abhed checks the role it connected as and **refuses to start** if
it is privileged, because a control that is silently off is worse than one
that is visibly missing. Provision two roles for this reason: a superuser used
only to create the database and the application role, and a plain application
role (`NOSUPERUSER NOBYPASSRLS`) that owns the tables and is the only one in the
server's DSN. Abhed applies its schema on connect as that role.

## Where settings come from

Later sources win, except that an org-managed file cannot be overridden:

1. built-in defaults
2. `~/.abhed/config.json`
3. `.abhed/config.json` in the workspace
4. environment (`ABHED_DATABASE_URL` and similar)
5. command-line flags
6. **managed settings**, which nothing below can loosen

Run `abhed doctor` after any change. It reports what is actually in effect,
which is not always what the file appears to say.
