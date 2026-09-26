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
| `Flush` | wait until `OnEvent` has returned for every event recorded so far |
| `Usage` | tokens, turns, cache hit rate, compactions |
| `ExportHTML` | a self-contained transcript |
| `SetModel` | swap providers mid-conversation |
| `Providers` | the provider types this build supports |

`OnEvent` gets every event, in the order the store records them, from a
goroutine of its own, so a slow one falls behind and loses nothing; what it
has not taken yet is held in memory. Before a process exits on a stopped
run, call `Flush` with a deadline so the run's end reaches `OnEvent`. Never
call it from `OnEvent`, whose own goroutine delivers what it waits for.

## Who settled a call

Every `action.approved` and `action.denied` says who settled it in `by`.
When `Approve` answers, the record says `reviewer`, a person asked. When it
answers without asking anyone, it says so with `NoteAnswer`, passing the
`ctx` it was given:

```go
Approve: func(ctx context.Context, tool string, args json.RawMessage, d abhed.Decision) (bool, error) {
    if s := d.Offer(); s != "" && remembered[s] {
        abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.BySessionScope, Scope: s})
        return true, nil
    }
    return askTheUser(tool, args, d.Reason)
},
```

`d.Offer()` is the scope a person may choose to always allow, and the only
one a remembered choice may satisfy. It is empty for an ask rule, a
destructive command, or anything else that must ask every time, even where
`d.Scope` is set, so check it rather than `d.Scope`.

A person's answer can say who they are and the scope they chose, recorded on
that approval or refusal as `approver` and `granted_scope`:
`abhed.NoteAnswer(ctx, abhed.Answer{By: abhed.ByReviewer, Approver: "olga@example.com", Granted: s})`.
`Approver` is recorded as your approver asserts it; Abhed does not verify it,
so authenticate the person before you name them.

| `By` | When |
|---|---|
| `BySessionScope` | allowed by a scope a person chose to always allow earlier; set `Scope` |
| `ByHeadless` | nobody could be asked, and a fixed answer applied; `Reason` says why, in place of "rejected" |
| `BySystem` | the request ended with no answer, such as a timeout; `Reason` says why |
| `ByReviewer`, `ByUser`, `ByPolicy` | a person answered, the person made the call, or policy decided; rarely needed from an approver |

A `By` that is none of these is ignored, and the record says `reviewer`. The
events and their fields are in the [data model](../architecture/10-data-model.md).

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
that setting selects, and `New` fails when no backend meets `sandbox.min_tier`.
`Sandbox: true` does the same from the configuration `ConfigDir` names, as the
CLI does (`abhed rpc`, `abhed acp` and `abhed resolve` set it). Otherwise the
SDK builds no sandbox and bash runs as the embedding process.

Not bound by the managed file:

- `Provider` and `SetModel`, which name any endpoint, as a user's own config
  file may.
- The organisation's `/etc/abhed/ABHED.md`: the SDK loads no memory files, so
  an embedded agent never sees it, and `SystemPrompt` replaces the built-in
  prompt entirely.
- `limits.max_budget_tokens` and `limits.max_tokens`, which the SDK does not
  apply at all.

The binding holds for the shipped entry points (the CLI, the server, the SDK)
and for any program that builds its configuration with `config.Load` or
`config.LoadManaged`. A `config.Config` built by hand records no managed keys,
and `Apply` then refuses nothing.
