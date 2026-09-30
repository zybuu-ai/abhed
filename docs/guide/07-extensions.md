# Extensions

An extension changes how the agent behaves without forking it. Extensions are
separate processes speaking line-delimited JSON on stdin and stdout, so they can
be written in any language and need no build step in Abhed.

## The one rule

**An extension may veto, never permit.**

It can block a call, force it to an approval prompt, rewrite arguments before a
tool runs, rewrite a result before the model reads it, drop messages before they
go upstream, supply a compaction summary, and add tools. It cannot turn a denied
action into an allowed one.

The reason is structural. The policy engine asks hooks first so they can veto.
A hook's refusal is final there; its ask waits until the deny rules and plan
mode have had their say, so it can never turn a refusal into a question; and
an "allow" is no opinion at all. A permission gate an extension could remove
would not be a guarantee.

## Protocol

One JSON object per line each way. Abhed writes a request; the extension writes
exactly one reply.

```jsonc
// Abhed → extension
{"event":"tool_call","session_id":"s-1","tool":"bash","args":{"command":"rm -rf /tmp/x"}}

// extension → Abhed
{"block":true,"reason":"rm -rf is not permitted here"}
```

### Events

| Event | When | The reply may |
|---|---|---|
| `tool_call` | before a tool runs | `block`, `ask`, rewrite `args` |
| `tool_result` | before the model sees output | rewrite `content`, set `is_error` |
| `context` | before each model call | `keep` a subset of messages, by index |
| `before_agent_start` | once per run | append to `system` |
| `before_compact` | before history is summarized | `cancel`, or supply the `summary` |
| `list_tools` | once at startup | declare `tools` this extension provides |
| `invoke_tool` | the model called one | return its `result` |
| `session_start`, `session_end` | run boundaries | nothing; setup and teardown |
| `user_prompt_submit` | before a message you send is recorded and sent (interactive CLI) | `block` it; you are told why and the model never sees it. A steering message typed during a run that is blocked is recorded as dropped |
| `permission_request` | before a call is put to you, a subagent's included (interactive CLI) | `block` it. Nothing in the reply approves the call; `allow` is ignored |
| `turn_end` | the agent has finished answering, with the reason it stopped (interactive CLI) | nothing; it only observes |
| `subagent_end` | a subagent the agent waited on has returned (interactive CLI) | nothing; it only observes |
| `notification` | the agent needs you, as when a call waits for approval (interactive CLI) | nothing; it only observes |

Declaring no `events` subscribes to the events up to `session_end` above; the
five after it are sent only to an extension that names them. An empty reply
`{}` means no opinion.

Each hook that blocks, forces an ask or answers with a `reason` or `log` is
recorded as `hook.fired`, with the extension, the event and its verdict:
`block`, `ask` or `annotate`. There is no `allow`. `/hooks` in the CLI lists
the configured extensions with the layer each came from, the events it takes,
its matcher and whether it is running, or why it stopped.

A `tool_call` is not always one call. The workbench Explorer's New folder,
rename and delete arrive as `mkdir`, `rename` and `delete`, with a `path` (and
for a rename a `to`), not as `bash`. A rename is judged on both names, so it
reaches a hook twice, first with `path` the old name, then with `path` the
new one; a hook that keeps a count or a log should expect that.

## Configuration

```json
"extensions": [
  { "name": "guard", "command": "bash", "args": ["/opt/abhed/guard.sh"],
    "events": ["tool_call"], "timeout_ms": 5000 },
  { "name": "git-guard", "command": "/opt/abhed/git-guard",
    "events": ["tool_call", "permission_request"], "match": ["bash(git *)"] },
  { "name": "notify", "command": "/opt/abhed/notify",
    "events": ["notification", "turn_end"], "async": true }
]
```

- `match` narrows `tool_call` and `permission_request` to the calls a rule
  matches, written as [permission rules](04-permissions.md) are and matched as
  a deny rule is: each part of a chained command, a path relative to the
  workspace or absolute, and in any Unicode normal form. A call that cannot be
  read that far is sent. With no `match`, every call reaches the extension.
- `async` sends the events that only observe (`turn_end`, `subagent_end`,
  `notification`, `session_start`, `session_end`) without waiting for a reply.
- A workspace's extensions start only once the workspace is trusted.
- `hooks.disabled`, which only the managed configuration sets, turns hooks
  off: each extension keeps only the tools it provides, is sent no hook
  event, and one that provides no tools is not started.

## Worked examples

Block reads of anything matching a pattern:

```bash
#!/bin/bash
while IFS= read -r line; do
  tool=$(jq -r '.tool // ""' <<<"$line")
  path=$(jq -r '.args.path // ""' <<<"$line")
  if [[ "$tool" == "read" && "$path" == *secret* ]]; then
    echo '{"block":true,"reason":"secrets are off limits"}'
  else
    echo '{}'
  fi
done
```

Redact credentials from every tool result:

```bash
#!/bin/bash
while IFS= read -r line; do
  if [[ $(jq -r '.event' <<<"$line") == "tool_result" ]]; then
    jq -c '{content: (.content | gsub("AKIA[A-Z0-9]{16}"; "[redacted]"))}' <<<"$line"
  else
    echo '{}'
  fi
done
```

Keep something the summarizer would drop:

```bash
#!/bin/bash
while IFS= read -r line; do
  if [[ $(jq -r '.event' <<<"$line") == "before_compact" ]]; then
    echo '{"summary":"Working on ticket ABC-123. Keep the ticket id."}'
  else
    echo '{}'
  fi
done
```

## Behaviour that is deliberate

**They combine toward the stricter answer.** Where two disagree, the blocking
one wins; where one asks for approval and another is silent, the call is asked.
Load order cannot change a verdict, which keeps the audit trail reproducible.

**A failing extension fails closed, and is not fatal.** One that crashes, hangs
past its timeout, or replies with something unparseable is marked dead. On a
`tool_call` or `permission_request`, the call it failed on is refused, and
while it is not running every call it would have screened is asked, since the
veto it stood for is gone. For the other events it is skipped: a
`user_prompt_submit` hook that has stopped fails open, and the CLI says each
message went unscreened. The session goes on either way.

**A hung extension is not retried.** The read is abandoned but the stream is
not, so a later reply would be matched to the wrong request.

**The extension sees the whole call**, including fields the model wrote itself,
such as a bash `description`. Deliberate — an extension judging a command should
see the stated intent alongside it — but it means a naive whole-line match can
fire on a description rather than the command. Match the field you mean.

**On a server, one process serves every session.** `abhed serve` starts each
configured extension once. It is asked about every user's calls, in every
tenant the server serves, and is given the conversation when a session
compacts, one request at a time. An extension that must not see one user's
work alongside another's belongs on a server of its own. The serve banner
lists the extensions and names any that is not running, and
`/v1/capabilities` gives each one's `status`: `running`, `stopped` (it
crashed, hung or was closed) or `not started`. A stopped `tool_call`
extension's calls are asked, as above.

## Providing tools

See [Tools](05-tools.md).
