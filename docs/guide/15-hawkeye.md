# HawkEYE: what a session actually did

Abhed records every message, tool call, policy decision, tool result and
compaction as an event. HawkEYE reads that record and answers the questions a
transcript does not: where the tokens went, which policy step decided each
call, how full the context got, and what deserves a second look.

It is a pure function of the record. It calls no model, reaches no network and
reads nothing from the workspace, so a report can be produced on an air-gapped
machine from an exported file, long after the session ran.

## Three ways to run it

```bash
# Inside a session: print the summary, optionally write the full page
/hawkeye
/hawkeye report.html

# On an exported record, anywhere
/export session.json                     # in the session
abhed hawkeye session.json               # later, on any machine
abhed hawkeye -o report.html session.json

# On a headless run's event stream, json or stream-json
abhed -p "fix the tests" -output-format json > events.jsonl
abhed hawkeye events.jsonl

# On an export of the local record, checked against the head on its last line
/export session.jsonl
abhed hawkeye session.jsonl

# By id, in the local record (~/.abhed/records) or Postgres when configured
abhed hawkeye s-k4dq7x2m

# From the server
GET /v1/sessions/{id}/hawkeye              # JSON
GET /v1/sessions/{id}/hawkeye?format=html  # the page
```

`-o` writes `.html` by default and the structured report for a `.json` path.
The endpoint answers to the same ownership check as replay: another user or
tenant gets a 404, because a report carries every tool result in the session.

`abhed hawkeye` exits **3** when the record has a gap in it, or when a session
from the local record or a `.jsonl` export of it fails verification, so a
pipeline can refuse a record that is not whole.

### A stream-json capture

`-output-format stream-json` leaves out `agent.delta` and
`agent.reasoning.delta` unless `-include-partial-messages` is given, so its
sequence numbers skip where those were. Its result line says so, with
`"omitted":["agent.delta","agent.reasoning.delta"]`, and HawkEYE reads that:
a gap where only deltas could sit is reported as **agent.delta omitted by
stream-json**, an info finding, and is no reason to exit 3. Deltas stream while
the model answers and are recorded just before its `model.call`, after the
event that prompted it (the message, a tool's result, a denial, a background
result, a wake, a compaction or a model switch) and once every call asked for
has a result or a denial; and only a call that streamed something has deltas,
so the gap is excused only where that `model.call`'s turn recorded an
`agent.message` or `agent.reasoning`, or the call failed part way. Any other
gap is still `record-gap`, critical, such as a missing `observation` or
`agent.message`. A `user.message` or a retried `model.call` removed from right
before a `model.call` that streamed a reply cannot be told from omitted
deltas, since stream-json carries no per-event hash.

HawkEYE fails closed when it cannot tell. A capture that ends with a result
line naming nothing omitted and holds no deltas may be stream-json from an
older `abhed`, or a json capture of a model that streamed nothing; a gap of
the delta shape in it is reported as `record-gap`, critical, and says that
HawkEYE cannot tell omitted deltas from missing events.

An event removed from exactly where deltas sit cannot be told from them. When
a record must be checked whole, capture with `-output-format json` or
`-include-partial-messages`, or check the local record or a `.jsonl` export
of it, which is verified against its head.

The JSON report says the same under `integrity`: `omitted` lists the gaps
excused as omitted deltas, `omitted_types` the event types the result line
named, and `unsure` is `true` when a gap could be omitted deltas but the
capture does not say it left any out.

## What the report contains

| Section | What it tells you |
|---|---|
| Summary | outcome (how the session ended; `running` with no end recorded; `no agent run` for a session only people worked in, at the workbench's terminal or editor), the model each call went to (a switch shows as `a → b`), wall clock split between model and tools, tokens in and out, cache hit rate, peak context against the window |
| Findings | rules over the record, each naming its evidence by sequence number |
| Context per turn | what each turn sent to the model, how much of it was served from cache, where an offload moved results out to the record, where compaction cut, and how often the agent used `recall` to go back |
| Calls | every tool call followed through: arguments, decision, **the policy step that made it**, who let it through (and the remembered scope, if one did), duration, exit code, output. A call is marked run (✓) only when the record holds its result; one with none is marked not run (`-`), and one the sandbox refused part of is marked `!` |
| Turns | per-turn tokens, time to first token, total latency |
| Files | what the file tools read and wrote |
| Subagents | what was delegated, how it ended, what it cost |

The tokens in the summary and per turn are the conversation's own model
calls. A next-prompt suggestion's call (`model.call` with `purpose:
"suggestion"`) is left out, though the session's own totals and its budget
count it, so HawkEYE's total can be lower than the session's.

A turn stopped while the model was still replying counts the tokens its
provider had reported by then: Anthropic reports the prompt when the reply
starts, and some OpenAI-compatible servers report usage on every chunk. Most
OpenAI-compatible servers report it only at the end of a reply, so a turn
stopped part way there is recorded with no tokens; nothing is estimated.

The summary's tokens and model time include the calls made outside the
conversation, such as next-prompt suggestions, as the session's own totals
do. They are not turns, so the Turns table leaves them out.

The policy step is one of `hook`, `deny`, `destructive`, `screen`, `ask`, `mode`,
`allow` or `default` — the stage of the [evaluation order](04-permissions.md#the-order) that
decided. Counting them shows which part of a policy is doing the work: a
session where everything lands on `default` has rules that never match.

## Findings

Findings are deterministic. The same record always produces the same findings,
and none of them is a model's opinion.

| Code | Severity | Fires when |
|---|---|---|
| `record-gap` | critical | the event sequence skips — the record was filtered, truncated or edited |
| `stream-omitted` | info | a stream-json capture skips only where the deltas it says it left out sit |
| `borrowed-host` | warn | an allowed call names a host the user never mentioned and that first appeared in tool output |
| `sensitive-path` | warn / info | a call reached, or was stopped from reaching, a credential path |
| `broken-edit` | info | an `edit` or `write` was refused, because it would have left a file that parsed no longer parsing or its new text was a pasted diff; the file was left as it was |
| `output-cap` | warn | a turn spent the whole output budget reasoning and made no tool call; the loop nudged it and lowered the effort |
| `secret-redacted` | warn | a stored secret's value was written out by a command or the model and redacted before the record |
| `abnormal-end` | warn | the session ended as anything other than completed or a user interrupt |
| `repeated-failure` | warn | the same call failed three times unchanged |
| `context-pressure` | warn | a turn used 85% or more of the window |
| `subagent-destructive` | warn | a subagent was allowed a destructive command; the detail says who allowed it and names the subagent's session |
| `denied` | info | a call was refused; the detail says by whom, from the event's `by` (policy, reviewer, the person, headless, or system for a request that ended unanswered), and names the `approver` where the record does |
| `subagent-denied` | info | a subagent's call was refused, from the parent's `subagent.action`; the detail says by whom and names the subagent's session |
| `sandbox-denied` | info | a `bash` command ran under a sandbox tier (its observation's `sandbox` is not `none`) but the sandbox refused an operation in it, whatever its exit status says; records without the tier do not raise it |
| `truncated` | info | tool results were cut before the model saw them |
| `slow-tool` | info | a tool call ran longer than a minute |
| `cold-cache` | info | under 20% of the prompt was cached across five or more turns whose provider reported a cache figure |
| `no-end` | info | the record has no terminal event |

Calls a person made by hand in the workbench (`actor: user`: saves, commands,
shells, Explorer operations) are in the report as theirs, with the `actor`
field on each call. They count in the totals and the policy figures and can
raise `sensitive-path`, `denied` and `secret-redacted`, which are about what
was reached, refused or exposed whoever did it. They never raise the findings
about the model's behaviour: `repeated-failure`, `slow-tool`, `truncated` and
`borrowed-host`. `no-end` is not raised for a session still running on the
server that makes the report. The command line reads only the record, which
cannot tell a shell still open from a server that stopped with one open, so
there `no-end` is still raised and says a shell was open.

`borrowed-host` is the one worth understanding. All tool output is untrusted,
and an injected instruction usually has to name somewhere to send things. A
call to a host that only tool output supplied is that shape — and it is also
the shape of following a link in a README, which is why it is a warning to
read and not a verdict.

## What it does not do

- **It does not prove the record is complete.** A gap proves something was
  removed; the absence of a gap proves nothing was removed from the *middle*.
  The guarantee that events cannot be deleted at all comes from the store —
  append-only triggers and forced row-level security in Postgres — not from
  this report.
- **It does not detect prompt injection.** It flags one mechanical shape
  injection tends to take. The boundary is still the sandbox and the policy.
- **It does not see inside `bash`.** File access is counted from the file
  tools' arguments; guessing paths out of a shell command would report things
  that did not happen.
- **Records written by v0.2.0 or earlier have no per-turn events.** The summary falls
  back to the session's own totals and the context chart is omitted.

## The page is safe to open

The report embeds tool output. It is rendered through `html/template`, which
escapes by context, and the server sends `Content-Security-Policy:
default-src 'none'`, so a result containing markup arrives as text. The page
loads nothing from anywhere.
