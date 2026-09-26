# Abhed — Data Model & API

Status: Draft · 2026-09-02 · Evidence status: [E], structured by P6 (event sourcing).

The event stream is the source of truth. Sessions, audit logs, replay, and the eval harness
are all projections of it. Get this right and audit/replay/eval come nearly free; get it
wrong and you will retrofit them expensively.

## 1. Event model

Every state change is an immutable, ordered event.

```go
type Event struct {
    ID        string    // ULID — sortable by time
    SessionID string
    ParentID  string    // subagent parent, empty for root
    Seq       int64     // monotonic within session
    Type      EventType
    Payload   json.RawMessage
    Actor     Actor     // user | agent | system | tool
    Trust     Trust     // trusted | untrusted  ← provenance (arch §6)
    CreatedAt time.Time
}
```

`Trust` is the field that makes injection defense enforceable: content from files, tool
output, MCP responses, and search results is tagged `untrusted` at ingest and stays tagged.
Policy reads it; the context assembler renders it in a distinct structural block.

### Event types

| Type | Payload | Emitted by |
|---|---|---|
| `session.started` | workspace, model, mode, origin; recorded today for a workbench session, which is opened without a prompt | system |
| `terminal.input` | call id of the shell, the line as typed, `edited`, or `withheld` with a reason | user |
| `user.message` | text, attachments | user |
| `agent.message` | text, reasoning (stripped from history) | agent |
| `action.requested` | tool, args | agent |
| `action.approved` / `.denied` | rule matched, actor | policy |
| `observation` | result, truncated, exit code | tool |
| `subagent.spawned` / `.returned` | prompt, summary, tokens | orchestrator |
| `compaction.started` / `.completed` | before/after tokens, summary | context mgr |
| `plan.updated` / `todo.updated` | items | agent |
| `session.ended` | terminal reason, totals | system |

**Terminal reasons** are an enum, not a string — CI exit codes and the eval harness both
depend on distinguishing them (§09):
`completed · max_turns · max_budget · policy_denied · user_interrupt · error · shutdown · retry_exhausted`

`shutdown` means the node exited while the turn was running, and it is
deliberately distinct from `user_interrupt`: nobody asked for it to stop, so it
is a turn to resume rather than a decision to respect. With `server.drain_seconds`
set, a shutdown first stops accepting turns — new requests get 503 with
`Retry-After` so a balancer moves on — and waits for the running ones, so a
rolling deploy records no `shutdown` at all unless a turn outlives the budget.

## 2. Schema

```sql
CREATE TABLE sessions (
  id             TEXT PRIMARY KEY,
  tenant_id      TEXT NOT NULL,
  user_id        TEXT NOT NULL,
  workspace      TEXT NOT NULL,
  model          TEXT NOT NULL,
  prompt_hash    TEXT NOT NULL,        -- traces results to exact prompt (§07)
  harness_version TEXT NOT NULL,
  mode           TEXT NOT NULL,
  parent_id      TEXT REFERENCES sessions(id),   -- subagents
  started_at     TIMESTAMPTZ NOT NULL,
  ended_at       TIMESTAMPTZ,
  terminal_reason TEXT,
  tokens_in      BIGINT DEFAULT 0,
  tokens_out     BIGINT DEFAULT 0,
  tokens_cached  BIGINT DEFAULT 0,     -- cache hit rate (P8)
  gpu_seconds    NUMERIC DEFAULT 0,
  cost_usd       NUMERIC DEFAULT 0
);

CREATE TABLE events (
  id          TEXT PRIMARY KEY,
  session_id  TEXT NOT NULL REFERENCES sessions(id),
  parent_id   TEXT,
  seq         BIGINT NOT NULL,
  type        TEXT NOT NULL,
  payload     JSONB NOT NULL,
  actor       TEXT NOT NULL,
  trust       TEXT NOT NULL DEFAULT 'trusted',
  created_at  TIMESTAMPTZ NOT NULL,
  UNIQUE (session_id, seq)
);
CREATE INDEX ON events (session_id, seq);
CREATE INDEX ON events (type, created_at);
CREATE INDEX ON events USING GIN (payload);

CREATE TABLE checkpoints (          -- backs /undo (§09)
  id         TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id),
  event_seq  BIGINT NOT NULL,
  path       TEXT NOT NULL,
  before     BYTEA,                 -- NULL = file did not exist
  created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE models (               -- registry + capability profile (§08 L2)
  name         TEXT PRIMARY KEY,
  provider     TEXT NOT NULL,
  endpoint     TEXT NOT NULL,
  context_window INT NOT NULL,
  profile      JSONB NOT NULL,      -- conformance results
  enabled      BOOLEAN DEFAULT false,
  registered_at TIMESTAMPTZ NOT NULL
);
```

**Events are append-only.** No UPDATE, no DELETE. Retention is by partition drop, which keeps
the audit guarantee intact — an audit log you can edit is not an audit log.

Tenant isolation is enforced in the query layer *and* by row-level security. A boundary that
exists in only one place is not a boundary (§ops).

## 3. Session protocol

CLI and web console both consume the same stream over SSE. One implementation.

```
POST /v1/sessions                    → {session_id}
POST /v1/sessions/{id}/messages      → 202, events stream; to a busy session
                                       {delivery:"steered", queue_id}, or with
                                       "interrupt":true a fresh turn
GET  /v1/sessions/{id}/events        → SSE (resumable via Last-Event-ID or ?after=seq)
GET  /v1/sessions/{id}/queue         → messages waiting for the next turn boundary
DELETE /v1/sessions/{id}/queue/{qid} → 204, or 404 once the loop has read it
                                       (these and /interrupt: 421 + Abhed-Session-Node
                                       for a session on another node)
POST /v1/sessions/{id}/interrupt     → 204
POST /v1/sessions/{id}/approve       {approved, scope?, request_id?} → on the running node:
                                       204 taken by the waiting turn; 200 {recorded:true,
                                       applied:false} recorded after the turn stopped waiting
                                       (nothing ran on it); 409 nothing pending, another or an
                                       ended request named, or already answered; 429 with
                                       Retry-After too many answers waiting; 503 row not
                                       written in time. Elsewhere: 421
                                       for a bound answer, 204 for an unbound one recorded on
                                       the store (request_id: the action.requested event's id)
POST /v1/sessions/{id}/compact       → 202
GET  /v1/sessions/{id}/replay        → full event list
DELETE /v1/sessions/{id}             → end session
```

Wire format:

```json
{"id":"01J...","seq":42,"type":"action.requested","actor":"agent","trust":"trusted",
 "payload":{"tool":"edit","args":{"path":"/w/auth.go","old_string":"...","new_string":"..."},
            "requires_approval":true,"reason":"changing a file needs approval in default mode"}}
```

`Last-Event-ID` resumption is what makes `/resume` and reconnect-after-network-drop work.
`?after=` does the same for a client that opens a new `EventSource`, which cannot set the header.

## 4. Model adapter interface

The single seam that makes Abhed model-agnostic (arch §5, P12).

```go
type Adapter interface {
    Complete(ctx context.Context, req Request) (<-chan Chunk, error)
    Capabilities() Profile
    CountTokens(msgs []Message) (int, error)
}

type Request struct {
    System    []Block          // cached prefix (§07 layers 1-4)
    Messages  []Message
    Tools     []ToolDef
    MaxTokens int
    Effort    EffortLevel      // per-model, per-task-class (P11)
    Stop      []string
}

type Profile struct {
    ContextWindow    int
    SupportsTools    bool
    SupportsStreaming bool
    ToolCallFormat   string   // json | xml | pythonic | harmony
    ReasoningTokens  bool
    GuidedDecoding   bool
    CachePrefix      bool     // if false, expect the 17x penalty (P8)
    Conformance      map[string]float64  // §08 L2 results
}
```

**Everything model-specific lives behind this interface** — tool-call parsing, reasoning-token
stripping, chat templates, structured-output strategy. Per §07 anti-patterns, if you find
yourself forking the *prompt* per model, the difference belongs here instead.

## 5. Configuration

```yaml
# ~/.abhed/config.yaml  or  /etc/abhed/config.yaml (managed, wins)
model:
  default: local-qwen
  providers:
    local-qwen:
      type: openai-compatible          # works with vLLM, SGLang, llama.cpp, Ollama...
      base_url: http://localhost:8000/v1
      model: Qwen/Qwen3-32B
      api_key_env: ABHED_API_KEY
      context_window: 131072
      tool_call_format: json
permissions:
  mode: default
  deny:  ["bash(rm -rf *)", "bash(git push --force*)", "write(/etc/**)"]
  allow: ["bash(git status)", "bash(go test*)", "read(**)"]
context:
  compact_at: 0.90
  memory_files: [ABHED.md, ABHED.local.md]
limits:
  max_turns: 100
  max_subagents: 20
  nested_subagents: false
```

Precedence: managed (`/etc/abhed`) → project → user → flags. **Managed always wins** — that
asymmetry is what makes org policy enforceable (P7).

The `openai-compatible` provider type is deliberately the primary path: it covers vLLM,
SGLang, TensorRT-LLM's OpenAI server, llama.cpp, Ollama, and every hosted API worth
supporting. One adapter, many backends.
