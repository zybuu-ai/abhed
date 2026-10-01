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
| `Fork` | rebuild the conversation up to a sequence number (0 for all of it), and record a `conversation.forked` event so later rebuilds leave out what came after; a step an earlier fork abandoned, or past the end, is refused, and so is a fork while a run is in progress (`ErrForkDuringRun`) |
| `Events` | everything recorded; the stream is the session. `abhed.Live(events)` drops the steps a fork abandoned (`EvForked`, payload `abhed.Forked`) |
| `Flush` | wait until `OnEvent` has returned for every event recorded so far |
| `Usage` | tokens, turns, cache hit rate, compactions |
| `ExportHTML` | a self-contained transcript |
| `Models`, `SwitchModelNamed` | list the configured models, and move the conversation to one by its configured name, recorded as `model.switched`. Only providers a trusted configuration file defines are listed, never a built-in nobody configured, and a managed `model.default` pins the model. An unknown name is `ErrUnknownModel`, a switch while a run is in progress is `ErrSwitchDuringRun`, and a provider whose `api_key_env` is unset is an error naming the variable |
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
the cluster logins and hosts the agent's session made. `Options.Warn`
receives what the tool set skipped or found unsafe as it was built, such as
an MCP server that did not start or a cluster that skips TLS verification;
nil discards it.

A subagent's own events stay in the agent's store; `Events` and `OnEvent`
carry the agent's own record, where `subagent.spawned`, `subagent.ask`,
`subagent.action` and `subagent.returned` stand for them, as the CLI's JSON
output does.

### Background tasks

With `ConfiguredTools`, `Options.Background` says what a background task
does. `"off"`, the default, joins it: `Run` returns when the work, the
task's included, is done, as it always has, and cancelling `Run`'s context
stops everything. `"notify"` lets a task outlive `Run`: its result is recorded
and reaches `OnEvent` as a `subagent.notice` when it ends, and the next `Run`,
or `Wake`, sees it. `"auto"` does too, and a result that arrives while no run
is in progress starts a wake run on its own, its events to `OnEvent`.
`HostWake`, when set, is given that run to start on the host's own terms
(the editor integration opens a turn for it); nil runs it on the agent's own
goroutine. `CancelTasks` also ends a wake run in progress and holds the next
until `Run`.

| Method | |
|---|---|
| `Background()` | the tasks, with status, model, turns and, when done, the summary |
| `CancelTask(id)`, `CancelTasks()` | stop one, or all, as a person's stop; `CancelTasks` ends a wake run too |
| `WaitBackground(ctx)` | wait until none is running |
| `Wake(ctx)` | run the agent on the results waiting, recorded as `session.woken` by the caller; `ErrNothingToWake` when none waits |

`Approve` may be called for a task's ask at any time until `Close`, after
`Run` has returned included. `Close` cancels running tasks as
`session_closed` and waits a moment for them to record their end.

With `ConfiguredTools` the subagents offer the [agent
definitions](17-agent-definitions.md) the CLI would load: the managed
directory, `ConfigDir`'s `.abhed/agents` when that workspace is trusted, and
`agents.dirs`. A subagent may run on another provider the configuration
names, never on an endpoint. With `Options.Provider` set, the agent's own
model is the only one, so no subagent chooses another: a definition naming a
model does not load, and a call naming one is refused.

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
store exists but cannot be loaded. The store is read when `New` is
called and again whenever the file changes, so a secret added during the
session is redacted from then on. A structured answer is redacted after it is validated, so a redacted
answer may no longer match the caller's schema, for example a `pattern`, an
`enum` or a length bound. One whose redaction fails comes back as
`{"withheld": ...}`, which will not decode into the caller's type. `bash` can
use a stored secret by name, as from the command line, when a
`secret(NAME)` rule allows it. See [Secrets](04-permissions.md#secrets).

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
| `ConfiguredLimits` | when `MaxTurns` is zero, the files' `limits.max_turns` binds, as for the CLI; off, it does not | the same, the managed value still the ceiling |
| `Allow` | added to `permissions.allow` | refused if the file sets `permissions.allow` |
| `Suggest` | after each completed `Run`, one small model call offers a next prompt as a `suggestion.offered` event, delivered after `Run` returns (the next `Run` or `Close` cancels it), unless `suggest.enabled` is false; off by default | a managed `suggest.enabled: false` binds |
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
