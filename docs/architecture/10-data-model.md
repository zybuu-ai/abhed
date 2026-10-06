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
| `session.started` | workspace, model, mode, origin (`chat` or `workbench`), provider; the first event of every session the server starts, so a resume keeps its provider. The CLI's also carries `surface`, `headless`, the permission `mode` it started in, `bypass_confirmed` (true only when bypass came from a confirmed `-dangerously-skip-permissions`) and the system prompt's digests. A CLI run that continues a recorded conversation (`-c`, `-r`, `/resume`, `-fork-session`) records its own after the record, with `resumed: true` and `through_seq`, the last step it goes on from, followed by a `mode.changed` when its mode is not the one the record was left in; a conversation rebuilt within the same process records none. A new `abhed acp` or `abhed rpc` session records one too, with `surface` (`acp`, `rpc`), `headless`, `provider`, `model` and `mode` | system |
| `terminal.input` | call id of the shell, the line as typed, `edited`, or `withheld` with a reason | user |
| `user.message` | text, attachments | user |
| `agent.message` | text, reasoning (stripped from history) | agent |
| `action.requested` | tool, args (always a JSON object, the canonical arguments every step and the tool read; `{}` on a call refused at step `args`); `raw_args`, the refused arguments as text; `dropped_args`, keys a built-in tool did not declare and dropped; `resolved`, arguments the harness set or rewrote before policy read the call, such as the cluster of the session's only login or a Kubernetes call's default namespace; `via` when something issued it for the agent, such as `skill research pipeline` for a skill pipeline's step (recorded in the record of the loop whose `skill` call ran the pipeline) | agent |
| `action.approved` / `.denied` | the step that decided (`step`), `reason`, `by`; `rule`, the rule as written, when a deny, ask or allow rule decided; `scope` when a remembered scope allowed it; `approver` and `granted_scope` when a person answered (below) | policy |
| `observation` | result, truncated, exit code; `sandbox`, the tier a `bash` command ran under (`none` on the host), when known | tool |
| `observation` with `not_run` | the answer to an approved call its turn ended before running (an interrupt, a shutdown): `is_error`, and a "Not run" text. It is a result, not an outcome, and HawkEYE does not mark the call run | system |
| `message.dropped` | queue id, client id, text, when it was queued, reason; a queued message the model never read because the server stopped first | system |
| `subagent.spawned` / `.returned` | description, agent type, `session` (the subagent's own record), turns, tokens; written to the parent's record and the subagent's; a nested subagent's are passed up to the top-level record. `spawned` also names the role and what it ran with: `definition`, `definition_source` (`builtin`, `managed`, `workspace`, `operator`), `definition_sha256` (a loaded file's content hash) and `tools` (sorted). Both events name `model`, and `provider` when the configured provider is known: the one the call or definition chose, or the parent's. A background task's carry `background: true` and `task_id` (the subagent's session id); `spawned` adds `branch` and `start` for a worktree, and for a resume `resume: true`, `through_seq` (the child's last event it goes on from) and `definition_changed`. `returned` names `end_seq`, the child's `session.ended` for that run, whose answer the return carries. `returned` reasons beyond the terminal reasons: `cancelled_by_parent`, `session_deleted`, `session_closed`, `owner_inactive`, `owner_revoked` (the owner's access was withdrawn), `lost` (reconciled after a crash) | orchestrator |
| `subagent.notice` | a background task's result, or with `kind: "shell"` a background shell's end, entering the conversation, in the parent's record, recorded before it is applied: `task_id`, `session`, `description`, `status` (`completed`, `failed`, `cancelled`, or the cap that ended it), `reason`, `turns`, `tokens_in`, `tokens_out`, `provider`, `model`, `call_id`, `content` (the summary, redacted), `delivery` (`boundary`, `idle` or `wake`), `wake` (`notify`, `auto`, `caller` or `skipped:<reason>`). Marked untrusted. Fork rebuilds it as an assistant `task_status` call with that `call_id` and its tool result | system |
| `shell.started` | a command `bash` started with `run_in_background`, recorded once it runs: `shell_id`, `call_id` (the `bash` call that started it), `command` (as policy judged it, redacted), `description`, `sandbox` (the tier, `none` on the host), `secrets` (names only), `lifetime_ms`, and `from_foreground` when you moved a running foreground command to the background with Ctrl-B (`call_id` is then the call it ran under). Its output is not recorded here; each `shell_output` or `shell_kill` read is an `observation`, redacted | system |
| `shell.ended` | a background shell ended: `shell_id`, `call_id`, `state` (`exited` or `killed`), `exit_code` (128 plus the signal for a kill), `reason` for a kill (`cancelled_by_parent` for `shell_kill` or `task_cancel`, `user_interrupt` for a person's stop, `session_closed`, `owner_revoked` when the owner's access was withdrawn, `shutdown` when Abhed exits, `lifetime`), `duration_ms`, `output_bytes`, `truncated` (output went past the 1 MiB kept). Its notice follows as `subagent.notice` with `kind: "shell"`, unless the agent killed it itself | system |
| `session.woken` | a run no person prompted, for background results, recorded before it: `by` (`policy` or `caller`), `wake_mode`, `task_ids`, `wakes_last_hour`. No message | system |
| `session.wake_set` | the session's wake mode changed: `wake`, `by`, `ceiling` | user |
| `session.renamed` | a person gave the session a title: `title`, `from`, `by`; an empty title clears it. The session row's `title` follows the latest | user |
| `subagent.action` | a subagent's call that was refused or put to an approver, in the parent's record: `session`, `call_id`, `tool`, `subject`, `decision` (`allowed` or `denied`), `step`, `reason`, `by`, and `scope`, `approver`, `granted_scope` as on `action.approved`, and `request_id`, the subagent's `action.requested`. Calls the policy allowed on its own are only in the subagent's record | the answer's actor |
| `subagent.ask` | a subagent's call put to the approver, in the parent's record when its turn to be asked comes, one at a time across subagents running together: `session`, `subagent` (its description), `request_id` (the id an answer names), `call_id`, `tool`, `args`, `subject`, `reason`, `scope`, `via`. Its answer follows as `subagent.action` with the same `request_id` | agent |
| `subagent.message` | a message you sent a running subagent (from the terminal's work panel, `Message @<agent>`), copied into the parent's record as yours: `session`, `text` (redacted), `by: user`. The subagent's own record holds it as a `user.message` | user |
| `subagent.backgrounded` | a subagent started in the foreground that you moved to the background while it ran (the terminal's Ctrl-B): `session`, `task_id`, `description`, `background: true`, `by: user`. Its `subagent.return` then carries `background` and `task_id`, and its result arrives as a background task's does | user |
| `compaction.started` / `.completed` | `before_tokens`, `after_tokens`, `summary`, `trigger` (`auto` or `manual`). A `started` is recorded only once there is older history to summarise into a shorter one, and every `started` is followed by a `.completed`: with the token counts, or with `error` when the summary failed. When there is nothing to summarise (automatic or `/compact`) neither is recorded and the compaction count does not rise | context mgr |
| `plan.updated` / `todo.updated` | items | agent |
| `model.call` | one round trip to the model: `turn`, `model`, `tokens_in`, `tokens_out`, `tokens_cached`, `context_window`, `first_token_ms`, `latency_ms`, `tool_calls`, `cut_off`, `error`, and `retryable` when a call or stall timeout ended it; `purpose` names a call outside the conversation (`suggestion`), whose tokens count in the session's totals and budget but which is no turn and says nothing of the context | system |
| `suggestion.offered` | a next prompt offered to the person after a run completed: `text` (one line, at most 80 characters, control and format characters removed, redacted), `turn`. Recorded after the run's `session.ended` (which says `suggesting: true`), by a call made off the run, and followed by that call's `model.call` with `purpose: suggestion`, which is always recorded last, offered or not (a session that closes first records neither); the next prompt, a wake or typing cancels it; never after an error, a stop, a wake run, or while an approval waits. Only shown, never sent: the person accepts it into the input and sends it themselves | system |
| `session.ended` | terminal reason, totals; `background`, what the session still owes at a run's end: background tasks running, results not yet delivered and a wake starting (the session is not over while it owes any: in Postgres its row stays open and the event stream stays open); `settled`, the closing end once it is all done; `detail`, more of how a person stopped the run (the CLI's Esc: `turn interrupted, background shells kept`; Ctrl-C: `interrupted, background shells stopped`; a stop signal to the process: `stopped by terminated`, `stopped by interrupt` or `stopped by hangup`); `suggesting`, a next-prompt suggestion follows (the event stream stays open until its `model.call`); `recovered`, written by reconciliation after a crash. Terminal reason `wake_limit` ends a wake run (exit code 0) | system |
| `conversation.forked` | `through_seq`; the conversation goes on from that step, and the steps between it and the marker are abandoned: kept in the record for audit, left out of every rebuild (`/fork`, `/tree`, `/resume`, a continued session) | user |

**The interactive CLI's own events.** These types and their payloads are
defined (`internal/agent/event_cli.go`) so the work that records them shares
one shape. The interactive CLI records those without a note; a row marked
*(not yet emitted)* is defined for work still to come, and the change that
records it removes the note. A change made before a conversation has a record,
such as a mode chosen before the first message, is recorded when the
conversation opens, ahead of that message. A conversation opened after
`/clear` or `/resume` first restates what it inherits: the mode, as
`mode.changed` with `via: carried`, and each added directory as
`workspace.dir_added`, so each record stands alone. Rewind reuses `conversation.forked`
and adds `file.restored`.

| Type | Payload | Emitted by |
|---|---|---|
| `mode.changed` | `from`, `to`, `by`, `via` (`flag`, `slash`, `shift-tab`, `plan-exit`, or `carried`: a conversation opened after `/clear` or `/resume` restating the mode it inherits) | user |
| `permission.changed` | `op` (`add` or `remove`), `list` (`allow`, `ask` or `deny`), `rule`, `scope` (`session`), `by`; a session rule, which ends with the session | user |
| `workspace.dir_added` | `path` as typed, `canonical` with symlinks resolved, `access` (`read` or `read-write`), `by` | user |
| `input.mention` | a file attached with `@`: `path`, `range` (`10-20`, absent for the whole file), `sha256` of the text attached, `bytes`, `truncated`; not the content, which the message carries | user |
| `command.invoked` | `name`, `source` (`builtin`, `user`, `workspace`, `managed` or `mcp`, set by the loader, never by the command), `sha256` of a command file's content, `args` redacted; recorded for custom commands, `/init` and `/output-style` | user |
| `memory.loaded` | `files`, each `path` (relative to the workspace when in it), `scope` (`managed`, `user`, `project`, `local`, `import`, `rule` or `auto`) and `sha256`; once per conversation | system |
| `memory.written` | `path`, `kind` (`user`, `feedback`, `project`, `reference`, `note` or `import`), `by` (`user` or `agent`); an agent's write is recorded untrusted | the writer |
| `session.named` | `name`, from `-n` or `/rename` | user |
| `session.branched` | the first event of a new session: `from`, the session it was copied from; `through_seq`, the last event taken; `unverified`, why the source's record failed verification, absent when it verified. A copy of the source's conversation and undo history follows, renumbered, with new ids; copies from a file or a failing record are `untrusted`. Only the copies' own `seq` changes: a `through_seq` inside a copied event (a `session.started` or `conversation.forked`) still counts in the source's numbering | user |
| `file.restored` | `path`, `before_sha256` (absent when the file did not exist), `after_sha256` (absent when the restore removed it), `checkpoint`, the seq of the `checkpoint.saved` whose content was put back, `by` (`user`). Recorded after the person's `action.requested`, the decision and its `observation` | user |
| `checkpoint.saved` | a file's content just before the agent changed it: `path`; `sha256`, the blob holding the content, absent when the file did not exist or none was kept; `turn`; `mode`, its permission bits; `skipped`, why no content was kept (a read deny rule, or a name that holds keys) | system |
| `plan.proposed` | `text`, the plan the agent submitted in plan mode | agent |
| `plan.decided` | `decision` (`accept` or `keep-planning`), `to_mode`; never `auto` or `bypass` | user |
| `model.fallback` | `from`, `to`, `reason`; a move to a configured fallback model *(not yet emitted)* | system |
| `hook.fired` | `extension`, `event`, `verdict` (`block`, `ask` or `annotate`; a hook never allows) | system |
| `record.repaired` | `reason`, `truncated_bytes`; recorded by the local record when it is opened for writing and its last line was left unfinished by a crash: cut off when the head does not count it (`truncated_bytes` says how much), or completed when it was whole and lost only its newline (`truncated_bytes` 0) | system |
| `config.refused` | a setting that did not take effect as written, or a refused change: `layer` (`user`, `settings`, `workspace`, `env` or `command`), `source` (the file, flag or variable), `key`, `value` asked for with credentials redacted, `decision` (`set_aside`, `ignored_untrusted`, `overridden` by the managed file, or `refused` by `/config set`), `reason`, and `principal`: `kind` (`os-user` or `agent-command`), `os_user`, `uid`, `session_owner` on a server, `agent_command` and `command_id` inside an agent's command. Recorded after `session.started`, and when `/config set` refuses | system |
| `config.narrowed` | the same fields, for a `web_search` or `web_fetch` setting a layer below the managed file narrowed, as it may: turned off, fewer results, fewer hosts | system |

**Who settled a call** is in `by` on every `action.approved` and `action.denied`:

| `by` | Meaning | Actor |
|---|---|---|
| `policy` | a rule or the mode decided; no one was asked (on the workbench, also malformed arguments at step `args`) | system |
| `reviewer` | a person was asked and answered | user |
| `user` | the person made the call at the workbench | user |
| `session-scope` | an "always allow" chosen earlier in the session let it through; `scope` names it | system |
| `headless` | nobody could be asked (`-p`, `rpc`, an SDK run without an approver, `abhed eval`, or a subagent of one of these), so the run's fixed answer applied; a refusal's reason starts `no approver:` | system |
| `system` | the harness: an unknown tool (step `unknown`); arguments that were not one object, named a key twice or in another case, or gave a tool a key it does not take where that is refused (step `args`, reason naming the key); a call that could not succeed, refused before anyone was asked (step `precheck`, reason the tool's error); or a request that ended before an answer (step `ask`, reason `interrupted before an answer`, `server shut down before an answer`, `deadline passed before an answer`, the same with `before the answer was applied` when an answer arrived as the wait ended, `no answer within 30 minutes: …` or `approval failed: …`) | system |

When a person answered, two more fields say what they did:

| Field | On | Meaning |
|---|---|---|
| `approver` | `by: reviewer`, approved or denied | who answered: the signed-in subject (the email where the identity has one) of the console or API request that answered. Absent where no one signed in, and where the approver cannot know, as at the terminal or in an editor over ACP. From the SDK it is what the embedder's approver asserts, unverified |
| `granted_scope` | `by: reviewer`, approved | the scope the person chose to always allow with this answer. Later calls it lets through are `by: session-scope` with `scope` set to it |

Records written before these fields have neither, and record a reviewer's
approval, and a workbench call the person ran, as `actor: system`.

Every `action.requested` is followed by one of the two, so a record never holds
a request with no outcome. Records written before `by` was on denials say it
only through the actor: `user` for a reviewer's refusal, `system` for policy.

**Terminal reasons** are an enum, not a string — CI exit codes and the eval harness both
depend on distinguishing them (§09):
`completed · max_turns · max_budget · policy_denied · user_interrupt · error · shutdown · retry_exhausted · stalled · deadline`

`deadline` means the run's own time limit passed (a context deadline, as the eval harness sets per task);
it used to be recorded as `user_interrupt`.

`shutdown` means the node exited while the turn was running, and it is
deliberately distinct from `user_interrupt`: nobody asked for it to stop, so it
is a turn to resume rather than a decision to respect. With `server.drain_seconds`
set, a shutdown first stops accepting turns — new requests get 503 with
`Retry-After` so a balancer moves on — and waits for the running ones, so a
rolling deploy records no `shutdown` at all unless a turn outlives the budget.
A turn stopped before the model's first reply ends as `user_interrupt`,
`shutdown` or `deadline`, like any other. Messages still queued for a turn that ends as
`shutdown` are recorded as `message.dropped` before `session.ended`; they are
not delivered after a restart. An interrupt leaves them queued for the next turn.

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
                                       the store, 409 when no node runs the session or its
                                       request ended (request_id: the action.requested event's id)
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
