# Automation

Three ways to run Abhed without a person at the prompt.

## Headless

```bash
abhed -p "fix the failing tests" -mode auto -allow 'bash(go test*)'
abhed -p "explain what pkg/auth does" -mode plan
abhed -p "add a test for Valid" -output-format json > events.jsonl
```

Exit codes: `0` completed · `2` turn limit · `3` budget · `4` policy denied ·
`5` retries exhausted · `130` interrupted. A CI job can branch on those.
`abhed -p` and the interactive CLI report `130` for any stop signal (Ctrl-C,
SIGTERM or a hang-up), from the moment they start; `rpc`, `acp`, `eval` and `resolve` report 128 plus the
signal's number (130, 143, 129); `serve` exits `0` once it has drained.
`serve` ignores further signals while it drains; `-p`, `eval` and `resolve`
end at once on a second signal.

A bad invocation, such as an unknown `-output-format` (`text` or `json`), an
unknown `-mode` or a word that is not a command, exits `2` before anything
runs. Its stderr line tells the cases apart from a turn limit and from each
other: `unknown -output-format`, `unknown -mode` or `unknown command`.

There is no one to approve, so anything needing approval is refused. Name what
may run with `-allow`, and keep the list narrow.

## RPC

For a caller that is not Go, `abhed rpc` speaks line-delimited JSON on stdin and
stdout — no server, no port, no auth for what is one process talking to its own
child.

```python
p = subprocess.Popen(["abhed", "rpc"], stdin=PIPE, stdout=PIPE, text=True, bufsize=1)

def send(**kw):
    p.stdin.write(json.dumps(kw) + "\n"); p.stdin.flush()

send(method="start", mode="auto")
send(method="prompt", prompt="fix the failing tests")
# every event arrives as {"type":"event", ...} while it works
```

| Method | |
|---|---|
| `start` | open a session — `workspace`, `mode`, `allow`, `deny` |
| `prompt` | send a prompt, get the final answer |
| `steer` | redirect a run in progress |
| `usage` | tokens, turns, compactions |
| `export` | the HTML transcript |
| `providers` | what this build supports |
| `tasks` | the session's background tasks |
| `cancel_task` | stop one background task — `task_id` |
| `wake` | run the agent on background results waiting for it; an error when none waits |
| `quit` | close; running background tasks end as `session_closed` |

`start` takes `wake`: `off` (the default), so `prompt` answers when the work,
background tasks included, is done; or `notify`, so they outlive the prompt
and their results arrive as event lines. rpc never starts a run on its own.

Events stream as they happen rather than only at the end, so a caller can render
progress. `steer` is why this is a persistent process rather than one request
per run.

The session has the CLI's tool set, subagents, MCP servers, skills and the
other tools the configuration enables included. There is no approver, so a
call that needs approval, a subagent's too, is refused.

Commands run in the sandbox tier the workspace's configuration sets
(`sandbox.min_tier`, `process` by default), as from the terminal: `start`
answers `{"type":"error"}` when no backend meets that tier, rather than running
`bash` on the host.

On Ctrl-C, SIGTERM or a hang-up, `abhed rpc` and `abhed acp` end the prompt
that is running, and the command it was running with everything that command
started, wait for it to record its end, then exit with 128 plus the signal's
number (130, 143, 129), as a shell reports a process the signal ended. A
second signal exits at once. Before, the signal ended the process outright and
left the command running.

## Resolving an issue

```bash
abhed resolve https://git.example.com/team/tool/issues/12 -allow 'bash(go test*)'
```

reads the issue, works on it in a branch of its own (`abhed/issue-12`, in a
worktree at `.abhed-worktrees/issue-12`, so your checkout is untouched),
commits what changed, pushes, and
opens a pull request that links the issue and summarises the diff. **GitHub,
GitLab and Gitea/Forgejo** are one command; a self-hosted instance is the
ordinary case: the kind is inferred from the host, `-kind` names it, and
`-ca file.pem` (or `ABHED_FORGE_CA`) trusts your certificate authority.

The run is sandboxed in the worktree, and your checkout's `.abhed/` is
Abhed's state for it as well: its commands can neither read nor write it.

The token is `GITHUB_TOKEN`, `GITLAB_TOKEN` or `GITEA_TOKEN` from the
environment, or the same name in [`abhed secret`](04-permissions.md#secrets).
It is used here, outside the session — the run sees the issue text and the
checkout, never the token — and it reaches `git push` through the
environment, not the command line, scoped to the forge's host.

The branch is pushed over https to the issue's own repository, at an
address built from the issue URL; a git remote, which the run could have
changed, is not consulted, and `-remote` is ignored. The push starts from a
temporary repository that borrows your checkout's objects and has no
configuration of its own, so your global git configuration applies but the
repository's does not: nothing the run wrote there (a proxy, a TLS setting,
an address rewrite) decides where the token goes. The temporary repository
is outside your checkout, so `includeIf "gitdir:…"` sections of your global
configuration do not match it: give a forge's certificate authority with
`-ca`, and scope a proxy or TLS setting to the host with `http.<url>.*`.

On the process sandbox tier, the push is refused, rather than trusting a file
the run could have written, when `~/.gitconfig`, `~/.config/git/config` or
`$XDG_CONFIG_HOME/git/config`, or the folder it would be made in, lies in the
run's worktree, a temp folder or a toolchain cache (for example a home
directory under `/tmp`). The rest of the checkout is not the run's to write,
so a home directory kept as a dotfiles checkout still pushes. The temporary
repository is made in `~/.abhed/push`, which sandboxed commands cannot write;
if that folder is in one of those areas, or is a file, a link or not your own
(ownership is not checked on Windows), the push is refused. On the `none`
tier, or in a container that mounts your home directory, the run can write
your global configuration and `~/.abhed/push`, and the push is not refused.

The commit, diff and push run with the program-running settings switched
off: hooks (including your own pre-commit and pre-push hooks), commit
signing, clean and smudge filters (so Git LFS does not run), textconv and
external diffs, credential helpers and every transport but https. git's own
environment variables are dropped too, so `GIT_SSL_CAINFO` does not apply:
use `-ca`. A change that holds a git repository of its own, such as a nested
checkout or a submodule the run made, is not committed.

If your global configuration rewrites https addresses to ssh (a
`url."git@github.com:".insteadOf https://github.com/`), the push stops with
"transport 'ssh' not allowed". Keep pushes to that host on https with
`git config --global url.https://github.com/.pushInsteadOf https://github.com/`;
fetches keep using ssh.

Opening the request is a mutating action of its own, `forge_pr`, judged by
policy like any other: a deny rule refuses it, an allow rule
(`forge_pr(team/tool)`) or `-y` permits it, and otherwise you are asked at
the terminal. No mode opens a pull request on its own. A run that changes
nothing pushes nothing, removes its worktree and its branch, and exits 2; if
it left ignored files, the worktree and branch are kept for them. The run is
asked not to commit, but a commit it made on its branch, on top of where it
started, is pushed as the change. A run that leaves the worktree on a detached
HEAD or another branch, or rewrites the branch, pushes nothing: resolve fails,
says where the commits are, and keeps the worktree. A run that fails, or a resolve stopped by
Ctrl-C, SIGTERM or a hang-up, leaves the worktree for inspection and says
where; a stopped one pushes nothing it had not pushed and exits with 128 plus
the signal's number. Otherwise the worktree is removed when resolve ends, and
the branch stays. `abhed eval` stopped the same way prints no summary, writes
no `-json` report, and exits the same way.

Not here: the bot that reacts to labels and comments. That is a multi-user
feature and lives with the server.

## Editors

`abhed acp` speaks the [Agent Client Protocol](https://agentclientprotocol.com)
over stdio, so an editor that supports it — Zed, JetBrains, and others — runs
Abhed as its agent without an extension per product. Point the editor at the
binary:

```json
{"agent_servers": {"Abhed": {"command": "abhed", "args": ["acp"]}}}
```

The editor's approval dialog is the approver: an `ask` decision becomes a
permission request with *Allow once*, *Always allow* the rule policy
suggests when one is offered, and *Deny*. It can answer an ask; it cannot
lift a deny rule, and the sandbox tier and the workspace boundary are
whatever the configuration says, exactly as from the terminal.

The request's `toolCallId` is the id of the `tool_call` update for the same
call, and that update is sent first, so the editor can show the prompt on
that call. A call the model wrote as prose has no id of its own; it is named
by its `requestId` everywhere instead.

The option ids are bound to the request. An answer naming any other option
is refused, including *Always allow* where it was not offered, and so is a
cancelled or unreadable reply. Each is recorded as refused by the system,
not as a reviewer's no.

The request's `toolCall._meta["zybuu.ai/abhed"]` carries:

- `tool`, and the policy `step` that asked, with its `reason`;
- `destructive`, true for a command with no undo, whichever step asked, so
  an editor can confirm it more firmly;
- `scope`, the rule *Always allow* would grant, when it is offered;
- `requestId`, the id of the recorded `action.requested`.

The title, `rawInput` and reason are the copy the session records. If the
record withheld that copy, the call is refused without asking, since nobody
can review input they cannot see, and the editor is told why. A `tool_call`
update names its tool in the same `_meta` key.

The agent's text, its reasoning, every tool call with its outcome, the plan
and the context usage stream to the editor as `session/update`
notifications, and the session is recorded like any other. The plan is the
agent's `todo` list, sent as a `plan` update each time it changes.

The session has the CLI's tool set, subagents included. A subagent's ask is a
permission request like the agent's own, labelled with the subagent, named
`subagent-<request id>` and preceded by a `tool_call` of that id; its answer
settles that call. The subagent's other calls are in its own record, not
streamed to the editor.

The workspace's `.abhed/config.json` applies whole only once the person has
trusted it; `session/new` reports the decision in
`_meta.abhed.workspaceTrust`, with the settings it ignored, so the editor can
ask and then run `abhed trust grant`. The editor may send
`_meta.abhed.trust: "untrusted"` to take only what tightens; it cannot grant
trust over the wire; pass the reported `sha256` to `abhed trust grant
-sha256` so only the content the person saw is trusted. Starting
`abhed -trust-workspace acp` trusts the file of every workspace the editor
opens for the life of the process, not only the one it was started in. See [Workspace
trust](../architecture/workspace-trust.md).

Background tasks run in `notify` mode. Each gets a `tool_call` card named
`bg-<task id>`, open while it runs and completed (or failed) by its result,
whether or not a prompt turn is open. A background task's ask needs an open
prompt turn, since that is when an editor can be asked: between turns its
card says it is waiting, and the ask goes out, first, when your next prompt
opens; it is refused after 30 minutes. `session/cancel` stops every
background task too, with or without a prompt open.

### Choosing the model

`session/new` lists the models the configuration defines as a session
config option, the form ACP schema v1.23.0 prefers:

```json
{"sessionId": "s-…", "configOptions": [{"id": "model", "name": "Model",
  "category": "model", "type": "select", "currentValue": "local",
  "options": [{"value": "local", "name": "local", "description": "qwen3:8b (ollama)"},
              {"value": "work", "name": "work", "description": "llama-3.3-70b (openai-compatible)"}]}],
 "models": {"currentModelId": "local", "availableModels": [
  {"modelId": "local", "name": "local", "description": "qwen3:8b (ollama)"}, …]}}
```

Each value is a provider's name under `model.providers`; the description
is its model id and type, never its `base_url`, key or `api_key_env`. To
switch, the editor sends

```json
{"method": "session/set_config_option",
 "params": {"sessionId": "s-…", "configId": "model", "value": "work"}}
```

and the reply is the full `configOptions` with the new `currentValue`. No
`config_option_update` follows: the spec keeps that for a change the agent
makes itself, and Abhed makes none. `models` and `session/set_model`
(`{"sessionId", "modelId"}`, reply `_meta["zybuu.ai/abhed"].currentModelId`)
are the earlier unstable form, for editors that predate config options.

The name is looked up in the configuration and nothing else, so an editor
cannot point the session at an endpoint. Offered are the providers a
trusted file defines: an untrusted workspace file adds none, and a built-in
provider nobody configured is not listed. A managed file that sets
`model.default` pins the model to it. A switch is refused while a prompt is
running, for a name not offered, and for a provider whose `api_key_env`
variable is unset, with the variable named. A switch is recorded as
`model.switched` before the new model answers, and the conversation is kept.
Subagents and background tasks started after a switch run on the new model,
unless their agent definition or the call names one; a task already running
keeps the model it started on.

Not yet supported: `session/load` (resuming an editor session from the
record) and editor-side modes. A conformance test drives the adapter with a
scripted client, so no editor is needed in CI.

## Server

```bash
abhed serve -addr :8420
```

A web console with an event stream, inline approvals and session history, plus a
JSON API. This is the multi-user path: local accounts or a reverse proxy's
identity headers, and with Postgres storage it enforces tenant isolation in the
database with row-level security. OIDC sign-in is part of the Enterprise
Edition.

```bash
B=http://127.0.0.1:8420
curl -s $B/v1/health
SID=$(curl -s -X POST $B/v1/sessions -d '{"prompt":"...","mode":"plan"}' | jq -r .session_id)
curl -sN $B/v1/sessions/$SID/events     # live
curl -s  $B/v1/sessions/$SID/replay     # the full audit trail
curl -s  $B/v1/sessions                 # your sessions, each with its state
curl -s  $B/v1/sessions/$SID/tasks      # its background tasks
curl -s -X POST $B/v1/sessions/$SID/tasks/<task_id>/cancel
curl -s -X POST $B/v1/sessions/$SID/wake -d '{"wake":"off"}'   # off, notify or auto, up to the server's
```

A session whose run has ended with background tasks still running is listed
as `background`, with their count in `background`; an approval waiting, run
or not, is in `pending_ask`. Its event stream stays open until the closing
end. These endpoints, like the session's approvals, answer only its owner.

A session's `state` in the list is `running`, `waiting_approval`, `idle` or
`done`. A `done` session also has a `reason`, how its last run ended as
recorded: `completed`, `user_interrupt`, `shutdown`, `deadline`, `stalled`,
`error`, `max_turns` and the rest.

Sending a message to a session that is already working **steers** it rather than
being refused.
