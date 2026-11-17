# abhed-sdk (TypeScript)

A TypeScript client for [Abhed](https://github.com/zybuu-ai/abhed), the secure
harness for deep agents. It drives `abhed rpc`, a child process speaking
line-delimited JSON on its standard input and output: no server, no port, and
no network call of its own. Policy, the sandbox, the record and the answer
checks stay the engine's; nothing here gets around them.

Requires Node.js 18 or later and an `abhed` binary (1.3.0 or later for
approvals) on `PATH` or named by `binary`. No runtime dependencies.

```ts
import { Abhed, Decision, RunError, visible } from "abhed-sdk";

const ab = Abhed.spawn({ cwd: "/srv/work" });
try {
  const session = await ab.start({
    mode: "default",
    verify: "strict",
    allow: ["bash(go test*)"],
    onEvent: (ev) => console.log(ev.type),
    onApproval: async (req) => {
      // The arguments are the model's: show them through visible().
      console.log(`${req.tool}: ${visible(JSON.stringify(req.args))} (${req.reason})`);
      if (req.destructive || req.args_hidden) return Decision.deny();
      return Decision.allowOnce("olga@example.com");
    },
  });
  const answer = await session.prompt("fix the failing tests; run go test ./...");
  console.log(answer.text, answer.verdict?.outcome);
} catch (e) {
  if (e instanceof RunError) console.log("ended as", e.reason, e.detail);
  else throw e;
} finally {
  await ab.close();
}
```

| | |
|---|---|
| `Abhed.spawn({ binary, cwd, env, trustWorkspace, args, onError })` | start `abhed rpc`; `close()` quits it |
| `start({ workspace, mode, allow, deny, verify, wake, runbook, onEvent, onApproval, approvals, onWoken })` | open a session |
| `session.prompt(text)` | the `Answer` (`text`, `verdict`); a run that did not complete rejects with `RunError` (`reason`, `detail`, `answer`, `verdict`) |
| `session.stream(text)` | async iterate `{kind: "event"}`, `{kind: "approval"}` when approvals have no handler, then `{kind: "answer"}` |
| `session.steer(text)` | `"steered"` or `"queued"` |
| `session.approve(requestId, decision)` | answer an approval from `stream` |
| `session.usage()`, `exportHtml()`, `tasks()`, `cancelTask(id)`, `wake()`, `events()` | as the Go SDK's |
| `ab.providers()` | the provider types this build supports |
| `visible(text)`, `argsHidden(args)` | mark what would not print as itself, as the engine does; an approval's own `args_hidden` is the engine's verdict |

Without `onApproval` (or `approvals: true`), a call that needs approval is
refused, as in a headless run.

The full guide is
[TypeScript and Python SDKs](https://github.com/zybuu-ai/abhed/blob/main/docs/guide/40-sdk-typescript-python.md).

## Tests

```bash
npm install && npm test                      # against a stub abhed rpc
ABHED_BIN=/path/to/abhed npm test            # and the real one
```

Licensed under the Apache License 2.0.
