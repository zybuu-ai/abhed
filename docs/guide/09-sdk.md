# The SDK

Abhed's internals live under `internal/`, which Go refuses to let another module
import. The `sdk` package is the supported surface across that wall, kept small
so the internals stay free to change.

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

## Surface

| Method | |
|---|---|
| `New` | build from options, a config directory, or both |
| `Run`, `Continue` | send a prompt; `Continue` keeps the conversation |
| `Steer` | redirect a run in progress, from another goroutine |
| `Fork` | rebuild the conversation up to a sequence number |
| `Events` | everything recorded; the stream is the session |
| `Usage` | tokens, turns, cache hit rate, compactions |
| `ExportHTML` | a self-contained transcript |
| `SetModel` | swap providers mid-conversation |
| `Providers` | the provider types this build supports |

## What does not change when embedded

Policy still decides what runs. Every action is still recorded, so an embedded
session replays exactly like one from the CLI. An extension still may veto and
never permit.

One difference is deliberate: **an agent with no `Approve` function refuses
anything needing approval** rather than assuming yes. A service with nobody to
ask should be stricter than a terminal with somebody watching, not looser.

Edits that would break a file's syntax are refused, as from the command line;
see [Tools](05-tools.md#an-edit-that-would-break-the-file).

## Options and configuration

`Options` are laid over the configuration the way flags are on the command
line. `ConfigDir` reads the same files the CLI does; without it only the
managed file is read.

| Option | Without a managed file | Under a managed file |
|---|---|---|
| `Mode` | replaces `permissions.mode` | `bypass` is refused; if the file sets `permissions.mode`, only that mode or `plan` |
| `SyntaxCheck` | replaces `tools.syntax_check` (`refuse`, `report`, `off`) | if the file sets it, only as strict or stricter (`off` < `report` < `refuse`) |
| `MaxTurns` | replaces the default turn limit | if the file sets `limits.max_turns`, at most that; zero uses it |
| `Allow` | added to `permissions.allow` | refused if the file sets `permissions.allow` |
| `Deny` | added to `permissions.deny` | added; the file's deny rules stay |
| `Extensions` | added to the configured ones | added; an extension can only veto |

An option the managed file forbids is an error from `New`, a
`*config.ManagedError` naming the setting, never a silent change of what runs.
A managed file makes the engine managed as it does for the CLI and the server,
so `bypass` reaching it from a lower file is refused there too, and its deny
and ask rules apply. If it sets any `sandbox` key, bash runs in the sandbox
that setting selects, and `New` fails when no backend meets `sandbox.min_tier`;
otherwise the SDK builds no sandbox and bash runs as the embedding process.

`Provider` and `SetModel` are not bound by the managed file: they name any
endpoint, as a user's own config file may.
