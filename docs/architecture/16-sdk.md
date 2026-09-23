# Embedding Abhed

Status: 2026-09-06

Everything Abhed does lives under `internal/`, which Go refuses to let another
module import. That is right for a binary and a wall for anyone who wants the
agent inside their own service. The `sdk` package is the supported surface
across that wall, kept small so the internals stay free to change.

```go
import abhed "github.com/zybuu-ai/abhed/sdk"

a, err := abhed.New(ctx, abhed.Options{
    Workspace: "/srv/work",
    Provider: &abhed.Provider{
        Type: "anthropic", Model: "claude-opus-5",
        APIKey: os.Getenv("ANTHROPIC_API_KEY"),
    },
    Mode: "auto",
    Deny: []string{"bash(rm -rf *)"},
    Approve: func(ctx context.Context, tool string, args json.RawMessage, d abhed.Decision) (bool, error) {
        return askTheUser(tool, args, d.Reason)
    },
    OnEvent: func(ev abhed.Event) { log.Println(ev.Type) },
})
if err != nil {
    return err
}
defer a.Close()

answer, err := a.Run(ctx, "fix the failing tests")
```

## The surface

| Method | Purpose |
|---|---|
| `New` | build an agent from options, a config directory, or both |
| `Run`, `Continue` | send a prompt; `Continue` keeps the conversation |
| `Steer` | redirect a run already in progress, from another goroutine |
| `Fork` | rebuild the conversation up to a sequence number |
| `Events` | everything recorded; the stream is the session |
| `Usage` | tokens, turns, cache hit rate, compactions |
| `ExportHTML` | a self-contained transcript |
| `SetModel` | swap providers mid-conversation, keeping history |
| `Providers` | the provider types this build supports |

## What does not change when embedded

Policy still decides what runs. Every action is still recorded as an event, so
an embedded session replays exactly like one from the CLI. An extension still
may veto and never permit.

One difference is deliberate: **an agent with no `Approve` function refuses
anything needing approval** rather than assuming yes. A service with nobody to
ask should be stricter than a terminal with somebody watching, not looser —
defaulting to permissive would make the SDK quietly weaker than the same policy
on the command line.

## Driving it from another language

`abhed rpc` speaks line-delimited JSON on stdin and stdout, for callers that are
not Go. See [15-extensions.md](15-extensions.md).

## What is not here yet

**Subscription auth.** Some harnesses sign in with a model vendor's consumer
subscription. Abhed takes an API key. Closing this needs each vendor's
OAuth device flow and token refresh, one at a time — real work, and worth doing,
but not a change to the harness.
