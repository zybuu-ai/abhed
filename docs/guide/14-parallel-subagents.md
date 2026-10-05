# Parallel subagents and worktree isolation

The `task` tool runs one subagent and waits. The `tasks` tool runs several at
once. The agent chooses it when subtasks are independent — investigate three
modules, try two fixes, review each service — and the model's own prompt
decides that, not a flag.

```json
{
  "tasks": [
    {"prompt": "…", "description": "audit pkg/auth", "agent_type": "review"},
    {"prompt": "…", "description": "audit pkg/store", "agent_type": "review"}
  ],
  "isolation": "worktree"
}
```

Up to eight per call. Concurrency is bounded by `limits.max_parallel_subagents`
(zero means all at once), and each subagent still counts against
`limits.max_subagents` and the token budget exactly as a single `task` does.
The budget (`limits.max_budget_tokens`) is one allowance for the session and
all its subagents: a subagent spends from it turn by turn and stops when it
runs out, as its parent does.

## Agent types

`agent_type` names the role each subagent runs in: `general` (the session's
tools), `explore` (reads only), `test` and `review`, plus any [agent
definitions](17-agent-definitions.md) this session loaded. The tools list the
types on offer, and a call naming any other is refused before anything runs;
`tasks` once ran such a task as `general`. A definition whose role works in its
own worktree gets one even when the call asks for no isolation, so `tasks` then
asks as worktree isolation does.

Each task may also name a `model`, a configured provider, when the
deployment offers more than one; see [Agent
definitions](17-agent-definitions.md#another-model-for-a-role). A model that
is not available refuses that call, and no other model is used instead.

## Where subagents run

`task` and `tasks` are part of the same agent on every surface (for tasks
started in the background, see [Background tasks](#background-tasks)):

| Surface | Subagents | A subagent's ask goes to |
|---|---|---|
| CLI, interactive | yes | your prompt, naming the subagent |
| CLI, `-p` | yes | nobody: refused as `headless` |
| `abhed serve`: console and workbench | yes | the person in the console, on the parent session |
| server, unattended runs (schedules) | yes | nobody: refused as `headless` |
| `abhed acp` | yes | the editor's permission dialog |
| `abhed rpc` | yes | nobody, as the rpc session has no approver: refused |
| SDK | with `Options.ConfiguredTools` | your `Approve`, or refused without one |
| `abhed eval` | yes | the eval's own approver, which approves (eval refuses to run under a managed configuration) |

Asks reach the person one at a time for the whole tree: a subagent's, a
nested subagent's and the parent's own share one queue, so the console's one
pending request, the terminal and the editor's permission dialog never hold
two at once. An ask still waiting when its run is cancelled gives up without
being shown. On a server an ask that ends leaves the session as it was
(`running` while a run is live, otherwise `done` or `idle`), and a message
sent while no run is live starts one rather than being queued as steering.

A subagent's worktrees are made under the session's workspace, and on a
server each session binds its own `task` and `tasks`, so one person's
subagents run in, and record into, that person's session only. On a
server every account shares one workspace, so those worktrees are
in the same directory tree as everyone else's.

A subagent, in a worktree or not, uses the cluster logins and connected
hosts of the session that started it, and never another session's: a
`k8s_login` in one person's console session does not reach another
person's subagents. See [Clusters and machines](../ops/infrastructure.md).

## Isolation

The interesting case is when subagents **write**. Two agents editing one
checkout overwrite each other, and the parent cannot tell whose change
survived. With `"isolation": "worktree"` each subagent gets:

- its own **git worktree** under `.abhed-worktrees/<id>` — a separate checkout
  of `HEAD`, so its files are its own;
- its own **branch**, `abhed/<id>`;
- its own **scoping boundary** for files: the child's file tools are rooted in
  the worktree and cannot reach the parent's tree or a sibling's. Its commands
  start in the worktree and run in the parent's sandbox, so, as there, they
  are confined to the workspace rather than to the worktree.

The worktrees live beside `.abhed/`, not in it: `.abhed/` is Abhed's own
state, which the agent can neither read nor write, so a worktree there could
not be worked in.

Nothing touches the main tree. When the tasks return, the parent receives each
subagent's summary and, for each worktree, what changed and how to take it:

```
## Task 1 — audit pkg/auth
<summary>

Worktree .abhed-worktrees/k3f9q2 (branch abhed/k3f9q2): 3 file(s) changed, UNCOMMITTED.
 auth/local.go | 12 ++++---
 …
To take these changes: review with `git -C .abhed-worktrees/k3f9q2 diff`, commit
there, then `git merge abhed/k3f9q2` from the main tree. To discard:
`git worktree remove --force .abhed-worktrees/k3f9q2 && git branch -D abhed/k3f9q2`.
```

Changes are left **uncommitted in the worktree**, on purpose. Merging is a
decision, and the point of isolation is that a person — or the parent agent,
with the diff in front of it — makes that decision rather than discovering it.

Worktrees are created up front and serially before any subagent starts, so a
git failure stops the whole call before a token is spent. The directory is
added to `.git/info/exclude` so the main tree's `git status` stays clean
without touching the repository's tracked `.gitignore`, and `glob`, `grep` and
the code index pass over it. A link planted at `.abhed-worktrees`, or at a
worktree's own path, is refused rather than followed.

## Requirements and limits

- `tasks` with `"isolation": "worktree"` is a mutating call, because it
  makes branches and checkouts before any subagent runs: it asks in default
  mode, plan mode refuses it, and with nobody to ask it is refused unless an
  allow rule (`tasks`) permits it. Without isolation it asks nothing; each
  subagent's own calls are judged as they come.
- Isolation needs the workspace to be a git repository, and `git` on the
  path. Asking for it elsewhere is refused with that reason, before spawning.
- A worktree the subagent left exactly as it was made — no change, no commit,
  no untracked or ignored file — is removed, with its branch. Any other is
  kept, as is one whose state cannot be read; they are small (a checkout,
  shared object store) and the parent's report says how to take or remove
  each one.
- Without isolation, subagents share the parent's workspace and boundary —
  fine for read-only work, and the default, because most parallel work is
  investigation.

## Approvals and the record

A subagent is policed as its parent is. The same policy decides each call,
and anything it routes to a person goes to the parent's approver:

- in the interactive CLI, your prompt, which names the subagent asking. Asks
  from subagents running together come one at a time, and one whose turn is
  interrupted while it waits is not asked;
- in `-p`, nobody can be asked, so they are refused as `headless`, as the
  parent's own asks would be;
- in the console and the workbench, the parent session's approval prompt,
  labelled with the subagent, answered like the agent's own: the answer names
  the subagent's request, and a stale answer is refused. Asks from subagents
  running together are offered one at a time, each when the run starts
  waiting on it. In a run with nobody
  attending it, a scheduled one, they are refused as `headless`;
- in an ACP editor, a permission request whose tool call is named
  `subagent-<request id>`, sent after a `tool_call` of the same id.

Wherever a person can be asked, nothing a subagent asks is approved on its
behalf; only `abhed eval`, which has no person, approves its own asks.

A subagent runs inside its parent's session, so an "Always allow" chosen
earlier in the session covers its calls, and one chosen at a subagent's
prompt lasts for the rest of the session, as it would for the parent.

Each subagent keeps its own record, whose events carry the parent's session
as `parent_id`. The parent's record holds `subagent.spawned` and
`subagent.returned` naming that `session`, a `subagent.ask` for each call
the subagent put to the approver (written when its turn to be asked comes, with
the call, the reason and the `request_id` an answer names), and a
`subagent.action` for every call of the subagent's that was refused or put to
an approver, with the same `request_id`. On a server with durable storage a
subagent's session row belongs to the person whose session started it and
names that session as its parent; it is not listed among their sessions, and
nobody else can open it. Resuming a subagent's session id is refused, in the
CLI and the console; resume the session that started it. Deleting the session deletes its subagents the same
way: in Postgres the rows are marked deleted and kept for the audit, as the
session's own are. Calls the
policy allowed on its own are in the subagent's record only. HawkEYE's
report on the parent lists each subagent's session, a `subagent-denied`
finding for each refused call and a `subagent-destructive` warning for each
destructive command that was allowed.

A subagent cannot start one of its own unless `limits.nested_subagents` is
on. When it is, the nested subagent's `subagent.*` events are passed up, so
the top-level record holds every subagent at every depth.

## Background tasks

`task` and `tasks` take `"background": true` to start a subagent and go on at
once: the call returns `Started in background: task_id <id>`, and the result
arrives by itself later. The task id is the subagent's own session id. A
background task belongs to the session, not to the run that started it: it
keeps working after the agent's answer, and its result comes back as a
**notice**.

A notice is recorded first, as `subagent.notice` (from the system, marked
untrusted, redacted as the parent's record is), and then put in the
conversation as a `task_status` call and its result: the channel a tool's
output comes through, never the person's. The subagent's summary is shaped by
the files it read, so it carries a tool result's authority, not yours. The
`task` tool's description tells the model that such a result is not an
instruction.

A result that arrives after the agent's final answer follows it as a second
assistant turn (the `task_status` call), then the call's result. The hosted
OpenAI-compatible, Anthropic, Gemini and watsonx APIs accept this. A server
whose chat template insists on strict user/assistant alternation (some
Mistral-style templates on local or OpenAI-compatible servers) may reject
it; with such a model, run subagents in the foreground. With Anthropic
extended thinking on, the synthetic call carries no thinking block.

When a result arrives while a run is live it is taken at the next turn
boundary. When the session is idle, what happens is the **wake** mode,
`subagents.wake`:

| Mode | A result arrives while the session is idle |
|---|---|
| `off` | cannot happen: the run that started a task waits for it |
| `notify` | recorded and shown; the agent acts on it with your next message |
| `auto` (default) | recorded, then a short wake run (`session.woken`, naming the tasks), at most `subagents.wake_max_turns` turns and `subagents.max_wakes_per_hour` an hour; it ends `wake_limit`, and the session goes on. A message you send during it steers it, and from then on it is your run, with a prompted run's turns |

Results that arrive together are delivered together, after a two-second
settle. Only the session's own tasks finishing start a wake: nothing else,
no timer or outside event, runs the agent without you.

The mode in effect is the tightest of the managed configuration, yours, the
workspace's (which may only tighten), the session's own switch, and what the
surface can host:

| Surface | Most it runs | Notes |
|---|---|---|
| CLI, interactive | `auto` | `auto` unless a file sets `subagents.wake`; results are drawn at the prompt; a wake waits while you are typing; the work list under the input, `/tasks` (`view`, `kill`, `cancel <id\|all>`), `/wake` |
| CLI, `-p`; `abhed eval`; unattended server runs and schedules | `off` | the run, its exit code and `OnEnd` wait for the tasks |
| `abhed serve`, console and workbench | `auto` | the session shows `background` and the count; the woken turn streams live, marked "continuing with results from <task>"; the workbench's status bar lists the tasks still running, and `/tasks` or a click on it lists every task with a Cancel for each one running; `POST /v1/sessions/{id}/wake` switches it |
| `abhed acp` | `auto` | a task has a card of its own, completed by its result; a woken turn streams as session updates between `_abhed/wake/started` and `_abhed/wake/ended`, and a prompt sent meanwhile waits for it |
| `abhed rpc`, SDK | `auto`, default `off` | `off` joins the tasks; in `auto` a woken run's events stream and rpc answers it with a `woken` line; an explicit `wake` or `Wake` runs the agent on a result |

A wake run has no more authority than a prompted one: the same policy,
approver and "Always allow" scopes. It starts only when the last run
completed, budget and turns remain, the hourly limit allows, and the surface
can host it (on a server: not draining, the session held here, and its owner
still active). Otherwise the notice is recorded as `skipped:<reason>` and
handled as `notify`. On a server, whenever a result arrives with no run live,
in any wake mode, the owner is looked up first; if they are no longer active,
the session's other tasks are cancelled as `owner_inactive`, since nobody may
answer their asks. A woken run asks again before each model call and before
each of its calls is approved, so access withdrawn while it runs ends it as
`owner_inactive`, its pending call refused (`action.denied`, step `owner`).
An edition that manages accounts can stop a revoked, disabled or removed
user's work on the server at once (`StopOwnerBackground`): the live run,
background shells and tasks, and terminals end as `owner_revoked` (a
terminal's observation reads `exit 137 · on a terminal, owner_revoked`), and
no wake runs for them afterwards. A call then waiting on an answer is refused
as interrupted (`action.denied`, step `ask`, "interrupted before an
answer"), not at step `owner`. The Community Edition does not call it:
`abhed user remove` signs the account out, but a run already going for it
goes on until it ends, or until a woken run or a background result finds
the owner gone, as above.
Signing a person out is not that. After Sign out everywhere, or after their
administrator rights are removed, which also signs them out, the account is
still active: a background shell or task they started keeps running, and its
result still starts a woken run in their session, under the same policy and
with no one signed in to watch it. To stop that work, stop the session's
tasks first (Stop, `/tasks`), or remove the account (`abhed user remove`),
after which a result wakes nothing and the session's other tasks are
cancelled as `owner_inactive`.

**Stop means stop.** An explicit stop cancels every background task: Stop or
`/interrupt` in the console, Ctrl-C during a task (or twice at the prompt),
`session/cancel`, `CancelTask`. It also stops a `task` or `tasks` call that
is still starting its tasks: none starts after the stop ("stopped before this
background task started"), and a task whose start was already recorded ends
at once with the stop's reason. "Send now" redirects the run and keeps the
tasks already running. After an explicit stop no result wakes the session
until your next message, even in `auto`: the stopped tasks' results are
recorded as `skipped:stopped` and wait for that message. Stopping one task
yourself (`/tasks kill`, the task's stop in the console or Studio, the API's
cancel) is a stop too: its end, and any other result, wakes nothing until
your next message.
A run that ends in `error`, `max_turns` or `max_budget` takes them with it.
Ending the conversation (`/exit`, `/clear`, `/resume`, SDK `Close`, rpc `quit`)
ends them as `session_closed`; deleting a session, as `session_deleted`; a
drain, after its budget, as `shutdown`. Each task also has a wall-clock
lifetime, `limits.background_max_minutes`, and ends `deadline` past it.

Limits: `limits.max_background_subagents` bounds the tasks alive at once per
session, across runs, and a `tasks` call that would pass it starts none. Every
background task is also a spawn under `limits.max_subagents`, and spends from
the session's one token budget.

An ask from a background task goes to whoever the session asks, one at a time
with the agent's own. With no run live: the console's pending approval
(answered only by the session's owner, by its request id: an approve naming
no `request_id` is refused with 409 and does not answer it, with or without a
run live, so it can only answer the run's own ask; refused after 30
minutes; the console and workbench keep it answerable after the run ends,
until its own outcome or the closing end); the
terminal, where only a number offered answers it (`1` Yes, `2` the
session-wide Yes when one is offered, the last number No), and any other
line is a prompt, with a note that the approval still waits; in an editor, held until your next prompt opens, then asked first,
and refused after 30 minutes, or if that turn ends before you answer; and
refused where nobody can be asked.

`task_status` reports this session's tasks, or one with its summary once
done; `task_cancel` stops one, as `cancelled_by_parent`. A session's tasks are
its own: another session asking about one is told there is no such task.

On a server, a session with tasks running keeps its row open, so another node
does not continue it while they run here, and the event stream stays open for
their results; the closing end (`settled`) releases it. A result the store
refuses while the session is idle is tried again, waiting twice as long each
time; after five tries the work owed is settled so the session is not held,
and the result arrives at the session's next run (or, on another server, is
rebuilt from the record). A result that finished
before a restart, and a task a crashed process lost (ended `lost`), are
delivered on the session's next run.

Each server process holds the sessions it runs under a liveness identity (its
`node_id` with a token of this process after a `#`, or an id of its own when
no `node_id` is set) and refreshes it every 30
seconds while a run, a background task or a workbench hold is live. Another
process takes a session over only once that heartbeat is two minutes stale,
so a live task on one server is never mistaken for a crashed one by another
sharing the database. A heartbeat renews the claim only while it is still
that process's: a process whose heartbeat is refused (another has taken the
session over), or has failed for as long as the claim takes to go stale,
stops at once. Its run and background tasks end as `lease_lost`, recorded in
the tasks' own records, it writes nothing more to the session, and it drops
the session; its claim is never taken back. With the Postgres store Abhed
ships (`storage.driver: postgres`), the record is fenced in the store as
well: each event is inserted only while its writer
holds the session (a subagent's, while its parent's session is held), in the
same statement, so a process that lost the session writes nothing into it
even before its next heartbeat, and the refusal stops it there. What a tool
was already doing when the session was lost (a command running, a file being
written) is not something a record can undo. Another durable store an
embedder supplies is not fenced this way, and the server warns of it at
start. A node restarted with the same
`node_id` takes back the sessions it held when it starts, before it serves
anything, under its new token, so the process it replaced, if it still runs,
writes nothing more into them. Two running nodes must still never share a
`node_id`: the one that starts later takes the other's sessions. A session
whose row names no holder, as an older release left it, is taken as crashed
only once its last event was stored two minutes ago by the database's clock;
a writer's own clock does not count.

A server also sweeps at startup, and again every two minutes, by staleness
alone: every open session whose holder's heartbeat is stale is reconciled then (its lost tasks
recorded as `lost`, its ends written), so the session list shows it ended and
ready to continue rather than running.

## Resuming a finished subagent

`task` takes `"resume": "<task_id>"` to continue a finished subagent with a
follow-up prompt: the same session and record, with what it learned. Omit
`agent_type` and `model`, or give the ones it ran with; another role or
model is a new task. It may run in the background too.

- Only the session that started it, and its owner, may resume it; any other
  id, a subagent still running or being resumed, or a subagent of a subagent
  is "no such task".
- Its role is the definition as it is now, so its tools are never wider than
  today's (`definition_changed` is recorded when the definition changed); a
  role this session no longer offers is refused.
- It runs on the model it ran on, or not at all; and not at all when a
  managed role now pins another model.
- A worktree subagent resumes in its worktree, which must still exist on its
  branch, and is settled again after; one whose worktree is gone, or whose
  record names a directory other than the workspace with no branch to
  check (as records from before branches were recorded do), is refused. A
  resume never moves a subagent into the main tree.
- Each resume counts as a spawn and gets a fresh allowance of turns on top of
  those already spent. A conversation filling more than 80% of the model's
  window is refused: start a new task with what it found.

## What it is not

It is not a multi-agent framework with roles and message passing. Abhed's
model is simpler and stays simple: a subagent is a fresh loop with a complete
prompt, it returns a summary, and the parent decides what to do with it.
Running several at once, in separate checkouts, is the whole of what this
adds.
