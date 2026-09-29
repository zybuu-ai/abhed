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
| `Fork` | rebuild the conversation up to a sequence number (0 for all of it), and record a `conversation.forked` event so later rebuilds leave out what came after; a step an earlier fork abandoned, or past the end, is refused |
| `Events` | everything recorded; the stream is the session. `abhed.Live(events)` drops the steps a fork abandoned (`EvForked`, payload `abhed.Forked`) |
| `Flush` | wait until `OnEvent` has returned for every event recorded so far |
| `Usage` | tokens, turns, cache hit rate, compactions |
| `ExportHTML` | a self-contained transcript |
| `SetModel` | swap providers mid-conversation; the switch is recorded as `model.switched`, and one the record refuses is an error and is not made |
| `Providers` | the provider types this build supports |

`OnEvent` gets every event, in the order the store records them, from a
goroutine of its own, so a slow one falls behind and loses nothing; what it
has not taken yet is held in memory. Before a process exits on a stopped
run, call `Flush` with a deadline so the run's end reaches `OnEvent`. Never
call it from `OnEvent`, whose own goroutine delivers what it waits for.

## Tools

By default an embedded agent has the built-in tools: `read`, `write`,
`edit`, `glob`, `grep`, `bash` and `todo`, whose list is recorded as
`todo.updated` like the CLI's. `Options.ConfiguredTools` gives it the tool
set the CLI runs with, as the configuration enables it: subagents (`task`
and `tasks`, which share the agent's policy, approver and budget; see
[Parallel subagents](14-parallel-subagents.md)), MCP servers, the tools
extensions provide, skills and their pipelines, web search and `web_fetch`,
the code index, rag corpora, and the Kubernetes and SSH tools. With no
`web_fetch.allowed_hosts`, `web_fetch` asks `Approve`, as it asks at the
terminal, and is refused without one; it never sends a URL holding a value
from the secrets store. It is off by default so an
embedder decides what else its agent can reach; `abhed rpc` and `abhed acp`
turn it on. What an untrusted `ConfigDir` file names (MCP servers,
extensions, skill directories, corpora, clusters, hosts) is ignored either
way. The built-in prompt then carries the `ABHED.md` memory files too; without
`ConfiguredTools` it has none. Either way the prompt names only the web tools
the agent has. `Close` ends the MCP connections and extension processes, and
the cluster logins and hosts the agent's session made.

A subagent's own events stay in the agent's store; `Events` and `OnEvent`
carry the agent's own record, where `subagent.spawned`, `subagent.ask`,
`subagent.action` and `subagent.returned` stand for them, as the CLI's JSON
output does.

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

Inside `Approve`, `abhed.CallIDOf(ctx)` is the call id its tool call events
carry (empty for a call the model wrote as prose), and
`abhed.RequestIDOf(ctx)` is the id of its `action.requested` event, unique in
the session; bind a person's answer to the latter.

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

Stored secrets are redacted as from the command line. A value in the
operator's store (`~/.abhed/secrets.json`, or `ABHED_SECRETS_FILE`) becomes
`[secret:NAME]` before it reaches the record, `OnEvent`, the model, the
arguments and decision passed to `Approve`, or what `Run` and `RunJSON` return.
The SDK has no option to turn this off, and `New` returns an error when the
store exists but cannot be loaded. The store is read once, when `New` is
called. A structured answer is redacted after it is validated, so a redacted
answer may no longer match the caller's schema, for example a `pattern`, an
`enum` or a length bound. One whose redaction fails comes back as
`{"withheld": ...}`, which will not decode into the caller's type. See [Secrets](04-permissions.md#secrets).

Edits that would break a file's syntax are refused, as from the command line;
see [Tools](05-tools.md#an-edit-that-would-break-the-file).

## Options and configuration

`Options` are laid over the configuration the way flags are on the command
line. `ConfigDir` reads the same files the CLI does; without it only the
managed file is read. The `.abhed/config.json` in `ConfigDir` is untrusted
until the person trusts it (`abhed trust grant`), and until then only its
tightening settings apply. `Options.WorkspaceTrust` overrides that for one
agent (`config.TrustGranted` or `config.TrustRefused`), and
`Agent.WorkspaceTrust()` reports what was decided and what was ignored. If
the untrusted file names its own model and `Options.Provider` is nil, `New`
returns `abhed.ErrUntrustedModel` rather than run on a different model; set
`Options.AllowDefaultModel` to run on the configured default instead.

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
- The memory files (`ABHED.md`, the organisation's `/etc/abhed/ABHED.md`
  included). The built-in prompt carries them, as the CLI's does, only with
  `Options.ConfiguredTools`: an embedder running on repositories it does not
  own takes their instructions only by opting in. `SystemPrompt` replaces
  the built-in prompt entirely, and with it the memory files.

`limits.max_budget_tokens` and `limits.max_tokens` apply as they do from the
command line.

The binding holds for the shipped entry points (the CLI, the server, the SDK)
and for any program that builds its configuration with `config.Load` or
`config.LoadManaged`. A `config.Config` built by hand records no managed keys,
and `Apply` then refuses nothing.
