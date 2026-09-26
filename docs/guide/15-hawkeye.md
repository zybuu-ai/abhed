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

# By id, against a durable store
abhed hawkeye s-k4dq7x2m

# From the server
GET /v1/sessions/{id}/hawkeye              # JSON
GET /v1/sessions/{id}/hawkeye?format=html  # the page
```

`-o` writes `.html` by default and the structured report for a `.json` path.
The endpoint answers to the same ownership check as replay: another user or
tenant gets a 404, because a report carries every tool result in the session.

`abhed hawkeye` exits **3** when the record has a gap in it, so a pipeline can
refuse a record that is not whole.

## What the report contains

| Section | What it tells you |
|---|---|
| Summary | outcome, wall clock split between model and tools, tokens in and out, cache hit rate, peak context against the window |
| Findings | rules over the record, each naming its evidence by sequence number |
| Context per turn | what each turn sent to the model, how much of it was served from cache, where an offload moved results out to the record, where compaction cut, and how often the agent used `recall` to go back |
| Calls | every tool call followed through: arguments, decision, **the policy step that made it**, who let it through (and the remembered scope, if one did), duration, exit code, output. A call is marked run (✓) only when the record holds its result; one with none is marked not run (`-`), and one the sandbox refused part of is marked `!` |
| Turns | per-turn tokens, time to first token, total latency |
| Files | what the file tools read and wrote |
| Subagents | what was delegated, how it ended, what it cost |

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
| `borrowed-host` | warn | an allowed call names a host the user never mentioned and that first appeared in tool output |
| `sensitive-path` | warn / info | a call reached, or was stopped from reaching, a credential path |
| `broken-edit` | info | an `edit` or `write` was refused, because it would have left a file that parsed no longer parsing or its new text was a pasted diff; the file was left as it was |
| `output-cap` | warn | a turn spent the whole output budget reasoning and made no tool call; the loop nudged it and lowered the effort |
| `secret-redacted` | warn | a stored secret's value was written out by a command or the model and redacted before the record |
| `abnormal-end` | warn | the session ended as anything other than completed or a user interrupt |
| `repeated-failure` | warn | the same call failed three times unchanged |
| `context-pressure` | warn | a turn used 85% or more of the window |
| `denied` | info | a call was refused; the detail says by whom, from the event's `by` (policy, reviewer, the person, headless, or system for a request that ended unanswered) |
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
