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

The reason is structural. The policy engine evaluates hooks first so they can
veto — which means a hook returning "allow" would short-circuit the deny rules
beneath it. That is acceptable for code an operator compiled in; it is not
acceptable for a file an operator dropped into a directory. A permission gate an
extension can remove is not a guarantee.

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

Declaring no `events` subscribes to all of them. An empty reply `{}` means no
opinion.

A `tool_call` is not always one call. The workbench Explorer's New folder,
rename and delete arrive as `mkdir`, `rename` and `delete`, with a `path` (and
for a rename a `to`), not as `bash`. A rename is judged on both names, so it
reaches a hook twice, first with `path` the old name, then with `path` the
new one; a hook that keeps a count or a log should expect that.

## Configuration

```json
"extensions": [
  { "name": "guard", "command": "bash", "args": ["/opt/abhed/guard.sh"],
    "events": ["tool_call"], "timeout_ms": 5000 }
]
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

**A failing extension is skipped, not fatal.** One that crashes, hangs past its
timeout, or replies with something unparseable is marked dead and skipped; the
agent continues under policy alone. Failing the session would trade a working
agent for a broken one and protect nothing, since an extension could only ever
have made a decision stricter.

**A hung extension is not retried.** The read is abandoned but the stream is
not, so a later reply would be matched to the wrong request.

**The extension sees the whole call**, including fields the model wrote itself,
such as a bash `description`. Deliberate — an extension judging a command should
see the stated intent alongside it — but it means a naive whole-line match can
fire on a description rather than the command. Match the field you mean.

## Providing tools

See [Tools](05-tools.md).
