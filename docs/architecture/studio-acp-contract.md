# Abhed Studio and the engine: the ACP contract

Status: 2026-10-01, `apiLevel: 1`; the engine side is implemented in 1.2.3 except where §11 says otherwise

Abhed Studio is an editor. It runs no agent code of its own: every capability
it shows is a call to the local `abhed acp` engine over the Agent Client
Protocol (ACP) on stdio. This page is the contract between the two. It lists
every method, notification and `_meta` field Studio uses, the shapes they
carry, what the engine does today, what the engine must add, and the security
rules for each.

The engine is the ground truth. Where this page and `app/acp.go` disagree
about something the engine already does, the engine is right and this page is
wrong. Where this page asks for something the engine does not do yet, it is
marked **Engine: add**. Those notes record the 1.2.2 starting point; §11
says what the engine serves now.

Studio's test double, `scripts/abhed/stub-engine/abhed-stub.mjs` in the Studio
repository, speaks this contract so Studio's views can be built and tested
before each engine addition lands. The stub is not a reference: a golden
transcript recorded from the real engine settles any disagreement.

---

## 1. Conventions

### 1.1 Protocol

- JSON-RPC 2.0, one message per line, on the engine's stdin and stdout. No
  port, no socket, no HTTP. Stderr is a log, never protocol.
- ACP protocol version 1. The engine follows the ACP schema; where it answers
  an older unstable shape as well (the `models` field and `session/set_model`),
  that is said in the row.
- **Spec first.** Where ACP has a method or update for a capability, it is
  used as specified, and Abhed's extra fields go in `_meta`.
- **Extensions.** ACP reserves method names that begin with an underscore for
  extensions. Abhed's extension methods and notifications are therefore named
  `_abhed/<area>/<verb>` on the wire. This page calls them the `abhed/*`
  family and always writes the full wire name.
- **`_meta` key.** Abhed's fields in any `_meta` object live under the key
  `zybuu.ai/abhed`, written `meta` below. Engines up to 1.2.2 also read and
  write `_meta.abhed` in two places (the `trust` field of `session/new` and
  the `workspaceTrust` field of its result). The engine accepts `abhed` on
  input for one more release and writes only `zybuu.ai/abhed` from `apiLevel`
  1. Studio reads both until then.
- **W3C trace keys** (`traceparent`, `tracestate`, `baggage`) at the root of
  `_meta` are reserved by ACP and are never used for Abhed data.

### 1.2 Shapes

Shapes are TypeScript interfaces. `?` marks an optional field; a field not
listed must be ignored by the reader. Times are RFC 3339 strings in UTC.
Hashes are lowercase hex SHA-256. Every string that came from a tool, a file,
a model or the network is **untrusted text**: Studio renders it as text, never
as HTML or markdown, and never uses it as a URL, a path to open without the
person's action, or a command.

### 1.3 Errors

| Code | Meaning |
|---|---|
| -32601 | Method not found: this engine does not have it. Studio hides the feature and says which engine version adds it. |
| -32602 | Invalid params, including an unknown session, task or terminal id. |
| -32000 | The engine refused. `message` says why in words a person can read. |
| -32001 | Refused by policy: managed configuration, workspace trust or a policy rule. `data.meta.rule` names the rule when there is one. |
| -32002 | Busy: a prompt is running and the request must wait for it to end. |
| -32003 | The record is unavailable or failed verification. |

The engine never answers a refusal with an empty success.

### 1.4 Capability advertisement

`initialize` returns, in `agentCapabilities._meta["zybuu.ai/abhed"]`:

```ts
interface AbhedAgentMeta {
  apiLevel: 1;                     // this page
  edition: "ce" | "ee";
  version: string;                 // "1.2.3"
  commit: string;
  features: string[];              // see below
  record: { store: "local" | "postgres" | "memory"; dir?: string };
  managed: boolean;                // a managed configuration is in force
}
```

`features` names every `abhed/*` area the engine serves: `sessions`, `fork`,
`events`, `record.verify`, `hawkeye`, `export`, `tasks`, `tasks.review`,
`capabilities`, `policy.explain`, `trust.inspect`, `modes`, `review`,
`checkpoints`, `terminal`, `queue`, `manual`, `doctor`, `resolve`, `index`,
`infra`, `mcp.restart`, `memory`, `team` (EE). Studio shows a view only when
its feature is listed, and otherwise says which engine version adds it. It
never probes by calling a method and reading the error.

Spec capabilities are also set truthfully: `loadSession`, and
`sessionCapabilities.list`, `.resume` and `.close` once sessions exist;
`sessionCapabilities.delete` is **never** set (see §4.1).

**Engine today:** `initialize` has no `_meta`, `loadSession: false`.
**Engine: add** the block above and the `abhed version --json` command that
prints it without starting a session, so Studio can check the engine before it
spawns it.

Studio compares `apiLevel` with the level it was built for. A mismatch is a
blocking message ("this engine does not match this Studio"), never a
best-effort fallback.

---

## 2. Rules every row obeys

These are the invariants. A row's own security notes add to them and never
relax them.

1. **The engine decides.** Studio can answer an ask the engine put to it. It
   can never lift a deny, run a tool, write a file for the agent, grant
   workspace trust, widen policy or add a provider, MCP server or extension
   over ACP. A method that would do one of these does not exist, and the
   engine refuses unknown fields that would.
2. **Only a person's click answers.** An answer to `session/request_permission`
   names one of the option ids the engine offered for that request, and only
   after a person clicked it. Anything else (a timeout, an auto-approve
   setting, an OS notification, an unreadable answer) is the offered reject
   option or `cancelled`. The engine refuses an option id it did not offer.
3. **Destructive and widening steps need a modal whose default is No.**
   Destructive calls get a second confirm in Studio. Trust grants and settings
   that widen the harness are never done over ACP: Studio's main process shows
   a native dialog and then runs the engine's own command line
   (`abhed trust grant -sha256 …`, `abhed config set …`).
4. **No secret value passes through Studio.** Secrets are typed into the
   engine's own hidden prompt in an Abhed terminal. Studio sees names only.
   Responses carry names, hashes and redacted text.
5. **Everything is recorded.** Every agent action, every answer, and every
   action a person takes through Studio's agent surfaces is an event in the
   session's hash-chained record, attributed `by: "user"`, `"agent"` or
   `"system"`. Reads (lists, explains, reports) are not recorded.
6. **The agent cannot reach the IDE.** The engine write-protects Studio's
   settings, extensions, key bindings, its own binary, `.vscode/**`,
   `.git/config`, `.git/hooks/**`, `*.code-workspace` and `.devcontainer/**`
   (§4.21). The record directory is readable and writable by neither the agent
   nor Studio's renderer except through these methods.
7. **No network from Studio** except what the person turns on. The engine's
   own traffic is the engine's, under its policy.
8. **Scoping.** Every method that takes a `sessionId` acts only on sessions of
   the engine's user (tenant) and, for `session/list`, only on records the
   engine may read. An id from another tenant is "unknown session".

---

## 3. Conversation

### 3.1 Chat, streaming and thinking

| | |
|---|---|
| Wire | Spec. `session/prompt`; updates `agent_message_chunk`, `agent_thought_chunk`, `tool_call`, `tool_call_update`, `plan`, `usage_update`. |
| Engine today | Forwards `agent.delta` as message chunks and the whole `agent.reasoning` as one thought chunk. `agent.reasoning.delta` is dropped. |
| Engine: add | Forward `agent.reasoning.delta` as `agent_thought_chunk` and skip the whole-text `agent.reasoning` when deltas were sent for that turn. Stop reasons from the loop's terminal reason instead of matching error text: `completed`→`end_turn`, `max_turns`/`wake_limit`→`max_turn_requests`, `user_interrupt`/`shutdown`→`cancelled`, `max_budget`→`max_tokens` with `meta.reason: "budget"`, anything else→`refusal` with `meta.reason` set to the terminal reason. |
| Security | Message and thought text is untrusted and rendered without HTML. Reasoning is display-only; the engine never feeds it back as history. |

### 3.2 Models and thinking level

| | |
|---|---|
| Wire | Spec config options. `session/new` returns `configOptions` with one `select` option of `category: "model"`, id `model`, whose values are configured provider **names**. `session/set_config_option {sessionId, configId: "model", value}` switches and returns every option. The older `models` field and `session/set_model` are answered too; that reply puts the new current model in `meta.currentModelId`. |
| Engine today | Done (`app/acp.go`). A switch while a prompt runs is refused with -32000. |
| Engine: add | A second option, id `thought_level`, `category: "thought_level"`, values `off`, `low`, `medium`, `high`, offered only for models whose provider reports reasoning control. `config_option_update` when the engine itself changes an option (a model fallback, recorded `model.fallback`). |
| Per-subagent model | `subagent.spawned` and the task list carry `provider` and `model` (already recorded). |
| Security | The value is a configured name looked up in the engine's configuration. Nothing from Studio is used as an endpoint, key or model id. Descriptions never include the base URL, the key or the key's variable name. |

### 3.3 Slash commands

`available_commands_update` (spec), sent after `session/new` and
`session/load`, and again when skills or custom commands change:

```ts
{ sessionUpdate: "available_commands_update",
  availableCommands: { name: string; description: string; input?: { hint: string } }[] }
```

Each entry carries `meta = { source: "builtin"|"user"|"workspace"|"managed"|"mcp"|"skill", sha256?: string }`.
Built-in names win over every other source. A command is a prompt the engine
expands; running one is recorded `command.invoked` with its source and hash.

**Engine: add** the update and the list: `/compact /fork /undo /diff
/hawkeye /tasks /memory /mode /model /clear /export`, plus custom commands and
skills. **Security:** workspace commands are listed only when the workspace is
trusted, since a command is instructions.

### 3.4 Usage and cost

`usage_update` (spec) `{used, size}` per model call. **Engine: add**
`cost: {amount, currency}` only when a price table is configured, and
`meta = {tokensIn, tokensOut, tokensCached, sessionTotalIn, sessionTotalOut}`.
No invented prices.

### 3.5 Images

`promptCapabilities.image` stays `false` while the engine's messages are text
only. Studio never offers an image attachment while it is false.

---

## 4. Sessions and the record

The durable record is the engine's hash-chained local store: one JSONL file
per session under `~/.abhed/records/<tenant>/`, an append-only index, and a
head file, each line carrying `seq`, `prev` and `hash`. The CLI and Studio
share it. That store and its event types belong to the CLI's record work;
Studio consumes them through the methods below and builds neither.

### 4.1 List, load, resume, close

| Method | Shape |
|---|---|
| `session/list` (spec) | `{cwd?, cursor?}` → `{sessions: SessionInfo[], nextCursor?}` |
| `session/load` (spec) | `{sessionId, cwd, mcpServers: []}` → replays the session as `session/update` notifications, then replies `{configOptions, modes, _meta}` |
| `session/resume` (spec) | same params → no replay; replies as `session/load` |
| `session/close` (spec) | `{sessionId}` → `{}`: ends the engine's hold on the session; the record stays |
| `_abhed/session/rename` | `{sessionId, name}` → `{}`, recorded `session.named` |

```ts
interface SessionInfo {          // spec fields
  sessionId: string; cwd: string; title?: string; updatedAt?: string;
  _meta?: { "zybuu.ai/abhed": SessionMeta };
}
interface SessionMeta {
  name?: string;                 // set by rename
  created: string;
  status: "live" | "idle" | "ended";
  endReason?: string;            // terminal reason when ended
  parent?: string; forkSeq?: number;   // a fork's origin
  model?: string; mode?: string;
  gitBranch?: string;
  events: number;
  head: { seq: number; hash: string };
  background: number;            // background tasks still running
  waitingAsks: number;           // held asks across the tree
  openElsewhere?: boolean;       // another Abhed process holds it
}
```

- **Replay.** `session/load` sends the recorded conversation as the same
  updates a live run would, with the recorded tool call ids and final
  statuses, and each update's `meta.seq`. It **never re-runs a tool**. Then it
  restores the model, the mode (within today's ceiling), the approval scopes
  recorded as `approval.scope_granted` (only those today's policy still
  allows), the budget and the todo list.
- **One writer.** A session held by another Abhed process (the CLI) is
  refused with -32000 "open in another Abhed process"; Studio offers to fork
  it instead.
- **Verification before trust.** `session/load` and `session/resume` verify
  the chain first. The reply's `meta.record = {verified: boolean, head,
  firstBad?}`. Studio shows "unverified" and asks before continuing a session
  whose chain fails.
- **No deletion.** Records are kept until the person prunes them with
  `abhed record prune` in a terminal, which asks for confirmation and writes a
  tombstone. `session/delete` is not advertised and answers -32601. Studio has
  no delete action.
- **Title.** The first prompt, redacted, cut to 80 characters.

**Engine today:** none of these; sessions are memory-only.
**Engine: add** the whole row once the local store exists; `session/list`
reads the index, `session/load` reads the record.

### 4.2 Fork

`_abhed/session/fork {sessionId, throughSeq?}` → `{sessionId, parent,
forkSeq}`. With no `throughSeq` it forks at the head. The new session is
recorded `conversation.forked` in the child and `session.branched` in the
parent. Logins and ssh hosts do not carry over. Refused with -32002 while a
prompt or a wake runs in the source session (the SDK's `ErrForkDuringRun`).
The fork is returned open on this connection, as a `session/new` session is,
so its first prompt needs no `session/load`. Studio opens it in a new chat.

### 4.3 Compact and clear

- `_abhed/session/compact {sessionId, focus?}` → `{before, after}` tokens;
  recorded `compaction.started` and `compaction.completed`.
- `/clear` is Studio's action: `session/close`, then `session/new`. Nothing is
  deleted.

### 4.4 The event stream

```ts
// request
_abhed/events/subscribe { sessionId, afterSeq?: number }  → { head: {seq, hash}, subscription: string }
_abhed/events/unsubscribe { subscription }                 → {}
_abhed/events/page { sessionId, afterSeq?: number, limit?: number (≤ 500), types?: string[] }
                                                           → { events: RecordEvent[], head, more: boolean }
// notification, once per event after afterSeq, in seq order, including a backlog
_abhed/event { subscription, sessionId, event: RecordEvent }

interface RecordEvent {
  seq: number; id: string; ts: string;
  type: string;                         // §8
  actor: "user" | "agent" | "system" | "tool";
  trust: "trusted" | "untrusted";
  session: string;                      // the child's id for a mirrored subagent event
  payload: unknown;                     // as recorded: already redacted
  prev: string; hash: string;
}
```

- The stream is the record, not a second copy: `payload` is exactly what was
  written, after redaction; withheld values stay withheld.
- Delivery is at least once in seq order. Studio drops a seq it has seen.
- A subscription ends with its session's process or `unsubscribe`. At most
  eight per session; the ninth is refused.
- `types` filters a page only. Streams are unfiltered, so a view never infers
  "nothing happened" from a filter.

**Engine today:** only nine event types reach Studio, as session updates.
**Engine: add** the three methods and the notification over the local store.
**Security:** payloads marked untrusted are shown as data. The stream is
read-only.

### 4.5 Verify and export

- `_abhed/record/verify {sessionId}` → `{ok: boolean, events, head, firstBad?:
  {seq, reason}, store}`. It recomputes every line's hash and the head.
- `_abhed/export {sessionId, format: "jsonl" | "html"}` → `{path, bytes,
  sha256, head}`. The engine writes the file under `~/.abhed/exports/`, never
  in the workspace, and returns its path; Studio then offers a save dialog and
  copies the file. The JSONL keeps the chain, so `abhed record verify <file>`
  works offline. HTML is the HawkEYE report.
- **Honesty:** the chain is evidence against the agent and against accidental
  or partial edits. It is not proof against the owner of the machine, who can
  rewrite and re-chain. Studio's wording says "verified chain", never
  "tamper-proof".

### 4.6 HawkEYE

`_abhed/hawkeye {sessionId, format: "json" | "html"}` → for `json`, the
report the engine's `hawkeye` package builds over the durable record:

```ts
interface HawkeyeReport {
  session_id: string; started: string; ended: string; prompt: string;
  outcome: string;                         // terminal reason, or "running"
  models?: string[];
  totals: { events, turns, tool_calls, tokens_in, tokens_out, tokens_cached,
            cache_hit_rate, model_ms, tool_ms, duration_ms, peak_context,
            context_window, untrusted_observations, recalls };
  turns: Turn[]; calls: Call[];
  policy: { allowed, denied, asked_reviewer, by_step: Record<string, number> };
  subagents?: Subagent[]; files?: { path, reads, writes }[];
  findings: { severity: "info" | "warn" | "critical"; code; title; detail; seq? }[];
  integrity: { first_seq, last_seq, gaps?: number[], ordered, has_end };
}
```

For `html`, `{html: string}`: the same page `abhed hawkeye` renders.

- Studio refreshes the report when the event stream shows a turn ended, no
  more than once every two seconds.
- Findings go to Studio's Problems view with the finding's `seq` as the link.
- **Security:** the HTML report is shown only in a webview with no Node, no
  scripts and CSP `default-src 'none'; style-src 'unsafe-inline'`. The JSON is
  rendered as text.

**Engine today:** the report exists for the server; not over ACP.
**Engine: add** the method over the local record.

### 4.7 Retention

Records are kept until pruned. `abhed record prune --older-than <age>` runs in
a terminal, asks for confirmation and writes a tombstone. A managed
`record.retention_days` may enforce pruning. There is no pruning method over
ACP.

---

## 5. Control: modes, approvals, policy, trust

### 5.1 Modes

Spec session modes:

```ts
// session/new, session/load and session/resume results
modes: { currentModeId: string,
         availableModes: { id: "default" | "accept-edits" | "plan" | "auto" | "bypass";
                           name: string; description: string;
                           _meta?: { "zybuu.ai/abhed": { locked?: boolean; lockedBy?: "managed"; widens: boolean } } }[] }
session/set_mode { sessionId, modeId } → {}
update { sessionUpdate: "current_mode_update", currentModeId }
```

The same list is also a config option of `category: "mode"`.

- `availableModes` is what the engine allows *now*: the managed ceiling and
  the user's own mode list applied. `bypass` is present only when the user's
  configuration offers it and no managed policy forbids it.
- A mode outside `availableModes` is refused with -32001.
- The change is recorded `mode.changed {from, to, by: "user", via: "studio"}`,
  and the engine sends `current_mode_update`.
- Plan mode's exit is the engine's `plan.proposed` / `plan.decided` flow, shown
  as a normal ask.

**Engine today:** `session/set_mode` answers -32601; the mode is fixed at
construction. **Engine: add** a loop-level mode change through the same
configuration path the CLI uses, so managed ceilings hold.
**Security:** Studio needs a modal (default Cancel) before it sends `auto` or
`bypass`, and shows a red status bar entry while bypass is on. The engine does
not trust that modal: it enforces the ceiling itself.

### 5.2 Approvals

Spec `session/request_permission`, engine→client, exactly as `app/acp.go`
sends it:

```ts
{ sessionId,
  toolCall: { toolCallId, title, kind, status: "pending", rawInput,
              _meta: { "zybuu.ai/abhed": AskMeta } },
  options: [ { optionId: "once:<bind>",   name: "Allow once",          kind: "allow_once" },
             { optionId: "always:<bind>", name: "Always allow <scope>", kind: "allow_always" },  // only when offered
             { optionId: "reject:<bind>", name: "Deny",                 kind: "reject_once" } ] }

interface AskMeta {
  tool: string; step: string; reason: string; destructive: boolean;
  requestId?: string;        // the engine's action.requested id; <bind> is it
  scope?: string;            // what "always" grants, only when offered
  subagent?: string;         // a subagent's ask; toolCallId is "subagent-<requestId>"
  rule?: string;             // Engine: add. The rule that asked, or "builtin:<name>"
  via?: string;              // Engine: add. "pipeline <name> step <n>"
  held?: boolean;            // Engine: add. Released from a background task's hold (§6.3)
  taskId?: string;           // Engine: add. The background task asking
  diff?: { path; oldText?; newText }[]; // Engine: add. For edit and write (§5.5)
}
```

- Option ids are bound to the request, so an answer meant for another ask is
  refused.
- "Always" is never offered for destructive, screen or ask-rule steps.
- An answer of "always" is recorded `approval.scope_granted {scope, by:
  "user"}` (**Engine: add**) so `session/load` can restore it.
- A call whose input the record withheld is refused without asking, with a
  message chunk that says so (done).
- An ask made while no prompt turn is open is held (§6.3).
- Studio's card shows the "why" line from `step`, `rule` and `reason`.

### 5.3 Destructive confirm

`meta.destructive: true` (done). Studio asks a second time in its own modal
(default Deny, the full command and reason shown) before sending an allow
option. No chat part or extension can answer that modal. Studio's own
tighten-only check also marks a command destructive when an older engine did
not say.

### 5.4 Policy view and explain

```ts
_abhed/capabilities { sessionId } → Capabilities   // §6.4; includes .policy
_abhed/policy/explain { sessionId, tool: string, args: unknown }
  → { decision: "allow" | "ask" | "deny"; step: string; rule?: string; reason: string; scope?: string }

interface PolicyView {
  mode: string; sandbox: { tier: string; network: boolean; backend?: string };
  managed: boolean;
  rules: { decision: "deny" | "ask" | "allow"; rule: string;
           layer: "managed" | "user" | "workspace" | "builtin";
           applied: boolean; ignoredBecause?: "workspace-untrusted" | "managed-override" }[];
}
```

- `explain` is a dry run: not recorded, not cached, and it never changes the
  per-session "always" scopes. Values shown are redacted.
- **Engine: add** `Result.Rule` in the policy engine, the rule on `action.*`
  events and on asks, the layered rule list and `explain`.
- Editing the user's rules is not an ACP method: Studio's main process runs
  `abhed config set --scope user …` and asks natively for widening keys.

### 5.5 Diffs on asks

**Engine: add.** For `edit` and `write`, the engine computes the diff before it
asks and puts it in the ask's `content` as spec `{type: "diff", path, oldText,
newText}` and in `locations`. For files over 256 KiB only the changed hunks
are sent, with `meta.hunksOnly: true`.

### 5.6 Workspace trust

| | |
|---|---|
| Wire today | `session/new` accepts `_meta.abhed.trust: "untrusted"` only (tighten-only; anything else is -32602) and returns `_meta.abhed.workspaceTrust = {workspace, file?, sha256?, trusted, reason, applied[], ignored[{key, value?, reason?}]}`. |
| Engine: add | Move both to the `zybuu.ai/abhed` key. `_abhed/trust/inspect {cwd}` → the same object plus `agents[{name, sha256}]`, `skills[{name, dir}]`, `commands[]` and `mcp[]` the file would bring. Notification `_abhed/trust/changed {cwd, oldSha256, newSha256}` when the file's bytes change during a session; the next prompt restarts the session under the new decision. |
| Studio | A banner, then a review editor built from `inspect`, then **Trust this exact file**, which asks in a native dialog raised by Studio's main process (path and SHA-256 shown) and then runs `abhed trust grant -sha256 <H> <dir>` with the bundled, hash-checked engine binary. |
| Security | There is no grant method, now or later. A grant is pinned to the reviewed hash, so a file changed after review is not trusted. Studio's own Restricted Mode sends `trust: "untrusted"`. |

---

## 6. Orchestration

### 6.1 Subagents in the foreground

The `task` tool's spawn and return reach Studio two ways:

- **As cards.** **Engine: add** a `tool_call` with `toolCallId: "sub-<child
  session>"`, `kind: "think"`, title `subagent <type>: <description>`, and
  `meta = {subagent: {session, agentType, depth, model, provider?, branch?,
  definitionSha256?}}` when `subagent.spawned` is recorded; a
  `tool_call_update` with the summary length, turns and tokens on
  `subagent.returned`.
- **As events.** `subagent.spawned`, `subagent.returned`, `subagent.ask`,
  `subagent.action` and `subagent.notice` on the event stream (§4.4).

A subagent's asks already arrive as cards with `meta.subagent` and
`toolCallId: "subagent-<requestId>"` (done). One ask at a time across the
whole tree.

### 6.2 Background tasks

A background task is a subagent started with `background: true`. ACP clamps
the wake mode to `notify`: a result is delivered to the conversation, and the
agent never runs on its own because a task finished.

**Today (done in `app/acp.go`):**
- `subagent.spawned` with `background: true` opens a card
  `toolCallId: "bg-<taskId>"`, `status: "in_progress"`, `meta.taskId`.
- `subagent.notice` completes that card (`completed`, or `failed` for any
  other status) with the result text, whether or not a turn is open.
- `session/cancel` cancels the running prompt **and every background task**,
  with or without a prompt open ("Stop means stop").

**Engine: add:**

```ts
_abhed/tasks/list   { sessionId } → { tasks: TaskInfo[] }
_abhed/tasks/cancel { sessionId, taskId } → { cancelled: boolean }
_abhed/tasks/resume { sessionId, taskId, prompt: string } → { taskId }   // a finished task, continued
_abhed/tasks/review { sessionId, taskId? } → { released: number }        // §6.3
// notification whenever a task's status, turns or waiting asks change
_abhed/tasks/changed { sessionId, task: TaskInfo }

interface TaskInfo {                 // the SDK's TaskInfo, plus
  task_id: string; description: string; agent_type?: string;
  provider?: string; model?: string;
  status: "running" | "completed" | "failed" | "cancelled" | string;  // or the cap that ended it
  reason?: string; turns?: number; started: string; summary?: string;
  tokens_in?: number; tokens_out?: number;
  waiting_asks: number;              // asks held for a person
  notice?: { delivery: "boundary" | "idle" | "wake"; content_chars: number };
  branch?: string;                   // worktree isolation
}
```

- `cancel` is recorded as the person's stop (`user_interrupt`).
- `resume` is the `task` tool's `resume` argument used by a person. It is a
  prompt, so it waits for the session to be idle (-32002 otherwise) and is
  recorded as a `user.message` to that child.
- **Security:** no auto-approval while idle; stopping the chat stops every
  task; a task never outlives `session/close` (its end is recorded
  `session_closed`).

### 6.3 Held asks and reviewing them

An ask from a background task while no prompt turn is open cannot be put to
the editor at once. Today (done) the engine marks the task's card with a
`tool_call_update` "Waiting for your approval; send a message to review it",
holds the ask until the next prompt opens (up to 30 minutes, then it is
refused and recorded `by: system`), and then sends the ordinary
`session/request_permission`.

**Engine: add** `_abhed/tasks/review {sessionId, taskId?}`, a person's
request to see the held asks now, without sending a prompt:

1. The engine opens a *review window* for the session and sends each held ask
   (of `taskId`, or all) as a normal `session/request_permission`, with
   `meta.held: true` and `meta.taskId`. It replies `{released: n}`.
2. The window stays open while asks are outstanding and for 5 minutes after
   the last answer; an ask a task makes during the window goes out at once
   with `held: true`.
3. Answers follow §5.2 exactly: bound option ids, a person's click only, the
   destructive modal, "always" only when offered.
4. A window closes on `session/cancel`, on `session/close`, or when a prompt
   opens (asks then go out inside the turn as today).

Studio's engine connection routes a permission request with `meta.held:
true` to its Tasks view, never to a chat turn, and answers `cancelled` if no
Tasks view is showing it. It never answers a held ask by itself.

### 6.4 Capabilities: agents, skills, MCP, extensions and the rest

`_abhed/capabilities {sessionId}` returns what this session can reach. It is a
superset of the server's `/v1/capabilities`, built by the same code.

```ts
interface Capabilities {
  model: { name: string; context_window: number };
  policy: PolicyView;                                        // §5.4
  tools: { name; description; mutates: boolean; source: "builtin" | "mcp" | "skill"; server?: string }[];
  agents: { name: string; source: "builtin" | "managed" | "user" | "workspace";
            sha256?: string; model?: string; tools: string[]; description?: string;
            trusted: boolean; refused?: string; path?: string }[];
  skills: { name; description; source: "user" | "workspace" | "managed";
            has_pipeline: boolean; allowed_tools?: string[]; dir?: string;
            trusted: boolean; refused?: string }[];
  mcp: { name: string; transport: "stdio" | "http" | "sse";
         status: "connected" | "starting" | "stopped" | "error"; error?: string;
         tools: string[]; source: "user" | "workspace" | "managed";
         pinned: boolean }[];                               // digest-pinned command or image
  extensions: { name: string; events: string[];            // Abhed hook extensions
                status: "running" | "stopped" | "not started"; error?: string; source: string }[];
  web: { search: boolean; fetch: boolean; allowed_hosts: string[]; ask: boolean };
  infra: { clusters: { name; context; logged_in: boolean }[]; ssh_hosts: string[] };
  secrets: { name: string }[];                               // names only
  index: { enabled: boolean; files?: number; chunks?: number; updated?: string; embedder?: string };
  rag: { name: string; docs: number }[];
  memory: { path: string; scope: "user" | "workspace" | "managed" | "auto"; bytes: number; loaded: boolean }[];
  memory_auto: boolean;                                      // opt-in, off by default
  studio: { host_terminal: boolean };                        // managed may set false
}
```

- **Refreshing.** Studio refetches when the event stream shows
  `mcp.status`, `extension.status`, `hook.fired`, `memory.loaded`,
  `memory.written` or a trust change, and when the person asks.
- `_abhed/mcp/restart {sessionId, name}` → `{status}`, recorded `by: user`.
- `_abhed/index/status {sessionId}` and `_abhed/index/rebuild {sessionId}`
  (recorded `by: user`; progress as events).
- `_abhed/infra/status {sessionId}` is `infra` alone, for polling.
- **Client MCP servers are refused.** `session/new` must be sent
  `mcpServers: []`; a non-empty list is not used and the reply's
  `meta.mcpServersRefused` names them. MCP servers are configured in the
  engine's configuration, under workspace trust.
- **Security:** read-only, redacted. Untrusted workspace agents, skills,
  commands and MCP servers are listed with `trusted: false` and `refused`, so
  Studio can say why they are missing. Abhed hook extensions can veto, never
  permit. Secret values never appear.

**Engine today:** none over ACP; the server has models, sandbox, permissions,
tools, skills, MCP and extensions. **Engine: add** the method and every field
above that the server lacks.

### 6.5 Pipelines

A pipeline step is a normal tool call. **Engine: add** `meta.via:
"pipeline <name> step <n>"` on its `tool_call` and on its ask. Each step goes
through policy; a step needing approval in a headless run is refused.

---

## 7. The person's own actions

### 7.1 The Abhed terminal (sandboxed)

```ts
_abhed/terminal/create { sessionId, mode: "lines" | "interactive", cols, rows }
  → { terminalId, tier: string, network: boolean, recorded: true }
_abhed/terminal/input  { terminalId, data: string }     → {}
_abhed/terminal/resize { terminalId, cols, rows }       → {}
_abhed/terminal/kill   { terminalId }                   → {}
// notifications
_abhed/terminal/output { terminalId, data: string }     // base64 of the bytes
_abhed/terminal/exit   { terminalId, code?: number, signal?: string }
// engine → client request: the destructive y/N
_abhed/terminal/confirm { terminalId, command, reason } → { confirmed: boolean }
```

- The shell runs in the engine's sandbox tier for the session. In `lines`
  mode each line is a `bash` call through policy; in `interactive` mode the
  shell runs whole under the sandbox and each entered line is recorded
  `terminal.input {line | withheld, by: "user"}`.
- A line the terminal did not echo (`read -s`, password prompts) is recorded
  as withheld, never as text.
- `confirm` is answered only from a modal whose default is No.
- Studio's ACP `terminal` client capability stays `false`: the agent never
  runs commands in Studio's terminals.

**Engine: add** all of it, by moving the server's pty and line capture into a
shared package so the server and ACP use the same code.

### 7.2 The host terminal

Studio also offers its own host shell, labelled **"not sandboxed, not
recorded"** in its name, its banner and its tab. It is not an engine feature
and has no ACP method: the engine cannot sandbox or honestly record a shell it
does not run. A managed policy removes it: when `capabilities.studio.
host_terminal` is `false`, Studio does not offer the profile and refuses to
open one. The sandboxed Abhed terminal is the default profile.

### 7.3 Manual edits and dirty buffers

- `_abhed/manual/edited {sessionId, path, beforeSha256, afterSha256, patch}`
  → `{}`, recorded `manual.edit {by: "user"}`. Record-only: a person's save on
  their own machine is not policy-gated.
- `_abhed/buffers/dirty {sessionId, paths: string[]}` → `{}`: the files with
  unsaved changes in Studio. The engine refuses an agent edit to one of them
  ("the person has unsaved changes to this file").
- **Security:** paths are canonicalised by the engine and must lie in the
  session's workspace or added directories; others are dropped.

### 7.4 Per-hunk review

```ts
_abhed/review/list     { sessionId } → { files: { path, status: "modified" | "added" | "deleted", hunks: number }[] }
_abhed/review/baseline { sessionId, path } → { text: string, sha256 }
_abhed/review/accept   { sessionId, path, hunk?: number } → { remaining: number }
_abhed/review/reject   { sessionId, path, hunk?: number } → { remaining: number }
// notification
_abhed/review/changed  { sessionId, paths: string[] }
```

- `accept` records `change.accepted {path, hunk?, by: "user"}` and moves the
  baseline.
- `reject` writes the baseline back through the engine's manual path, as a
  policy-checked write recorded `by: "user"`: a protected path stays
  protected.
- Studio's renderer never writes a file. There is no auto-accept delay.

### 7.5 Checkpoints and undo

`_abhed/review/undoTurn {sessionId, turn}` → `{restored: {path, beforeSha256,
afterSha256}[]}`. Each file is restored through the engine's manual path and
recorded `file.restored {by: "user", checkpoint}`. Pre-images live in the
record's content-addressed blob store, so undo works after a restart. A
conversation rewind is `_abhed/session/fork` at the turn's seq.

### 7.6 Queue, steer and stop

- `_abhed/session/steer {sessionId, text}` → `{}`: "send now" while a prompt
  runs; recorded as a `user.message` with `meta.steered: true`.
- `_abhed/queue/list {sessionId}` → `{items: {id, text, queued}[]}` and
  `_abhed/queue/cancel {sessionId, id}` → `{}`.
- Stop is spec `session/cancel`, which also cancels background tasks (done).

### 7.7 Resolve an issue

`_abhed/resolve {cwd, issueUrl}` → `{sessionId}` starts a session running the
resolve flow under policy. Pushing and opening a pull request are asks. The
forge token comes from the secrets vault by name. **Engine: add.**

---

## 8. Health, setup and memory

### 8.1 Doctor

`_abhed/doctor {cwd?}` → `{checks: {id, title, status: "ok" | "warn" | "fail",
detail}[]}`, the same checks as `abhed doctor --json`: configuration, trust,
provider reachability (loopback only unless configured), sandbox tier,
record store and verification, MCP, index, managed policy. Output is
redacted. **Engine: add** both.

### 8.2 Setup

Setup is not an ACP method: it runs before a session can exist. Studio's main
process runs `abhed setup --json …` (writes `~/.abhed/config.json`
atomically, validates, runs doctor) and `abhed secret set NAME` in an Abhed
terminal for a hosted provider's key. Providers name keys by
`api_key_secret: NAME`, resolved from the vault, because an app launched from
the Dock has no shell environment. **Engine: add** `setup --json`,
`api_key_secret` and `config get|set --scope user --json`, which classifies
each key as tightening or widening.

### 8.3 Memory files and auto memory

- `capabilities.memory` lists the loaded memory files.
- **Auto memory is opt-in.** `memory_auto` is `false` unless the user's
  configuration turns it on; a managed policy can force it off, and a
  workspace can only turn it off. While it is off the engine writes no memory
  file of its own.
- A person's edit to a memory file in Studio is a manual edit (§7.3). The
  agent's writes to memory are policy-checked tool calls, recorded
  `memory.written {path, kind, by}`.

---

## 9. Enterprise: the team server (EE only)

A CE engine answers every method here with -32601, and CE Studio shows
nothing about team servers.

```ts
_abhed/team/status {} → { connected: boolean; server?: string; issuer?: string;
                          subject?: string; expires?: string; policy?: { sha256, version } }
_abhed/team/connect { server: string } → { started: true }
_abhed/team/disconnect {} → {}
// notification
_abhed/team/changed { connected: boolean; reason?: string }
```

- **Organisation identity provider only.** The engine reads the issuer from
  the team server's own discovery document and refuses consumer and platform
  identity providers (Microsoft consumer and GitHub sign-in among them); the
  allowed issuers come from the managed configuration. Studio never supplies
  an issuer.
- **The engine does the sign-in.** OAuth 2.0 authorization code with PKCE,
  in the person's system browser, redirected to a loopback port the engine
  opens for that one exchange. Tokens are stored in the engine's secrets
  vault and never pass through Studio. Studio shows only `status`.
- `server` is checked against the managed configuration's allowed team
  servers; an unlisted server is refused with -32001, so a renderer cannot
  point the engine at a server of its choosing.
- Managed policy distributed by the team server applies as managed
  configuration (tighten and lock). Schedules, the offline gallery and
  evidence packs are the team server's and are not ACP methods.

---

## 10. Event types Studio reads

Studio's Events and HawkEYE views read these types from the stream. Types
marked *new* are added by the CLI's shared interface work; Studio only reads
them.

| Type | Actor | Payload (main fields) |
|---|---|---|
| `session.started`, `session.ended` | system | reason on end |
| `user.message` | user | text; `steered?` |
| `agent.message`, `agent.delta` | agent | text |
| `agent.reasoning`, `agent.reasoning.delta` | agent | text |
| `action.requested` | agent | call_id, tool, args, subject, `rule?` |
| `action.approved`, `action.denied` | system/user | call_id, by, step, reason, scope, `rule?` |
| `observation` | tool (untrusted) | call_id, tool, content, is_error, exit_code |
| `subagent.spawned`, `subagent.returned` | agent | session, description, agent_type, depth, model, provider, background, task_id, branch |
| `subagent.ask`, `subagent.action` | system | request_id, tool, decision, reason |
| `subagent.notice` | system | task_id, status, content, delivery, wake |
| `session.woken`, `session.wake_set` | system | |
| `compaction.started`, `compaction.completed` | system | before, after |
| `context.offloaded` | system | results, before, after |
| `plan.updated`, `todo.updated` | agent | items |
| `model.call`, `model.switched` | system | tokens, latency, model |
| `change.accepted` | user | path, hunk |
| `conversation.forked` | user | from, through_seq |
| `terminal.input` | user | line or withheld |
| `monitor.verdict` | system | verdict, before, after (shown when present) |
| `message.dropped` | system | |
| *new* `mode.changed` | user | from, to, via |
| *new* `permission.changed` | user | op, list, rule, scope |
| *new* `workspace.dir_added` | user | path, access |
| *new* `command.invoked` | user | name, source, sha256 |
| *new* `memory.loaded`, `memory.written` | system/agent | files / path, kind |
| *new* `session.named`, `session.branched` | user | name / from, through_seq |
| *new* `file.restored` | user | path, before_sha256, after_sha256, checkpoint |
| *new* `plan.proposed`, `plan.decided` | agent/user | text / decision |
| *new* `model.fallback` | system | from, to, reason |
| *new* `hook.fired` | system | extension, event, verdict |
| *new* `record.repaired` | system | reason, truncated_bytes |
| Studio's additions: `approval.scope_granted`, `manual.edit`, `trust.changed`, `mcp.status`, `extension.status`, `task.cancelled`, `team.connected` | as named | as in the sections above |

An unknown type is shown with its raw payload as text. Studio never infers
meaning from a type it does not know.

---

## 11. The whole surface at a glance

"Engine 1.2.3" is what the engine serves. Where it differs from a section
above, the engine is right (§ intro) and the difference is listed after the
table.

| Capability | Wire | Kind | Engine 1.2.3 |
|---|---|---|---|
| Handshake, features | `initialize` + `meta`, `abhed version --json` | spec + meta | done |
| Chat, thinking, plan, usage | `session/prompt`, `session/update` | spec | done: reasoning deltas, stop reasons from the terminal reason, usage `meta`; no `cost` (no price table) |
| Models | `session/set_config_option` (`model`) | spec | done; `config_option_update` on `model.fallback` |
| Thinking level | config option `thought_level` | spec | not yet: never offered, as no provider reports reasoning control |
| Slash commands | `available_commands_update` | spec | done (see note 3) |
| Modes | `session/set_mode`, `current_mode_update`, config option `mode` | spec | done |
| Approvals | `session/request_permission` | spec + meta | done: `rule`, `via`, `diff`, `held`, `taskId`; `approval.scope_granted` |
| Policy view, explain | `_abhed/capabilities`, `_abhed/policy/explain` | ext | done |
| Workspace trust | `session/new` meta, `_abhed/trust/inspect`, `_abhed/trust/changed` | meta + ext | done (see note 6) |
| Sessions | `session/list`, `session/load`, `session/resume`, `session/close` | spec | done (local record only) |
| Rename, fork, compact | `_abhed/session/rename`, `/fork`, `/compact` | ext | done; `compact` refuses a `focus` |
| Event stream | `_abhed/events/subscribe`, `/page`, `/unsubscribe`, `_abhed/event` | ext | done |
| Verify, export | `_abhed/record/verify`, `_abhed/export` | ext | done |
| HawkEYE | `_abhed/hawkeye` | ext | done |
| Subagents | `tool_call` cards + events | spec + meta | done |
| Background tasks | `bg-<id>` cards, `_abhed/tasks/*`, `_abhed/tasks/changed` | spec + ext | list, cancel, review done; `resume` not yet |
| Agents, skills, MCP, extensions, web, infra, secrets, index, RAG, memory | `_abhed/capabilities` | ext | done (see note 7) |
| MCP restart, index, infra polling | `_abhed/mcp/restart`, `_abhed/index/*`, `_abhed/infra/status` | ext | not yet (-32601; `mcp.restart`, `index`, `infra` not in `features`) |
| Abhed terminal | `_abhed/terminal/*` | ext | done, both modes (see note 4) |
| Host terminal | none (Studio only), removable by managed `studio.disable_host_terminal` | — | Studio; the managed key is read into `capabilities.studio` |
| Manual edits, dirty buffers | `_abhed/manual/edited`, `_abhed/buffers/dirty` | ext | done |
| Per-hunk review, undo | `_abhed/review/*` | ext | done |
| Queue, steer, stop | `_abhed/session/steer`, `_abhed/queue/*`, `session/cancel` | ext + spec | done |
| Resolve | `_abhed/resolve` | ext | not yet (-32601; `resolve` not in `features`) |
| Doctor | `_abhed/doctor`, `abhed doctor --json` | ext | done |
| Setup, config, trust grant, secrets | engine command line from Studio's main process | — | `trust grant -sha256` and `secret set` exist; `setup --json`, `api_key_secret` and `config get|set` not yet |
| Memory area | `memory` feature | ext | not yet (memory files are in `capabilities`) |
| Team server (EE) | `_abhed/team/*` | ext | CE answers -32601 |
| Deletion | none; `abhed record prune` in a terminal | — | — |

Where the engine differs from the sections above:

1. **Fork (§4.2).** `session.branched` is recorded in the new session, naming
   the source and the last seq taken, as the CLI's branch records it; the
   source's record is not written to, since another process may hold it.
   A fork of a record that failed verification copies it marked untrusted.
2. **Export (§4.5).** `html` is the HawkEYE page, as this page says; the
   CLI's `abhed record export` still writes the transcript page.
3. **Commands (§3.3).** A custom command that runs shell lines, narrows the
   tools or names a model is refused over ACP (the CLI runs it). Skills are
   listed as commands of source `skill`; MCP prompts are not listed.
   `/mode` from a prompt refuses `auto` and `bypass`, which need Studio's
   picker; `/fork` and `/clear` answer that they are Studio's actions.
4. **Terminal (§7.1).** `lines` mode runs each line as the person's `bash`
   call through policy, in the session's sandbox, on a tool session of its
   own; a destructive line asks `_abhed/terminal/confirm`. `interactive`
   runs one shell under the sandbox with the workbench's line capture, now
   shared in `internal/termline`: each line is put to the deny rules at its
   Enter and recorded `terminal.input`, withheld when the terminal did not
   show it. As in the workbench, it is refused where only a line-by-line
   terminal applies every rule (`sandbox.terminal: "lines"`, or a managed
   policy whose deny rules or hooks screen `bash`), and a destructive line
   there is not confirmed, since the shell, not the engine, runs it.
5. **Undo (§7.5).** `turn` is the turn number the record's
   `checkpoint.saved` events carry. An accepted hunk moves the baseline, so
   undo returns to it.
6. **Trust changes (§5.6).** The file's hash is compared at each prompt and
   each `_abhed/capabilities` call, not watched; on a change the engine sends
   `_abhed/trust/changed` and the prompt restarts the session from its record
   under the new decision.
7. **Capabilities (§6.4).** Skills report `source: "user"`; `rag.docs` is 0;
   clusters report `logged_in: false`; an MCP server is `connected` or
   `error`. `_abhed/tasks/changed` is sent on spawn, result, cancel and a
   change in waiting asks, not on each turn.
8. **Editor files (§2.6).** The file tools refuse `.vscode/**`,
   `.devcontainer/**`, `.git/config`, `.git/hooks/**` and any
   `*.code-workspace`; commands are kept from writing those paths and the
   `*.code-workspace` files that exist when the session starts. On Linux a
   path that does not exist yet cannot be held read-only by bubblewrap.
9. **Modes (§5.1).** A mode changes only between prompts and while no
   background task runs (-32002 otherwise), since the policy engine is read
   by every call.
