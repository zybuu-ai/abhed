# TypeScript and Python SDKs

Two thin clients over [`abhed rpc`](10-automation.md#rpc), for a program that
is not written in Go. Each starts `abhed rpc` as a child process and speaks
its line-delimited JSON on the child's standard input and output: there is
no server, no port, and no network call of the SDK's own. The engine is the
same one the command line runs, so policy, the sandbox, the record and the
answer checks are exactly as from the terminal; neither SDK adds a way
around them.

| | Package | Source |
|---|---|---|
| TypeScript (Node.js 18+) | `abhed-sdk` | `sdk/typescript` |
| Python (3.9+) | `abhed-sdk`, imported as `abhed_sdk` | `sdk/python` |

Neither has a runtime dependency. Both need an `abhed` binary; approvals need
1.3.0 or later.

```ts
import { Abhed, Decision, RunError, visible } from "abhed-sdk";

const ab = Abhed.spawn({ cwd: "/srv/work" });
try {
  const s = await ab.start({
    verify: "strict",
    allow: ["bash(go test*)"],
    onEvent: (ev) => console.log(ev.seq, ev.type),
    onApproval: async (req) =>
      req.destructive || req.args_hidden ? Decision.deny() : askThePerson(req),
  });
  const answer = await s.prompt("fix the failing tests; run go test ./...");
  console.log(answer.text, answer.verdict?.outcome);
} catch (e) {
  if (e instanceof RunError && e.unverified) console.log("not verified:", e.verdict?.claims);
  else throw e;
} finally {
  await ab.close();
}
```

```python
from abhed_sdk import Abhed, Decision, RunError

with Abhed(cwd="/srv/work") as ab:
    s = ab.start(verify="strict", allow=["bash(go test*)"],
                 on_event=lambda ev: print(ev.seq, ev.type),
                 on_approval=lambda req: Decision.deny() if req.destructive else ask_the_person(req))
    try:
        answer = s.prompt("fix the failing tests; run go test ./...")
        print(answer.text, answer.verdict.outcome)
    except RunError as e:
        print("ended as", e.reason, e.detail)
```

## Surface

The same as the [Go SDK](09-sdk.md)'s for a session, named for each language.

| Go SDK | TypeScript | Python | `rpc` |
|---|---|---|---|
| `New` | `Abhed.spawn(...)`, `start(...)` | `Abhed(...)`, `start(...)` | `start` |
| `Run`, `Continue` | `prompt(text)` | `prompt(text)` | `prompt` |
| `OnEvent` | `onEvent`, `stream(text)` | `on_event`, `stream(text)` | event lines |
| `Steer`, `Queued` | `steer(text)` → `steered` or `queued` | `steer(text)` | `steer` |
| `Approve`, `NoteAnswer` | `onApproval` → `Decision`, or `approve(id, decision)` | `on_approval` → `Decision`, or `approve(id, decision)` | approval lines, `approve` |
| `Verdict`, `EndedError` | `Answer.verdict`, `RunError` | `Answer.verdict`, `RunError` | `verdict` on the answer or error |
| `Usage` | `usage()` | `usage()` | `usage` |
| `ExportHTML` | `exportHtml()` | `export_html()` | `export` |
| `Background`, `CancelTask`, `Wake` | `tasks()`, `cancelTask(id)`, `wake()`, `onWoken` | `tasks()`, `cancel_task(id)`, `wake()`, `on_woken` | `tasks`, `cancel_task`, `wake`, woken lines |
| `Events` | `events()` | `events()` | |
| `Providers` | `providers()` | `providers()` | `providers` |
| `Visible`, `ArgsHidden` | `visible()`, `argsHidden()` | `visible()`, `args_hidden()` | |

`start` takes what the `rpc` `start` takes: `workspace`, `mode`, `allow`,
`deny`, `verify` (`off`, `label`, `strict`), `wake` (`off`, `notify`,
`auto`) and `runbook`. A process holds one session at a time; a second
`start` closes the first, and the first's methods then fail.

What the Go SDK has that `rpc` does not carry is not in these SDKs either:
`Fork`, `SetModel` and `SwitchModelNamed`, `RunJSON`, `AttachImages`, and
building the agent from options in-process (`Provider`, `Extensions`,
`SystemPrompt`). The model is the one the configuration names; use the
workspace's or your own `~/.abhed/config.json`, or the managed file.

## Approvals

Without a handler, a call that needs approval is refused, as in a headless
run: an embedded agent with nobody to ask is stricter, never looser. With
`onApproval` (`on_approval`), each one is put to your function with the
request as the record holds it ([Approvals](10-automation.md#approvals)):
`tool`, `args`, `reason`, the policy `step` and `rule`, `offer` (the scope a
person may always allow), `destructive`, `args_hidden`, and the `subagent` or
background `task_id` that asked. It returns a `Decision`:

| | |
|---|---|
| `allowOnce(approver)` / `allow_once(approver)` | allow this call |
| `allowAlways(approver)` / `allow_always(approver)` | allow it and keep `offer` for the session: later calls it covers are allowed without asking, recorded as `by: session-scope`. Ignored when nothing is offered |
| `deny(approver)` | refuse it |

`approver` is recorded on the approval or refusal as your program asserts
it; Abhed does not verify it, so authenticate the person before you name
them. A handler that throws refuses the call. Each ask is handled
concurrently, so events keep streaming while a person decides.

With `approvals: true` and no handler, the asks are yielded by `stream()`
instead, to answer with `approve(requestId, decision)`. An ask nobody can
answer is refused rather than left waiting, and so is one still open when
the process's input ends. One open when its run is stopped ends as
interrupted, and an answer that arrives with the stop is not applied.

**Show arguments through `visible()`.** A tool call's arguments are the
model's, unescaped: a value can hold a right-to-left override, a terminal
escape or a zero-width character that makes it read as something else.
`visible()` shows each such character as `\u{XXXX}`; it marks more than the
engine's own check (`args_hidden`), never less. Consider refusing outright
when `args_hidden` is set.

## Verdicts and errors

`prompt` resolves to an `Answer`: `text`, and `verdict` when answers are
checked (`outcome`, `verified`, `unverified`, `contradicted`, `claims`,
`note`). A run that ended otherwise than completed fails with a `RunError`:
`reason` is the terminal reason (`unverified` under `strict`, `max_turns`,
`max_budget`, `prompt_refused`, ...), `detail` what followed it, and
`answer` and `verdict` what the run produced, which may be set although it
failed. Any other error line is a `RunError` with no `reason`. A process
that exits, or never starts, fails every request waiting with
`ProcessExited`, carrying its exit code and the last 64 KiB of its stderr.

An error line no request owns (a queued steer that was never delivered) goes
to `onError` (`on_error`).

## Security

- The SDK passes the child its environment: this process's by default, or
  `env`. The model's key reaches the engine from there, never through the
  protocol. Commands the agent runs in a sandbox tier get an environment of
  their own.
- `trustWorkspace` (`trust_workspace`) passes `-trust-workspace`, trusting
  the workspace's `.abhed/config.json`. Leave it off for a checkout you did
  not write; then only what tightens applies, and `start`'s
  `workspace_trust` says what was ignored.
- `close()` sends `quit`, closes stdin and waits; after a timeout it sends
  SIGTERM, which ends the running prompt and records its end, then SIGKILL.
  It signals only the child it started.

## Testing

Each SDK is tested against a stub `abhed rpc` that speaks the protocol with
scripted answers, and, with `ABHED_BIN` naming a built binary, against the
real engine on a scripted model served from 127.0.0.1, with a temporary
`HOME`:

```bash
cd sdk/typescript && npm install && npm test
cd sdk/python && python3 -m unittest discover -s tests
ABHED_BIN=$PWD/abhed npm test     # and the same for Python
```

## Packages

Both packages are ready to publish (`npm pack`, `python -m build`) under the
Apache License 2.0, versioned with Abhed, and are not published yet. Until
they are, install from the repository: run `npm run build` in
`sdk/typescript` and `npm install` that folder, or `pip install ./sdk/python`.
