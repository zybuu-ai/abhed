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

Each type also has a row in the OCSF table (`ocsf/table.go`): the OCSF 1.8 class its lines
become, or why it is not mapped. A new type fails the tests until it has one; the classes are
listed in [OCSF events and logs](../guide/30-ocsf-and-logs.md#classes).

| Type | Payload | Emitted by |
|---|---|---|
| `session.started` | workspace, model, mode, origin (`chat` or `workbench`), provider; the first event of every session the server starts, so a resume keeps its provider. The CLI's also carries `surface`, `headless`, the permission `mode` it started in, `bypass_confirmed` (true only when bypass came from a confirmed `-dangerously-skip-permissions`) and the system prompt's digests. A CLI run that continues a recorded conversation (`-c`, `-r`, `/resume`, `-fork-session`) records its own after the record, with `resumed: true` and `through_seq`, the last step it goes on from, followed by a `mode.changed` when its mode is not the one the record was left in; a conversation rebuilt within the same process records none. A new `abhed acp` or `abhed rpc` session records one too, with `surface` (`acp`, `rpc`), `headless`, `provider`, `model`, `mode` and `workspace_trust`: `requested` (`stored`, `trusted` or `untrusted`, the trust the surface asked for), `trusted`, `reason` and `sha256` of what applied. An `abhed acp` session continued from its record records a `session.resumed` instead | system |
| `policy.layer` | the permission rules an edition added to a server session as a layer of their own, such as a Team workspace's ([Policy layers](../guide/04-permissions.md#policy-layers-from-an-edition)): `layer` (its name, which `action.approved` and `.denied` carry as `layer`), `revision`, `sha256` of the rules, `deny`, `ask` and `allow` as applied, and `set_aside`, allow rules left out because the managed configuration sets the permissions. Recorded after `session.started`, and for a continued session before its next turn when the layer changed | system |
| `terminal.input` | call id of the shell, the line as typed, `edited`, or `withheld` with a reason | user |
| `user.message` | text, attachments | user |
| `agent.message` | text, reasoning (stripped from history) | agent |
| `action.requested` | tool, args (always a JSON object, the canonical arguments every step and the tool read; `{}` on a call refused at step `args`); `raw_args`, the refused arguments as text; `dropped_args`, keys a built-in tool did not declare and dropped (each clipped to 64 characters, at most 32 and then `+N more`), which the observation also names to the model as `[ignored: …]`; `resolved`, arguments the harness set or rewrote before policy read the call, such as the cluster of the session's only login or a Kubernetes call's default namespace; `via` when something issued it for the agent, such as `skill research pipeline` for a skill pipeline's step (recorded in the record of the loop whose `skill` call ran the pipeline); `supervisor`, the supervisor's verdict on the call when it gave one: `decision`, `code`, `rationale`, `advice`, `judge` | agent |
| `action.approved` / `.denied` | the step that decided (`step`; `plan` when an approved runbook's step stood in for the ask), `reason`, `by` (`plan` for that step); `plan_hash` and `plan_step` when the call was a runbook step; `rule`, the rule as written, when a deny, ask or allow rule decided, and `layer` when that rule came from a policy layer an edition added (below); `scope` when a remembered scope allowed it; `approver` and `granted_scope` when a person answered (below); `carried_from`, the call whose approval it took, with `by: carried`, when an approved `edit` or `write` failed with no effect and this retry of the same change took that approval | policy |
| `observation` | result, truncated, exit code; `sandbox`, the tier a `bash` command ran under (`none` on the host), when known; `sandbox_note`, when `bash` added a note that an operation was not permitted under a sandbox: what the output named that the sandbox denies (`ssh`, `kube`, `aws`, `gnupg`, `credentials` for another credential path in the home folder, `abhed` for Abhed's own state), or `denied` when it named none; `cwd`, the folder a `bash` command ran in, as the tool followed it | tool |
| `observation` with `not_run` | the answer to an approved call its turn ended before running (an interrupt, a shutdown): `is_error`, and a "Not run" text. It is a result, not an outcome, and HawkEYE does not mark the call run | system |
| `observation` with `outcome_unknown` | the answer to a call a process approved and then stopped before recording its result, written by the process that recovers the session (or, under an edition that moves turns, continues it): `is_error`, and an "Outcome unknown" text. The call is not run again, and the same call (tool and arguments) later in the session is asked of a person first, in every mode | system |
| `message.dropped` | queue id, client id, text, when it was queued, reason; a queued message the model never read because the server stopped first, or because the session moved to another process | system |
| `turn.continued` | written only under an edition that moves running turns between processes (Enterprise high availability): a turn another process left running continued here, before anything else it writes: `through_seq`, the last step the record had; `asked_again`, each call that never ran with the id of the new call it is asked again as (the old request is denied, step `handover`, and the new one goes through policy and approval as any call does); `outcome_unknown`, the calls that may have run, answered after the new calls with an `observation` marked `outcome_unknown`. Its run then goes on and records its own `session.ended`; a run suspended to be moved records none | system |
| `subagent.spawned` / `.returned` | description, agent type, `session` (the subagent's own record), turns, tokens; written to the parent's record and the subagent's; a nested subagent's are passed up to the top-level record. `spawned` also names the role and what it ran with: `definition`, `definition_source` (`builtin`, `managed`, `workspace`, `operator`), `definition_sha256` (a loaded file's content hash) and `tools` (sorted). Both events name `model`, and `provider` when the configured provider is known: the one the call or definition chose, or the parent's. `spawned` names `call_id`, the parent's call that started it. A background task's carry `background: true` and `task_id` (the subagent's session id); `spawned` adds `branch` and `start` for a worktree, and for a resume `resume: true`, `through_seq` (the child's last event it goes on from) and `definition_changed`. `returned` names `end_seq`, the child's `session.ended` for that run, whose answer the return carries. `returned` reasons beyond the terminal reasons: `cancelled_by_parent`, `session_deleted`, `session_closed`, `owner_inactive`, `owner_revoked` (the owner's access was withdrawn), `lost` (reconciled after a crash) | orchestrator |
| `subagent.notice` | a background task's result, or with `kind: "shell"` a background shell's end, entering the conversation, in the parent's record, recorded before it is applied: `task_id`, `session`, `description`, `status` (`completed`, `failed`, `cancelled`, or the cap that ended it), `reason`, `turns`, `tokens_in`, `tokens_out`, `provider`, `model`, `call_id`, `content` (the summary, redacted), `delivery` (`boundary`, `idle` or `wake`), `wake` (`notify`, `auto`, `caller` or `skipped:<reason>`). Marked untrusted. Fork rebuilds it as an assistant `task_status` call with that `call_id` and its tool result | system |
| `shell.started` | a command `bash` started with `run_in_background`, recorded once it runs: `shell_id`, `call_id` (the `bash` call that started it), `command` (as policy judged it, redacted), `description`, `sandbox` (the tier, `none` on the host), `secrets` (names only), `lifetime_ms`, and `from_foreground` when you moved a running foreground command to the background with Ctrl-B (`call_id` is then the call it ran under). Its output is not recorded here; each `shell_output` or `shell_kill` read is an `observation`, redacted | system |
| `shell.ended` | a background shell ended: `shell_id`, `call_id`, `state` (`exited` or `killed`), `exit_code` (128 plus the signal for a kill), `reason` for a kill (`cancelled_by_parent` for `shell_kill` or `task_cancel`, `user_interrupt` for a person's stop, `session_closed`, `owner_revoked` when the owner's access was withdrawn, `shutdown` when Abhed exits, `lifetime`), `duration_ms`, `output_bytes`, `truncated` (output went past the 1 MiB kept). Its notice follows as `subagent.notice` with `kind: "shell"`, unless the agent killed it itself | system |
| `session.resumed` | an `abhed acp` session continued from its record in a new agent, recorded after the record: `surface`, `via` (`load`, `resume`, `fork`, or `restart` for a changed workspace file), `through_seq`, `provider`, `model`, `mode`, `web` and `workspace_trust` as on `session.started`. Its configuration attempts follow, as after `session.started`; the session keeps its one `session.started` | system |
| `sandbox.bound` | `abhed serve` with `control_plane.per_session`: the control-plane sandbox the session's commands run in, before the first of them this process runs: `sandbox_id`, `generation`, `driver`, `tier`, `template`, `workspace` (the sandbox's folder) and `scope` (`session` or `user`). A command whose `sandbox.bound` cannot be recorded is not run | system |
| `session.woken` | a run no person prompted, for background results, recorded before it: `by` (`policy` or `caller`), `wake_mode`, `task_ids`, `wakes_last_hour`. No message | system |
| `session.wake_set` | the session's wake mode changed: `wake`, `by`, `ceiling` | user |
| `session.renamed` | a person gave the session a title: `title`, `from`, `by`; an empty title clears it. The session row's `title` follows the latest | user |
| `subagent.action` | a subagent's call that was refused or put to an approver, in the parent's record: `session`, `call_id`, `tool`, `subject`, `decision` (`allowed` or `denied`), `step`, `reason`, `by`, and `scope`, `approver`, `granted_scope` as on `action.approved`, and `request_id`, the subagent's `action.requested`. Calls the policy allowed on its own are only in the subagent's record | the answer's actor |
| `subagent.ask` | a subagent's call put to the approver, in the parent's record when its turn to be asked comes, one at a time across subagents running together: `session`, `subagent` (its description), `request_id` (the id an answer names), `call_id`, `tool`, `args`, `subject`, `reason`, `scope`, `via`. Its answer follows as `subagent.action` with the same `request_id` | agent |
| `subagent.message` | a message you sent a running subagent (from the terminal's work panel, `Message @<agent>`), copied into the parent's record as yours: `session`, `text` (redacted), `by: user`. The subagent's own record holds it as a `user.message` | user |
| `subagent.backgrounded` | a subagent started in the foreground that you moved to the background while it ran (the terminal's Ctrl-B): `session`, `task_id`, `description`, `background: true`, `by: user`. Its `subagent.returned` then carries `background` and `task_id`, and its result arrives as a background task's does | user |
| `compaction.started` / `.completed` | `before_tokens`, `after_tokens`, `summary`, `trigger` (`auto` or `manual`). A `started` is recorded only once there is older history to summarise into a shorter one, and every `started` is followed by a `.completed`: with the token counts, or with `error` when the summary failed. When there is nothing to summarise (automatic or `/compact`) neither is recorded and the compaction count does not rise | context mgr |
| `plan.updated` / `todo.updated` | items | agent |
| `model.call` | one round trip to the model: `turn`, `model`, `tokens_in`, `tokens_out`, `tokens_cached`, `context_window`, `first_token_ms`, `latency_ms`, `tool_calls`, `cut_off`, `error`, and `retryable` when a call or stall timeout ended it; `purpose` names a call outside the conversation (`suggestion`), whose tokens count in the session's totals and budget but which is no turn and says nothing of the context | system |
| `monitor.verdict` | the supervisor's judgement of one call ([The supervisor](../guide/04-permissions.md#the-supervisor)): `call_id`, `before` and `after` (the decision; `after` is never looser), `code` (`ok` or the concern), `confidence`, `rationale`, `latency_ms`, `version` (the rules' or the prompt's), `unavailable` when the judge could not answer and the fail-closed rule decided, `skipped` (`turn budget spent`) for a call `max_per_turn` kept from the judge, whose decision stands as policy made it. Recorded before the call's `action.requested`; a call the fast path skips has none | system |
| `supervisor.correction` | what a refused call's answer told the model: `call_id`, `code`, `decision`, `text` (`Supervisor: <code>. <rationale>. Do this instead: <advice>.`). Follows the call's `action.denied` | system |
| `supervisor.replan` | the note added to the answer of the third refused or failed call in a row (and every third after): `failures`, `text`, which restates the task and the approved plan | system |
| `suggestion.offered` | a next prompt offered to the person after a run completed: `text` (one line, at most 80 characters, control and format characters removed, redacted), `turn`. Recorded after the run's `session.ended` (which says `suggesting: true`), by a call made off the run, and followed by that call's `model.call` with `purpose: suggestion`, which is always recorded last, offered or not (a session that closes first records neither); the next prompt, a wake or typing cancels it; never after an error, a stop, a wake run, or while an approval waits. Only shown, never sent: the person accepts it into the input and sends it themselves | system |
| `answer.verdict` | the checks on one answer against the session's record ([Verified answers](../guide/29-verified-answers.md)): `mode`, `source` (`answer_tool` or `text`), `claims` (each with `n`, `text`, `kind`, `verdict` (`verified`, `unverified` or `contradicted`), `reason`, `evidence` as the `seq` of the entries it rests on, `cited` as the agent wrote them, `checked`, `load_bearing`, `conflict` (`seq`, `call_id`, `excerpt`) when contradicted, and `critic` when the critic downgraded it), the counts `verified`, `unverified` and `contradicted`, `contract` (`ok`, `require` with each command's `seq`, `exit_code`, `ok`, `reason` and `reused`, and `protected_changed`), `critic` (`checked` or `unavailable`), `revision` (the note sent for the run's one revision turn; another `answer.verdict` follows it), `outcome` (`verified` or `unverified`) and `turn`. Recorded after the answer's `agent.message` and before `session.ended`. When the agent answered through the `answer` tool, its text is recorded as an `agent.delta` and an `agent.message` first; the contract's commands are `action.requested` by `system` with `via: "completion contract"` | system |
| `session.ended` | terminal reason, totals; `background`, what the session still owes at a run's end: background tasks running, results not yet delivered and a wake starting (the session is not over while it owes any: in Postgres its row stays open and the event stream stays open); `settled`, the closing end once it is all done; `detail`, more of how a person stopped the run (the CLI's Esc: `turn interrupted, background shells kept`; Ctrl-C: `interrupted, background shells stopped`; a stop signal to the process: `stopped by terminated`, `stopped by interrupt` or `stopped by hangup`); `suggesting`, a next-prompt suggestion follows (the event stream stays open until its `model.call`); `recovered`, written by reconciliation after a crash. Terminal reason `wake_limit` ends a wake run (exit code 0); `session_closed` is recorded when an ACP editor closes a session or goes away with something recorded after the last end (terminal lines, saves, or nothing but its start), carrying the last run's totals; the session can be loaded and continued as after any end | system |
| `conversation.forked` | `through_seq`; the conversation goes on from that step, and the steps between it and the marker are abandoned: kept in the record for audit, left out of every rebuild (`/fork`, `/tree`, `/resume`, a continued session) | user |

**The interactive CLI's own events.** These types and their payloads are
defined (`internal/agent/event_cli.go`) so the work that records them shares
one shape. The interactive CLI records each of them. A change made before a conversation has a record,
such as a mode chosen before the first message, is recorded when the
conversation opens, ahead of that message. A conversation opened after
`/clear` or `/resume` first restates what it inherits: the mode, as
`mode.changed` with `via: carried`, and each added directory as
`workspace.dir_added`, so each record stands alone. Rewind reuses `conversation.forked`
and adds `file.restored`; taking a worktree back adds `worktree.taken`.

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
| `session.branched` | the first event of a new session: `from`, the session it was copied from; `through_seq`, the last event taken; `unverified`, why the source's record failed verification, absent when it verified. A copy of the source's conversation and undo history follows, renumbered, with new ids; copies from a file or a failing record are `untrusted`. A `through_seq` inside a copied `session.started` or `session.resumed` is renumbered to the copy's numbering, as a restore's checkpoint is; `conversation.forked` markers are not copied | user |
| `file.restored` | `path`, `before_sha256` (absent when the file did not exist), `after_sha256` (absent when the restore removed it), `checkpoint`, the seq of the `checkpoint.saved` whose content was put back, `by` (`user`). Recorded after the person's `action.requested`, the decision and its `observation` | user |
| `checkpoint.saved` | a file's content just before the agent changed it, with the write or edit tools or through a `bash` command: `path`; `sha256`, the blob holding the content, absent when the file did not exist or none was kept; `turn`; `mode`, its permission bits; `skipped`, why no content was kept (a read deny rule, a name that holds keys, or a `bash` snapshot's limit) | system |
| `worktree.taken` | a subagent's worktree taken back by hunk (`/take`, the workbench): `worktree` (`.abhed-worktrees/<id>`), `branch`, `base` (the commit it shares with the workspace), `taken` (paths written or removed), `conflicts`, `refused` (with the reason), `left` (hunks not taken), `removed` (the worktree and its branch). Each file taken is the person's own `write` or `delete` before it, with its decision and `observation` | user |
| `debug.launched` | a person's debug session started in the session's sandbox (Studio's `_abhed/debug/launch`): `debug_id`, `call_id` (the person's `bash` call before it, whose command is what policy and the supervisor judged), `language` (`go` or `python`), `command` (the debuggee's command line), `tier`, `bridge` and `bridge_pinned` for a Go session (the Linux build of Abhed that bridged Delve's socket, and whether the build pinned its checksum), `loopback` for a session on the process tier with loopback open on the person's agreement | user |
| `debug.setup` | a person's choice about where debugging runs (Studio): `action` (`use_container`: the workspace's `.abhed/config.json` set to the container tier, with its `path`; `process_tier_consent`: agreed to debug this workspace on the process tier with loopback open, with the launch's `call_id`; `process_tier_forgotten`), `workspace` | user |
| `debug.stopped` | the session ended: `debug_id`, `call_id`, `reason` (`stopped` by the person, `ended` by the protocol, `failed`, `adapter-missing`, or `closed` with its session or connection), `exit_code` of the adapter command, `duration_ms`, `dropped` (the adapter's messages not passed on because a decisive key repeated or varied in case) | user |
| `lsp.started` | a person's language server started in the session's sandbox (Studio's `_abhed/lsp/start`, or after a crash): `lsp_id`, `call_id` (the person's `bash` call before it, whose command policy and the supervisor judged), `language`, `command`, `tier`, `restart` (how many restarts came before) | user |
| `lsp.stopped` | it ended: `lsp_id`, `call_id`, `language`, `reason` (`stopped`, `crashed`, `closed`, `failed`, `workspace-config-changed`: a rust-analyzer.toml appeared or changed while it ran), `exit_code`, `requests` and `refused` (the editor's messages passed and refused; they are not recorded one by one), `duration_ms` | user |
| `completion.enabled` | recorded just before a session first sends code to a completion provider (Studio's `_abhed/complete`); recorded again with `mode: "chat"` when the session falls back to chat; nothing is sent if it cannot be recorded: `provider`, `model`, `local` (its endpoint is on this machine), `mode` (`fim` or `chat`). Completions are not recorded one by one | user |
| `next_edit.enabled` / `next_edit.summary` | the same as `completion.enabled` and `completion.summary`, for Studio's next-edit suggestions (`_abhed/nextEdit`); `mode` is always `chat` | user / system |
| `commit_message.requested` | recorded before a staged diff goes to the commit model (Studio's `_abhed/commitMessage`); nothing is sent if it cannot be recorded: `provider`, `model`, `local`, `files` sent, `excluded` (staged but left out: a read rule, a key file, Abhed's state), `bytes` sent after redaction, `truncated` | user |
| `commit_message.generated` | its answer: `outcome` (`served`, `empty` or `failed`), `tokens_in`, `tokens_out` | system |
| `completion.summary` | a session's completions, when it closes: `provider`, `model`, `requests` (sent), `served`, `failed`, `cancelled` (after sending), `refused` (not sent: a file a rule keeps, the rate limit), `superseded` (replaced by a newer request before sending), `tokens_in`, `tokens_out` (as reported; a lower bound) | system |
| `plan.proposed` | `text`, the plan the agent submitted in plan mode; `hash`, what an answer names; `runbook`, when it has one: `hash` (sha256 over the text and each step's hash, in order) and `steps`, each `index`, `tool`, `target`, `args` as the loop will see the call, its `hash`, `note`, and `destructive`, why it cannot be undone. Recorded `user` when the operator supplied the runbook (`-runbook`, `Options.Runbook`) | agent |
| `plan.decided` | `decision` (`accept` or `keep-planning`), `to_mode`; never `auto` or `bypass`; `plan_hash` and `runbook` when an accept bound one; `via: operator` when the operator's setting approved it; `by`, the signed-in person who answered, on a server. Recorded before the runbook is bound, which a record that did not take it never is | user |
| `plan.step` | a call that ran as a runbook step: `call_id`, `plan_hash`, `step`, `of`, `step_hash`, `tool`, `target`, `by` (`plan` when the plan's approval stood in for the ask, else who let it through). Follows its `action.approved` | the approver's actor |
| `model.fallback` | `from`, `to`, `reason`; a move to the next model in `model.fallback` after the one in use could not be reached or refused access | system |
| `toolchain.install` | a language toolchain installed for the session's commands: `lang`, `extra` (set for its debugger, `delve` or `debugpy`, or its language server, `gopls`, `basedpyright`, `typescript-language-server` or `rust-analyzer`, installed with it; `version` is then the debugger's, and Delve's `built_with` names the Go it was built with and `digest` the built binary's sha256), `version`, `platform` (`linux-arm64`…), `outcome` (`ok`, `failed`, `declined`, `refused`, `cancelled`), `why` (`requested`, `pinned by <file>`, `latest stable`), `source`, `url`, `digest` (the published `sha256:` or `sha512:` it was checked against), `bytes`, `cached` (installed already, nothing downloaded), `consent` (`call` for the agent's `toolchain` tool, whose approval is its `action.approved`; `cli`, `cli -yes` or `studio` for a person's own act), `error`, `by` (`agent` or `user`), `surface` (`tool`, `cli`, `acp`). The tool's follows its call's `observation` | system, or user |
| `toolchain.registry` | a language's official package registries opened to the session's commands through its egress proxy, with the person's agreement to the language's toolchain: `lang`, `hosts`, `consent`, `by`, `surface`. Only where `sandbox.toolchains_registries` allows it (§ Toolchains); each connection is then an `egress.decision` | system, or user |
| `ollama.start`, `ollama.install`, `ollama.pull` | a person's attempt to get a chosen local Ollama model working: `op` (`start`, `install` or `pull`), `outcome` (`ok`, `failed`, `declined`, `refused`, `cancelled`, or `manual` when the step is left to the person, such as an installer page opened), `provider`, `model` (the Ollama tag), `base` (the loopback API), `command` exactly as the person was shown it, `source` (where an install or a pull comes from), `bytes` (a pull's size), `pid` (a started server), `consent` (`asked`, or `always` for a standing choice to start), `problem` (the kind, such as `assist_off`, `not_admin`, `port_busy`, `disk_short`) and `error`, `by` (`user`) and `surface` (`cli`, `tui`, `server`, `acp`). Only a person's model selection records them; no tool can | user |
| `hook.fired` | `extension`, `event`, `verdict` (`block`, `ask` or `annotate`; a hook never allows) | system |
| `record.repaired` | `reason`, `truncated_bytes`; recorded by the local record when it is opened for writing and its last line was left unfinished by a crash: cut off when the head does not count it (`truncated_bytes` says how much), or completed when it was whole and lost only its newline (`truncated_bytes` 0) | system |
| `config.refused` | a setting that did not take effect as written, or a refused change: `layer` (`user`, `settings`, `workspace`, `env` or `command`), `source` (the file, flag or variable), `key`, `value` asked for with credentials redacted, `decision` (`set_aside`, `ignored_untrusted`, `overridden` by the managed file, or `refused` by `/config set`), `reason`, and `principal`: `kind` (`os-user` or `agent-command`), `os_user`, `uid`, `session_owner` on a server, `agent_command` and `command_id` inside an agent's command. Recorded after `session.started`, and when `/config set` refuses | system |
| `config.narrowed` | the same fields, for a `web_search` or `web_fetch` setting a layer below the managed file narrowed, as it may: turned off, fewer results, fewer hosts | system |
| `egress.decision` | one decision of the session's egress proxy or of Abhed's own requests under `sandbox.network: "allowlist"`: `call_id`, `kind` (`connect`, `http`, `dns`, `auth`, `request`, `summary`; for Abhed's own requests `model`, `web_fetch`, `web_search` or `mcp`), `host`, `port`, `ip`, `synthetic`, `method` and `path` (plain HTTP; the path without its query, left out with `egress.record_paths: false`), `decision` (`allow`, `deny` or `would_deny`), `rule`, `reason`, `bytes_in`, `bytes_out`, `repeats` on a rate-limit summary, `session` for Abhed's own requests and stdio MCP servers, `mcp_server` for a stdio server's. Never bodies, header values, query strings or credentials ([Network policy](../guide/21-network-policy.md#the-record)) | system |
| `fence.denied` | the system calls the fence's seccomp filter refused a command, as Abhed answered them (mount_namespace mode, Linux 5.7+), recorded once it has exited: `call_id`, `sandbox`, `source` (`seccomp`), `calls` (each refused call's count by name, a socket's with its family, such as `socket(unix)`; at most 64 names, the rest as `other`) | system |
| `sandbox.git_planted` | a `commondir` taken out of a git folder before a command, which is then not run (bubblewrap, a container, the vm tier, the fence): `call_id`, `sandbox`, `workspace`, `entries` (`path`, `outcome` `moved` with `moved_to` under `~/.abhed/quarantine`, `removed`, `still_present`, or under the fence `renamed_in_place`), `reason` | system |
| `sandbox.git_walk_bounded` | the look for git folders stopped at a bound: `call_id`, `sandbox`, `workspace`, `bound`, `refused`, `reason`. With `refused: false` (the workspace's folder bound, recorded once per sandbox) a repository past it, other than the workspace's own, its submodules and linked worktrees and those found earlier, is not protected; with `refused: true` (a git folder's `modules` past its entry bound) the command is not run | system |
| `sandbox.git_linked` | a git folder part that is a symbolic link (`.git/hooks`, a `.git`, `modules` or `worktrees` folder, or a folder in either), which the sandbox cannot hold read-only, refusing the command: `call_id`, `sandbox`, `workspace`, `paths`, `refused: true` | system |

**Who settled a call** is in `by` on every `action.approved` and `action.denied`:

| `by` | Meaning | Actor |
|---|---|---|
| `policy` | a rule or the mode decided; no one was asked (on the workbench, also malformed arguments at step `args`) | system |
| `reviewer` | a person was asked and answered | user |
| `user` | the person made the call at the workbench | user |
| `carried` | a retry of the very same change took the approval a person gave the attempt that failed with no effect (`carried_from` names it); nobody reviewed the retry itself, and the original approval stays recorded `reviewer` by the user | system |
| `session-scope` | an "always allow" chosen earlier in the session let it through; `scope` names it | system |
| `headless` | nobody could be asked (`-p`, `rpc`, an SDK run without an approver, `abhed eval`, or a subagent of one of these), so the run's fixed answer applied; a refusal's reason starts `no approver:` | system |
| `system` | the harness: an unknown tool (step `unknown`); arguments that were not one object, named a key twice or in another case, or gave a tool a key it does not take where that is refused (step `args`, reason naming the key); a call that could not succeed, refused before anyone was asked (step `precheck`, reason the tool's error); a request that ended before an answer (step `interrupted`, reason `interrupted before an answer`, `server shut down before an answer`, `deadline passed before an answer`, or the same with `before the answer was applied` when an answer arrived as the wait ended); an ask no one answered (step `unanswered`, by `system`: reason `no answer within 30 minutes: …`, `input ended: …`, a client that could not ask, or `approval failed: …`); or an ask whose record was withheld, so it was put to no one (step `withheld`) | system |

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
`completed · max_turns · max_budget · policy_denied · user_interrupt · error · shutdown · retry_exhausted · stalled · deadline · unverified`

`unverified` means strict verification or a completion contract could not verify the answer after its one revision turn (exit code 8).

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

**Events are hash-chained** (schema version 8). The sketch above leaves out
three columns the trigger `zz_events_chain` sets on every insert, whatever the
writer sent, in the insert's own transaction: `chain_pos` (1, 2, … within the
session), `prev_hash` (the previous position's `hash`, 64 zeros at position 1)
and `hash`, the SHA-256 in hex of `prev_hash`, a newline, and the row's
columns `id, session_id, tenant_id, parent_id, seq, type, payload, actor,
trust, created_at` as Postgres renders them as `jsonb` text in UTC. The chain
is **per session**: sessions never wait on each other's lock, a session's
chain is whole on its own (export, verify, erase by key), and a subagent's
session has its own. What a per-session chain cannot show by itself is a
whole session removed, or the end of one cut; that takes a head kept
elsewhere (`record.witness`, or an edition's anchor; see
[the Postgres record](../guide/12-records.md#the-postgres-record)). The control plane's `sandbox_audit` is chained the same way **per
tenant**, being one stream.

`chain_pre` (schema version 11) holds, per chain, how many unchained rows were
already there when the chain began, counted once by a migration and never
chained over. The runtime role can only read it. A check fails any unchained
row it does not cover: one written with the trigger off. The runtime role
holding INSERT, UPDATE, DELETE or TRUNCATE on `chain_pre` is refused at
start, as it is for any privilege beyond appending to the record.

**Commit order** (schema version 12). `events.commit_tx` (`xid8`, default
`pg_current_xact_id()`) is the writing transaction, and `(commit_tx,
created_at, session_id, seq)` is the order `Postgres.ScanCommitted` reads:
only rows whose transaction is older than every one still running, so a
reader that resumes from a position (`CommitPos`) never skips a row that
commits later. Rows from before version 12 have no `commit_tx` and come
first, by time. The column was added without a default and given one after,
so existing rows are not rewritten. Version 12's index on `COALESCE(commit_tx,
'0')` is replaced in **schema version 16** by `events_commit_tx_idx` on
`(tenant_id, commit_tx, created_at, session_id, seq) WHERE commit_tx IS NOT
NULL`: under row security Postgres does not take a condition on the
`COALESCE` into an index, so every scan read all of the tenant's rows; on the
bare column it is an index range, and rows without a transaction are read on
their own through `events_tenant_time_idx`.

**The OCSF file's cursors** (schema version 13). `ocsf_cursors` holds, per
tenant and sink (`serve` for `abhed serve`'s `record.ocsf`), each session's
last seq written (`seq`), the model `provider` its later lines need, and
`primed`; the row with session `''` holds `commit_pos`, the place in commit
order the writer reads on from. Its primary key is `(tenant_id, sink,
session_id)`, and its row security is forced with the tenant policy every
tenant table has. The runtime role may SELECT, INSERT and UPDATE it, never
DELETE; a cursor's seq only moves forward. The writer holds a Postgres
advisory lock on its own connection while it writes
([OCSF events and logs](../guide/30-ocsf-and-logs.md#the-ocsf-file)).

**A cheaper chain** (schema version 14). `abhed_events_chain` replaces the
generic chain trigger on `events` with static queries and one rendering of
the hashed columns (`jsonb_build_object`, the same text and so the same hash
as before), and sets `inserted_at` and `commit_tx` itself, whatever the
writer sent, so no writer can keep a row out of the commit-ordered scan; the
separate `events_inserted_at` trigger is dropped, so each insert runs one
trigger. The GIN index on `events.payload` is dropped: no query used it, and
payloads are sealed, so it indexed ciphertext on every insert.

**A deleted session takes no row** (schema version 15). Once
`abhed_events_chain` holds the session's lock it checks `sessions.deleted_at`
again and refuses the row (SQLSTATE `ABD01`). A delete takes the same locks
for its whole session tree (`LockSessionTree`, `DeleteSessionTx`), so no row
an insert's earlier check let through can land after the delete commits.

The chain head API (`store/chain`): `Source.Head(scope)` is the last position
and hash, and `Source.VerifyRange(scope, from, to)` recomputes positions
`from..to` and returns the hash it links to (`Before`) and the hash at `to`.
`Postgres.EventChain()` (scope: a session), `Postgres.AuditChain()` (scope:
the tenant), `local.Store.Chain()` (scope: a session, positions are lines),
`local.AuditChain(dir)` (scope: the tenant's `api.jsonl`) and
`sqlite.Store.AuditChain()` (the control plane's SQLite record) answer it.

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
GET  /v1/sessions/{id}/logs          → the same stream as OCSF events (?format=shorthand,
                                       ?version=, ?type=); see guide/30-ocsf-and-logs.md
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
                                       Retry-After too many answers waiting; 503 with
                                       Retry-After row not written in time; 404 a session
                                       the caller cannot see, whatever the body. Elsewhere: 204 for an unbound answer,
                                       or one bound to the stored row's request, recorded on
                                       the store; 421 for one bound to another request or to a
                                       row from before rows carried request_id; 409 when no
                                       node runs the session or its request ended
                                       (request_id: the action.requested event's id)
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
