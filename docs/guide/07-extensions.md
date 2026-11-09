# Extensions

An extension changes how the agent behaves without forking it. Extensions are
separate processes speaking line-delimited JSON on stdin and stdout, so they can
be written in any language and need no build step in Abhed.

For a check that needs no long-lived process, a [simple hook](#simple-hooks)
runs one command, or POSTs to one endpoint, per event. It takes the same
events and follows the same rule.

## The one rule

**An extension may veto, never permit.**

It can block a call, force it to an approval prompt, rewrite a result before the model reads it, drop messages before they
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
| `tool_call` | before a tool runs | `block`, `ask` (an `args` in the reply is ignored) |
| `tool_result` | after a tool runs, before the model sees its output, with secrets already redacted | rewrite `content`, set `is_error` (recorded as `annotate`, with the size and SHA-256 of what the tool returned; a stored secret's value written back in is redacted again) |
| `context` | before each model call | `keep` a subset of messages, by index |
| `before_agent_start` | once per run | append to `system` |
| `before_compact` | before history is summarized | `cancel`, or supply the `summary` |
| `list_tools` | once at startup | declare `tools` this extension provides |
| `invoke_tool` | the model called one | return its `result` |
| `session_start`, `session_end` | run boundaries. **Not sent yet**: an extension subscribed to them is never told | nothing |
| `user_prompt_submit` | before a message you send is recorded and sent | `block` it; the model never sees it, and it is recorded as `message.dropped` with the reason. You are told why: in the terminal and the workbench, on stderr for `-p`, `abhed rpc` and `abhed acp` (rpc's and acp's answers say `prompt_refused` too), and in the SDK as an `*EndedError` whose `Reason` is `prompt_refused` and `Detail` the reason. A steering message typed during a run that is blocked is recorded as dropped the same way |
| `permission_request` | before a call is put to you, a subagent's included | `block` it. Nothing in the reply approves the call; `allow` is ignored |
| `turn_end` | the agent has finished answering, with the reason it stopped | nothing; it only observes |
| `subagent_start` | the agent starts a subagent; `tool` is `task` or `tasks`, `content` its arguments | nothing; it only observes |
| `subagent_end` | a subagent the agent waited on has returned (the subagent's stop) | nothing; it only observes |
| `notification` | the agent needs you, as when a call waits for approval | nothing; it only observes |
| `tool_failure` | a tool ran and failed, a subagent's included; `content` is its output, with secrets redacted | nothing; it only observes |
| `post_compact` | history was summarized; `content` is `auto` or `manual` | nothing; it only observes |
| `stop` | the agent is about to finish answering; `content` is its answer | `block` with a `reason`: the agent is sent back to work with the reason as its instruction, at most 3 times a run |

Declaring no `events` subscribes to the events up to `session_end` above; the
ones after it are sent only to an extension that names them. An empty reply
`{}` means no opinion.

`stop` is asked after the answer has passed [verification](29-verified-answers.md),
and only of the main agent: a subagent's end is `subagent_end`. A `stop` hook
that fails, or answers nothing, lets the run end. Its instruction reaches the
model as a message, with any stored secret's value replaced by
`[secret:NAME]`, and is recorded as `hook.continued` with that exact text, so
a resumed or forked session reads it too. The calls a continued run makes are
judged by policy as any other; a hook that asks to continue grants nothing.
The run also ends when it reaches its turn or budget limit, however often a
hook asks.

Any event's reply may end the run: `"continue": false`, with a `stopReason`.
The call, permission request or message it was about does not go ahead, nor
does any other call left in that turn, and the run ends before the model is
called again, as `policy_denied`, with the reason as the end's detail (exit
code 4 for `-p`). A `stop` hook's `continue: false` ends it the same way. A
halt raised on a subagent's call ends the whole session tree: the subagent,
and the run that started it. `"continue": true`, or no
`continue`, is no opinion, and only a `stop` hook can send the agent on.

Replies written for other agent harnesses mean the same here:

| Reply | Means |
|---|---|
| `"decision": "block"` with a `reason` | `block`: refuses the call, or on `stop` sends the agent on |
| `"continue": false` with a `stopReason` | ends the run |
| `"continue": true` | no opinion |
| an approving decision | ignored: nothing a hook says approves |

Each hook that blocks, forces an ask, asks the agent to continue, ends the run
or answers with a `reason` or `log` is recorded as `hook.fired`, with the
extension, the event and its verdict: `block`, `ask`, `continue`, `halt` or
`annotate`. There is no `allow`. A hook on a call the agent makes is given
that session's id in `session_id`, as its other events are. `/hooks` in the CLI lists the configured extensions and simple hooks
with the layer each came from, the events it takes, its matcher and whether it
is running, or why it stopped.

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
- `env` sets variables for the process. An extension does not inherit
  Abhed's environment, which holds `ABHED_API_KEY` and model provider keys:
  it gets `PATH`, `HOME`, `USER`, `LOGNAME`, the locale, `TMPDIR` and `TZ`
  (on Windows also what a program needs to start), then its `env`, which
  wins. A variable given an empty value, `"GITHUB_TOKEN": ""`, passes your
  own value of it. Before 1.2.8 an extension inherited everything.
- `async` sends the events that only observe (`turn_end`, `subagent_start`,
  `subagent_end`, `notification`, `tool_failure`, `post_compact`) without
  waiting for a reply.
- A workspace's extensions start only once the workspace is trusted.
- `hooks.disabled`, which only the managed configuration sets, turns hooks
  off: each extension keeps only the tools it provides, is sent no hook
  event, and one that provides no tools is not started.
- `hooks.managed_only`, which only the managed configuration sets, does the
  same for every extension the managed file did not configure, including
  those an SDK program passes: only the organisation's extensions take hook
  events. `/hooks` marks the others "tools only".

## Simple hooks

A simple hook is a command, or an HTTP endpoint, run once for each event it
takes. It needs no protocol loop: it reads one event and exits.

```json
"hooks": {
  "run": [
    { "name": "no-push", "events": ["tool_call"], "match": ["bash(git push*)"],
      "command": "echo 'pushes go through review' >&2; exit 2" },
    { "name": "tests-first", "events": ["stop"], "command": "/opt/abhed/hooks/stop-check.sh",
      "timeout_ms": 60000 },
    { "name": "audit", "events": ["tool_failure", "turn_end"], "async": true,
      "url": "https://audit.example.com/abhed", "headers_env": {"Authorization": "AUDIT_TOKEN"} }
  ]
}
```

**A command hook** gets the event on stdin, as the same JSON an extension is
sent, and its exit status decides:

| Exit | Means |
|---|---|
| `0` | go on. It may print a JSON reply on stdout; only `block`, `ask`, `decision`, `continue`, `stopReason`, `reason` and `log` are read. Plain text is ignored |
| `2` | block, with stderr as the reason. On `stop`, continue, with stderr as the instruction |
| anything else, or no exit within the timeout | the hook failed |

It runs as the agent's own commands do: in the session's sandbox, at its tier,
in the folder the session works in (its worktree, when it has one), with the
same environment. What the sandbox hides from a command it hides from a hook,
and no stored secret is passed to one. The timeout is `timeout_ms`; unset, it
is 10 seconds, or 2 seconds on an event that only observes. `async` runs the
events that only observe without waiting for the hook: at most 4 runs of one
hook at once, a run beyond that skipped and recorded as `error`, and those
still going cancelled when the session ends. What a hook prints to stderr or
as `log` is written to Abhed's log with stored secrets redacted.

**Keep a veto's script out of the workspace.** The agent can change the files
in the workspace, so a `tool_call` or `permission_request` hook whose script
lives there can be rewritten by the calls it screens. Put it on a path the
sandbox keeps read-only, such as `/opt/abhed/hooks`, and name it by its
absolute path.

**An HTTP hook** is POSTed the event as JSON, with `headers`, and with each
header in `headers_env` set to the value of the environment variable it names
(a variable that is not set fails the hook), so a token need not be written
into the file. Headers are sent only over `https`, or plain `http` to this
machine; a hook that sets them for plain `http` elsewhere stops Abhed loading. A `2xx` answer goes on, and its body, if any, is read as the
same JSON reply; any other status, a redirect, or no answer within the timeout
is a failure. The request is one of Abhed's own, so under
`sandbox.network: allowlist` it goes only where an egress rule allows, judged
as kind `hook`; an endpoint not on the allowlist is a failure.

A simple hook follows the extension's rule and fails the same way: it can
block a call or ask for it, or end the run, never approve one; a `tool_call`
or `permission_request` hook that fails refuses the call it failed on; a
`user_prompt_submit` hook that fails lets the message through, and you are
told it was not screened; for the other events a failure is skipped. It can
take `tool_call`, `permission_request`, `user_prompt_submit`, `stop`,
`tool_result` (to see it; a simple hook rewrites nothing), `tool_failure`,
`turn_end`, `subagent_start`, `subagent_end`, `post_compact` and
`notification`. It sees a result, a failure's output and a stop's answer with
stored secrets already redacted.

Every run is recorded as `hook.fired`, whatever came of it, with `kind`
(`command` or `http`), the `layer` the hook came from, the `reason`, the
`exit_code` and `duration_ms`. A run with no objection has the verdict
`none`, and one that failed `error`; neither is drawn in the terminal's
activity. Name each hook uniquely, apart from the extensions too.

**Layers.** Each layer adds to `hooks.run` rather than replacing it: the user's
file, then a trusted workspace's, then the managed configuration's. Only the
managed configuration replaces a hook by name: a later file's hook with a name
an earlier layer already gave is set aside, named in the warnings and recorded
as `config.refused`, and the earlier hook stands. So a workspace cannot swap
your own veto for something else, and no lower layer can remove or change a
managed hook. A workspace's hooks run only once the workspace is trusted; an
untrusted file's are set aside and named. The org policy never sets them.
`hooks.disabled` turns every simple hook off, and `hooks.managed_only` leaves
only the managed file's. A hook that could not run as written (no name, a name
taken twice in one file or by an extension, both or neither of `command` and
`url`, an event it cannot take, a `match` that does not parse) stops Abhed
loading, rather than leaving a veto silently missing.

`abhed serve` runs each command hook under the server's own sandbox and
workspace, not in a session's own sandbox where the edition gives sessions
one, and says so in a warning at start-up when both are on. It records each
run in the session it ran for. Where a session's sandbox is on another
machine (a remote driver), its folder is not on the server's, so a command
hook cannot start there: a `tool_call` or `permission_request` command hook
then fails, and every call it screens is refused. Use an HTTP hook for
those sessions.

### Simple hook examples

Refuse writes outside `src/`, reading the call from stdin:

```bash
#!/bin/bash
# /opt/abhed/hooks/src-only.sh, for "events": ["tool_call"]
ev=$(cat)
tool=$(jq -r '.tool' <<<"$ev")
path=$(jq -r '.args.path // ""' <<<"$ev")
if [[ "$tool" == "write" || "$tool" == "edit" ]] && [[ "$path" != */src/* ]]; then
  echo "only files under src/ may change" >&2
  exit 2
fi
```

Send the agent back until the tests pass, which `stop` asks at most 3 times a run:

```bash
#!/bin/bash
# /opt/abhed/hooks/stop-check.sh, for "events": ["stop"]
cat >/dev/null
if ! out=$(make test 2>&1); then
  echo "the tests fail; fix them before you finish:" >&2
  tail -n 20 <<<"$out" >&2
  exit 2
fi
```

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
`user_prompt_submit` hook that has stopped fails open, and each message is
said to have gone unscreened: in the terminal, on stderr for `-p`, `abhed rpc`
and `abhed acp` (and to an SDK caller's `Warn`), and in `abhed serve`'s log.
These hooks run on every surface that loads extensions: the terminal, `-p`,
`abhed rpc`, `abhed acp`, `abhed serve` and the SDK; before 1.2.8 only the
terminal asked them. The session goes on either way.

**One decision's policy hooks have a deadline.** A `tool_call` hook must
decide within 30 seconds of the decision, or by the deadline of the request
it serves if that comes first, such as an Explorer rename or delete judging
each file it touches. A call whose hooks have not all decided by then is
refused, and once that deadline has passed the rest of the batch is refused
without asking the hooks again, so a slow extension cannot hold the
workbench for the length of a large folder. The bound is kept by hooks given
the decision's context, as extensions' are (`Engine.ContextHooks` in the Go
API). A Go program's own `Engine.Hooks` or `Engine.EngineHooks` get no
context: one that returns after the deadline has its allow refused, but one
that never returns holds the decision, so give a slow hook a context.

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
crashed, hung or was closed) or `not started`. Where people sign in, only an
administrator is given `status`, since it says when a veto is down; others
see each extension's name and events. A stopped `tool_call` extension's
calls are asked, as above.

## Providing tools

See [Tools](05-tools.md).
