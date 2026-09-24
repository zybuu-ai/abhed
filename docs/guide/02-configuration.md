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
| `tools` | `syntax_check`: whether an edit that breaks a file is refused, reported or allowed — [Tools](05-tools.md#an-edit-that-would-break-the-file) |

## Context

```json
"context": {
  "offload_at": 0.60,
  "compact_at": 0.80,
  "memory_files": ["ABHED.md"]
}
```

Two things happen as the window fills, in this order.

**`offload_at` — nothing is lost.** Past this fraction, old and large tool
results are replaced *in the window* by a short stub: what the call was, how
the result began, and its `call_id`. The full text is already in the session
record and stays there. The agent gets it back with the `recall` tool — by
`call_id` for one result in full, or by `query` to search everything said and
returned in the session. The four most recent results are never touched, and
neither is anything under 2,000 characters, since a stub costs about as much.
It costs no model call, and it all happens in one pass per crossing, because
rewriting old messages invalidates the endpoint's prefix cache and that is
worth paying once rather than every turn. `0` turns it off; unset means 0.60.

This matters most on a local model with a 16–32k window, where a few file
reads fill the context and the alternative is to compact early and often. A
session that offloads well may never need to compact at all.

What this is and is not: nothing is lost from the record, and anything dropped
from the window can be retrieved. It is not "lossless context" — the window is
still finite, and the model still cannot see everything at once. `recall` reads
this session's record and no other; the session is fixed when the tool is
built, not passed as an argument.

**`compact_at` — the summary.** At this fraction the history is summarized. The
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

**Two roles, not one.** The audit record is only as protected as the role that
writes it. Database triggers refuse an `UPDATE`, a `DELETE` or a `TRUNCATE` on
`events` — but the role that *owns* a table may disable its triggers or drop
it, and nothing inside the database can stop an owner. So:

```sql
CREATE ROLE abhed_owner   LOGIN PASSWORD '…' NOSUPERUSER NOBYPASSRLS;  -- owns the tables
CREATE ROLE abhed_runtime LOGIN PASSWORD '…' NOSUPERUSER NOBYPASSRLS;  -- the server runs as this
GRANT ALL ON SCHEMA public TO abhed_owner;
```

```bash
# Once, and after each upgrade — ideally from somewhere other than the server host:
ABHED_MIGRATE_DATABASE_URL='postgres://abhed_owner:…@db/abhed' abhed migrate

# The server only ever sees the runtime role:
ABHED_DATABASE_URL='postgres://abhed_runtime:…@db/abhed' abhed serve
```

`abhed migrate` applies the schema as the owner and grants the runtime role
exactly what the server uses: `INSERT` and `SELECT` on `events`, and nothing
that changes or removes one. Keep the owner's credentials off the host that
runs the server; whoever holds them can alter the record.

**Abhed refuses to start** if the role in `storage.dsn` could alter the record
— if it owns `events`, or holds `UPDATE`, `DELETE` or `TRUNCATE` on it — and
says which. It asks the database what the connection can do rather than
trusting the configuration.

**`"single_role": true`** turns that refusal off: the server connects as the
role that owns the tables and applies the schema itself, as every version up to
0.2 did. It is the simple setup for a laptop or a trial. It is also weaker, and
the difference is exact: the record is then protected against application bugs
and stray statements, and **not** against anyone holding the server's database
credentials. `abhed doctor` and the startup banner say which mode is in force.

**Do not connect as a superuser,** in either mode. Row-level security is what
isolates tenants, and Postgres does not apply it to a superuser or a
`BYPASSRLS` role, not even with `FORCE`. Abhed checks and refuses, because a
control that is silently off is worse than one that is visibly missing.

## Accounts

With `auth.mode` set to `local` and no database, accounts live in a file:
`<workspace>/.abhed/users.json` by default, or wherever `auth.users_file`
points (`ABHED_USERS_FILE` overrides it). A server deployment sets it to a
directory outside every workspace, so accounts never sit in a tree an agent
is pointed at. With Postgres, accounts are rows and the file is not used.

### Keys for the paid editions

The Community Edition checks these when it loads a config, so a mistake is
reported at once, and otherwise ignores them: GitHub sign-in is part of the
paid editions.

| Key | Meaning |
|---|---|
| `auth.github.orgs` | admit members of any of these GitHub organisations |
| `auth.github.teams` | admit members of any of these teams, each written `org/team-slug` |
| `auth.github.allow_any` | admit every GitHub account; cannot be combined with `orgs` or `teams` |

```json
{ "auth": { "github": { "orgs": ["acme"], "teams": ["acme/platform"] } } }
```

`auth.proxy_logout_url` is the authenticating proxy's own sign-out, in `proxy`
mode: with it the console offers Sign out and `/logout` redirects there;
without it there is no Sign out, since the proxy owns the session.

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
