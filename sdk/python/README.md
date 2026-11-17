# abhed-sdk (Python)

A Python client for [Abhed](https://github.com/zybuu-ai/abhed), the secure
harness for deep agents. It drives `abhed rpc`, a child process speaking
line-delimited JSON on its standard input and output: no server, no port, and
no network call of its own. Policy, the sandbox, the record and the answer
checks stay the engine's; nothing here gets around them.

Requires Python 3.9 or later and an `abhed` binary (1.3.0 or later for
approvals) on `PATH` or named by `binary=`. No other dependencies.

```python
from abhed_sdk import Abhed, Decision, RunError, visible

def decide(req):
    # The arguments are the model's: show them through visible().
    print(f"{req.tool}: {visible(str(req.args))} ({req.reason})")
    if req.destructive or req.args_hidden:
        return Decision.deny()
    return Decision.allow_once(approver="olga@example.com")

with Abhed(cwd="/srv/work") as ab:
    session = ab.start(mode="default", verify="strict",
                       allow=["bash(go test*)"], on_approval=decide,
                       on_event=lambda ev: print(ev.type))
    try:
        answer = session.prompt("fix the failing tests; run go test ./...")
        print(answer.text, answer.verdict.outcome)
    except RunError as e:
        print("ended as", e.reason, e.detail)
```

| | |
|---|---|
| `Abhed(binary, cwd=, env=, trust_workspace=, args=, on_error=)` | start `abhed rpc`; `close()` quits it |
| `start(workspace=, mode=, allow=, deny=, verify=, wake=, runbook=, on_event=, on_approval=, approvals=, on_woken=)` | open a session |
| `Session.prompt(text)` | the `Answer` (`text`, `verdict`); a run that did not complete raises `RunError` with `reason`, `detail`, `answer`, `verdict` |
| `Session.stream(text)` | yield each `Event`, each `ApprovalRequest` when approvals have no handler, then the `Answer` |
| `Session.steer(text)` | `steered` or `queued` |
| `Session.approve(request_id, Decision)` | answer an approval from `stream` |
| `Session.usage()`, `export_html()`, `tasks()`, `cancel_task(id)`, `wake()`, `events()` | as the Go SDK's |
| `Abhed.providers()` | the provider types this build supports |
| `visible(text)`, `args_hidden(args)` | mark what would not print as itself, as the engine does; an approval's own `args_hidden` is the engine's verdict |

Without `on_approval` (or `approvals=True`), a call that needs approval is
refused, as in a headless run.

The full guide is
[TypeScript and Python SDKs](https://github.com/zybuu-ai/abhed/blob/main/docs/guide/40-sdk-typescript-python.md).

## Tests

```bash
python3 -m unittest discover -s tests            # against a stub abhed rpc
ABHED_BIN=/path/to/abhed python3 -m unittest discover -s tests   # and the real one
```

Licensed under the Apache License 2.0.
