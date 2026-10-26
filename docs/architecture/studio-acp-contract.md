# Abhed Studio and the engine: the ACP contract

Status: 2026-10-01, `apiLevel: 1`; the engine side is implemented in 1.2.4 except where §11 says otherwise

Abhed Studio is an editor. It runs no agent code of its own: every capability
it shows is a call to the local `abhed acp` engine over the Agent Client
Protocol (ACP) on stdio. This page is the contract between the two. It lists
every method, notification and `_meta` field Studio uses, the shapes they
carry, what the engine does today, what the engine must add, and the security
rules for each.

The engine is the ground truth. Where this page and `app/acp.go` disagree
about something the engine already does, the engine is right and this page is
wrong. Where this page asks for something the engine did not do at 1.2.2, it is
marked **Not implemented in 1.2.2**. Those notes record the 1.2.2 starting
point; §11 says what the engine serves now.

Studio's test double, a stub engine in the Studio repository, speaks this
contract so Studio's views can be built and tested before each engine
addition lands. The stub is not a reference: a golden
transcript recorded from the real engine settles any disagreement.

---

## 1. Conventions

### 1.1 Protocol

- JSON-RPC 2.0, one message per line, on the engine's stdin and stdout. No
  port, no socket, no HTTP. Stderr is a log, never protocol.
- ACP protocol version 1. The engine follows the ACP schema; where it answers
  an older unstable shape as well (the `models` field and `session/set_model`),
  that is said in the row.
- **Spec first.** Where ACP has a method or update for a capability, it is
  used as specified, and Abhed's extra fields go in `_meta`.
- **Extensions.** ACP reserves method names that begin with an underscore for
  extensions. Abhed's extension methods and notifications are therefore named
  `_abhed/<area>/<verb>` on the wire. This page calls them the `abhed/*`
  family and always writes the full wire name.
- **`_meta` key.** Abhed's fields in any `_meta` object live under the key
  `zybuu.ai/abhed`, written `meta` below. Engines up to 1.2.2 also read and
  write `_meta.abhed` in two places (the `trust` field of `session/new` and
  the `workspaceTrust` field of its result). `zybuu.ai/abhed` is the current
  key; the engine still accepts the legacy `abhed` on input, refuses a
  `_meta` carrying both, and writes only `zybuu.ai/abhed` from `apiLevel` 1.
  Studio reads both.
- **W3C trace keys** (`traceparent`, `tracestate`, `baggage`) at the root of
  `_meta` are reserved by ACP and are never used for Abhed data.

### 1.2 Shapes

Shapes are TypeScript interfaces. `?` marks an optional field; a field not
listed must be ignored by the reader. Times are RFC 3339 strings in UTC.
Hashes are lowercase hex SHA-256. Every string that came from a tool, a file,
a model or the network is **untrusted text**: Studio renders it as text, never
as HTML or markdown, and never uses it as a URL, a path to open without the
person's action, or a command.

### 1.3 Errors

| Code | Meaning |
|---|---|
| -32601 | Method not found: this engine does not have it. Studio hides the feature and says which engine version adds it. |
| -32602 | Invalid params, including an unknown session, task or terminal id. |
| -32000 | The engine refused. `message` says why in words a person can read. |
| -32001 | Refused by policy: managed configuration, workspace trust or a policy rule. `data.meta.rule` names the rule when there is one. |
| -32002 | Busy: a prompt is running and the request must wait for it to end. |
| -32003 | The record is unavailable or failed verification. |
| -32004 | Another Abhed process (the CLI) has the session open; it can be forked. |

The engine never answers a refusal with an empty success.

### 1.4 Capability advertisement

`initialize` returns, in `agentCapabilities._meta["zybuu.ai/abhed"]`:

```ts
interface AbhedAgentMeta {
  apiLevel: 1;                     // this page
  edition: "ce" | "ee";
  version: string;                 // "1.2.3"
  commit: string;
  features: string[];              // see below
  record: { store: "local" | "postgres" | "memory"; dir?: string };
  managed: boolean;                // a managed configuration is in force
}
```

`features` names every `abhed/*` area the engine serves: `sessions`, `fork`,
`events`, `record.verify`, `hawkeye`, `export`, `tasks`, `tasks.review`,
`capabilities`, `policy.explain`, `trust.inspect`, `modes`, `review`,
`checkpoints`, `terminal`, `queue`, `manual`, `doctor`, `resolve`, `index`,
`infra`, `mcp.restart`, `memory`, `suggestions`, `answer-verdicts`, `worktrees`, `ollama`, `team` (EE). Studio shows a view only when
its feature is listed, and otherwise says which engine version adds it. It
never probes by calling a method and reading the error.

Spec capabilities are also set truthfully: `loadSession`, and
`sessionCapabilities.list`, `.resume` and `.close` once sessions exist;
`sessionCapabilities.delete` is **never** set (see §4.1).

**Engine today:** `initialize` has no `_meta`, `loadSession: false`.
**Not implemented in 1.2.2:** the block above and the `abhed version --json` command that
prints it without starting a session, so Studio can check the engine before it
spawns it.

Studio compares `apiLevel` with the level it was built for. A mismatch is a
blocking message ("this engine does not match this Studio"), never a
best-effort fallback.

---

## 2. Rules every row obeys

These are the invariants. A row's own security notes add to them and never
relax them.

1. **The engine decides.** Studio can answer an ask the engine put to it. It
   can never lift a deny, run a tool, write a file for the agent, grant
   workspace trust, widen policy or add a provider, MCP server or extension
   over ACP. A method that would do one of these does not exist, and the
   engine refuses unknown fields that would.
2. **Only a person's click answers.** An answer to `session/request_permission`
   names one of the option ids the engine offered for that request, and only
   after a person clicked it. Anything else (a timeout, an auto-approve
   setting, an OS notification, an unreadable answer) is the offered reject
   option or `cancelled`. The engine refuses an option id it did not offer.
3. **Destructive and widening steps need a modal whose default is No.**
   Destructive calls get a second confirm in Studio. Trust grants and settings
   that widen the harness are never done over ACP: Studio's main process shows
   a native dialog and then runs the engine's own command line
   (`abhed trust grant -sha256 …`, `abhed config set …`).
4. **No secret value passes through Studio.** Secrets are typed into the
   engine's own hidden prompt in an Abhed terminal. Studio sees names only.
   Responses carry names, hashes and redacted text.
5. **Everything is recorded.** Every agent action, every answer, and every
   action a person takes through Studio's agent surfaces is an event in the
   session's hash-chained record, attributed `by: "user"`, `"agent"` or
   `"system"`. Reads (lists, explains, reports) are not recorded.
6. **The agent cannot reach the IDE.** The engine write-protects Studio's
   settings, extensions, key bindings, its own binary, `.vscode/**`,
   `.git/config`, `.git/hooks/**`, `*.code-workspace` and `.devcontainer/**`
   (§4.21). The record directory is readable and writable by neither the agent
   nor Studio's renderer except through these methods.
7. **No network from Studio** except what the person turns on. The engine's
   own traffic is the engine's, under its policy.
8. **Scoping.** Every method that takes a `sessionId` acts only on sessions of
   the engine's user (tenant) and, for `session/list`, only on records the
   engine may read. An id from another tenant is "unknown session".

---

## 3. Conversation

### 3.1 Chat, streaming and thinking

| | |
|---|---|
| Wire | Spec. `session/prompt`; updates `agent_message_chunk`, `agent_thought_chunk`, `tool_call`, `tool_call_update`, `plan`, `usage_update`. |
| Engine today | Forwards `agent.delta` as message chunks and the whole `agent.reasoning` as one thought chunk. `agent.reasoning.delta` is dropped. |
| Not implemented in 1.2.2 | Forward `agent.reasoning.delta` as `agent_thought_chunk` and skip the whole-text `agent.reasoning` when deltas were sent for that turn. Stop reasons from the loop's terminal reason instead of matching error text: `completed`→`end_turn`, `max_turns`/`wake_limit`→`max_turn_requests`, `user_interrupt`/`shutdown`→`cancelled`, `max_budget`→`max_tokens` with `meta.reason: "budget"`, anything else→`refusal` with `meta.reason` set to the terminal reason. |
| Security | Message and thought text is untrusted and rendered without HTML. Reasoning is display-only; the engine never feeds it back as history. |

### 3.1a Attached context

`session/prompt` takes `text`, `resource_link` and `resource` blocks
(`embeddedContext: true`). A `text` block is the person's own words. A
`resource_link` names a file the agent reads through its policy-checked
tools. A `resource` is text the person attached in the editor (a selection,
a symbol, a problem the editor reports). The engine does not read the text
itself, since the person picked it, but a `file:` URI is put to the `read`
rules and the `.abhed` check as an `@` mention is: one that may not be read
refuses the prompt (`-32001`) and is recorded as a denied read by the person.
The text is treated as data. It is redacted,
capped as an `@` mention is (256 KiB for all of a prompt's attachments), and
given to the model fenced under a
tag with a random suffix after a note that says it is data, not
instructions, so nothing in it can close the block and read as the person's
words. Each is recorded `input.mention {path, range?, sha256, bytes,
truncated?}` by the person, with `path` and `range` from the URI
(`file:///…#L3-L9`), when it reaches the model: a built-in slash command
runs no prompt, so what was attached with it is neither used nor recorded,
and never becomes the command's arguments. Studio sends a selection as a `resource` only when it
is small and its file does not look like it holds secrets, and otherwise as
a `resource_link` to the lines.

### 3.2 Models and thinking level

| | |
|---|---|
| Wire | Spec config options. `session/new` returns `configOptions` with one `select` option of `category: "model"`, id `model`, whose values are configured provider **names**. `session/set_config_option {sessionId, configId: "model", value}` switches and returns every option. The older `models` field and `session/set_model` are answered too; that reply puts the new current model in `meta.currentModelId`. |
| Engine today | Done (`app/acp.go`). A switch while a prompt runs is refused with -32000. |
| Not implemented in 1.2.2 | A second option, id `thought_level`, `category: "thought_level"`, values `off`, `low`, `medium`, `high`, offered only for models whose provider reports reasoning control. `config_option_update` when the engine itself changes an option (a model fallback, recorded `model.fallback`). |
| Per-subagent model | `subagent.spawned` and the task list carry `provider` and `model` (already recorded). |
| Configuration warnings | Added in 1.2.9. `session/new`, `load` and `resume` results carry `_meta["zybuu.ai/abhed"].configWarnings: string[]` when something is wrong with the configuration: a key written twice in one object of any file read (the last one wins; a second provider of the same name replaces the first), a setting written twice in different letter case, a key no part of Abhed reads, and each setting an untrusted workspace file could not make. One plain, redacted sentence each; absent when there are none. Studio lists them in its model picker. |
| Security | The value is a configured name looked up in the engine's configuration. Nothing from Studio is used as an endpoint, key or model id. Descriptions never include the base URL, the key or the key's variable name. |

### 3.2a A local Ollama model

Added in 1.2.9; served when `features` lists `ollama`. When the person
chooses a model that a local Ollama serves (provider `type: "ollama"`, or an
OpenAI-compatible `base_url` on port 11434), the engine
can say whether it works and, after the person agrees in Studio, start
Ollama, install it or pull the model. Studio shows a card; the engine decides
what may run.

**On a switch.** The reply to `session/set_config_option` (`configId:
"model"`) and to `session/set_model` carries `meta.ollama: OllamaStatus` when
the chosen model is Ollama's and not ready; it is absent otherwise. This only
reads this machine (no registry, no download host). Studio then calls
`_abhed/ollama/status` for the full card. After `session/new` Studio calls
`_abhed/ollama/status` once for the current model; -32602 means Ollama does
not serve it, and nothing is shown.

```ts
interface OllamaProblem {
  kind: "not_installed" | "not_running" | "not_pulled" | "port_busy" | "not_ready" | "start_failed"
      | "install_failed" | "no_network" | "disk_short" | "pull_failed" | "model_not_found"
      | "remote" | "assist_off" | "not_admin" | "in_agent" | "cancelled" | "not_ollama" | "manual_steps";
  message: string;      // one plain sentence
  steps: string[];      // numbered manual steps, in order: commands or sentences
  retry: boolean;       // checking again may work once the steps are done
}
interface OllamaStatus {
  provider: string; model: string;  // configured name; Ollama tag
  api: string;                      // Ollama's API root, e.g. http://127.0.0.1:11434
  loopback: boolean;                // only then does the engine act
  installed: boolean; binary?: string; app?: string;
  running: boolean; version?: string; port_busy?: boolean;
  pulled: boolean; ready: boolean;
  next?: "install" | "start" | "pull";
  problem?: OllamaProblem;          // absent when ready
}
interface OllamaReport extends OllamaStatus {
  assist: "ask" | "local" | "off";  // model.ollama.assist
  can_act: boolean;                 // the next step may be offered
  detail: boolean;                  // sizes were asked of the registry or download host
  start_always: boolean;            // the person said "always start"
  start?: { command: string; log?: string; program?: string; app_binds?: boolean };
  install?: { kind: "run" | "open" | "manual"; command: string; source: string;
              size?: string; steps?: string[] };
  pull?: { command: string; source: string; bytes: number; size_known: boolean;
           disk_free?: number; mem_total?: number; ram_needed_gb?: number; ram_estimate?: boolean;
           warnings?: string[]; smaller?: string };
  refusal?: OllamaProblem;          // why the next step is not offered
  catalog: { tag: string; label: string; bytes: number; min_ram_gb: number }[];
}
```

| Method | Params → result |
|---|---|
| `_abhed/ollama/status` | `{sessionId, provider?, detail?: boolean}` → `OllamaReport`. `provider` defaults to the session's current model. Reads only, and without `detail` asks nothing off this machine: the local Ollama, the disk and the memory, with a curated model's size from Abhed's list (`pull.size_known: false`). Studio's automatic status (after `session/new`, on a model switch, when polling) must not set `detail`. With `detail: true`, sent only when the person opens the install or download step, it also asks the registry or download host for the size as it is now (`detail: true` in the report), and only when that step may be offered (never under `assist: "local"` or `"off"`). Not recorded |
| `_abhed/ollama/start` | `{sessionId, provider?, always?: boolean}` → `{started: {pid?, version, log?}, status: OllamaStatus}`, or `{status}` when it was already running. `always` keeps "always start without asking" (only after the person ticked it). Recorded `ollama.start` |
| `_abhed/ollama/install` | `{sessionId, provider?}` → `{outcome: "ok" \| "manual", output, status}`, or `{status}` when installed. Runs `brew install ollama` where Homebrew is, or opens the macOS download page (`outcome: "manual"`: the person installs). Never runs `sudo`, nor the official Linux install script, which does: both are `manual_steps`, printed with their source. Recorded `ollama.install` |
| `_abhed/ollama/pull` | `{sessionId, provider?, pullId}` → `{status}` when it ends ready. While it runs, notifications `_abhed/ollama/progress {sessionId, pullId, status, completed?, total?}`. Disk short, model not in the registry or no network fail before any download. Recorded `ollama.pull` |
| `_abhed/ollama/cancel` | `{pullId}` → `{cancelled: boolean}`; the pull's reply is then -32000 with `kind: "cancelled"`. Ollama keeps what it fetched |
| `_abhed/ollama/forget` | `{}` → `{}`: forget "always start" |

**Errors.** A refusal by configuration or place (`assist_off`, `not_admin`,
`in_agent`, `remote`) is -32001; a failure (`start_failed`, `not_ready`,
`port_busy`, `install_failed`, `pull_failed`, `disk_short`, `no_network`,
`model_not_found`, `cancelled`, `manual_steps`, and `not_ollama` for a pull asked of a server that answers only the OpenAI API) is
-32000. Either way `data` is `{problem: OllamaProblem, status:
OllamaStatus}`; Studio shows `problem.message`, the numbered `problem.steps`
and a *Check again* button. A provider Ollama does not serve, or not
configured, is -32602.

**Studio's side.** Before `start`, a modal showing `start.command` with
*Start*, *Always start without asking* and *No* (default No; *always* is a
widening choice). Before `install` and `pull`, a modal every time showing the
exact `command`, `source` and size (`install.size`, or `pull.bytes` with
`pull.warnings` and `pull.smaller`), default No. A *Stop* button sends
`_abhed/ollama/cancel`. A `manual_steps` answer shows the steps for the
person to run in their own terminal, outside Abhed. Not in the sandboxed
Abhed terminal (§7.1): there `abhed ollama check` is refused as running
inside an agent's command, and the sandbox could not install anyway.
Nothing is sent without a click (§2.2). When `start.app_binds` is set, the
modal says the app listens where its own settings say.

| | |
|---|---|
| Security | The engine acts only on a loopback Ollama, as the engine's user with a minimal environment, never with `sudo`. A server it starts with `ollama serve` (always, when the program is found) is bound to `127.0.0.1`; the macOS app, opened by its path only when the program is missing, binds where its own settings say, which the engine does not control. `model.ollama.assist` (managed-lockable) is applied whatever Studio sends; inside an agent's command every action is refused. No tool exposes any of this, so the agent cannot start, install or pull. A server it started keeps running when the engine exits. Every attempt and outcome is recorded with `by: "user"` and `surface: "acp"`. |

### 3.2b Language toolchains

The agent's commands get the language toolchains Abhed installed, first on
`PATH` ([Toolchains](../guide/32-toolchains.md)). The agent installs one with
its `toolchain` tool, as an ordinary ask. Studio offers the same from its own
UI, for instance when its Run or Test button's command exits 127, with these
methods (`toolchain` in the session's `_meta` features). `language` is a
language (`go`, `python`, `node`, `rust`, `java`, `maven`, `gradle`, `ruby`,
`dotnet`) or a program one provides, as the failed command named it
(`cargo` is rust, `mvn` maven, `python3` python, `gofmt` go).

| Method | |
|---|---|
| `_abhed/toolchain/status` | `{sessionId, language, version?, debugger?, lsp?}` → `{language, available, version?, can_act, debugger?: {name, version, available}, lsp?: {name, version, available}, offer?: {command, title, source, size?, version?, into?, why, url, digest, registries, registries_note?, debugger?: extra, lsp?: extra}, refusal?: problem}`, where `extra` is `{name, display, version, source, url, digest, size?, note?, installed, needs?: {language, display, version, source, url, digest, size?}}`. With `lsp: true` the offer includes the language's server (gopls, basedpyright, typescript-language-server for `node`, `typescript` or `javascript`, rust-analyzer); `needs` is a language it runs on that is installed first (Node.js for basedpyright), which the modal must show, and `title` says so; `refusal.kind` is `no_lsp` where it cannot be had (Go older than 1.26, Node.js older than 22.22.2 for TypeScript's server, a platform with no build). With `debugger: true` the offer includes the language's debugger (Delve for Go, debugpy for Python; `title` names both, "Go 1.27.3 + Delve debugger 1.27.2", and `note` says how Delve is built); `refusal.kind` is `no_debugger` where it cannot be had (not a Linux amd64 or arm64 sandbox for Delve, Go older than 1.25, Python older than 3.10). `debugger` at the top says whether the session's commands have it now. `registries` are the package registry hosts the install opens to the session's commands through its egress proxy (an empty list with `registries_note` saying why none, such as `sandbox.toolchains_registries is off`); this is the one way a person opens egress outside the managed file, so **the modal must show them**. `available` and `version`: the session's commands have the language now (`into` is where an install would go, for the platform commands run on). `offer` is what an install would fetch: the version and `why` (`requested`, `pinned by <file>`, `latest stable`), the release `source`, the download `url`, its `size` and the published `digest` it is checked against; `command` is the same install from a terminal. `can_act` is false, with `refusal`, when `sandbox.toolchains` is off, inside an agent's command, or when the source has no such build. Asks the release source; installs nothing. Not recorded |
| `_abhed/toolchain/install` | `{sessionId, language, version?, installId, debugger?, lsp?, agreed?: {version, digest, debugger_version?, debugger_digest?, lsp_version?, lsp_digest?, needs_version?, needs_digest?, registries}}`. Studio sends `agreed`: the offer's version, digests and `registries` the modal showed, and `needs_version` and `needs_digest` for a language the server runs on that comes first (`offer.lsp.needs`); if the install would now fetch anything else, or open other registries, it is refused with `changed` and nothing is installed. An install without `agreed` (an older Studio) is still made, but recorded with `consent: "studio-unconfirmed"` and **opens no registries**; a later contract version will require `agreed`. → `{language, available: true, version, dir, bin, debugger?: {name, version, dir, bin, pythonpath}, lsp?: {name, version, dir, bin}, registries}` once it is verified and in place (with `debugger: true`, the debugger is planned first, and a `no_debugger` refusal installs nothing; progress for it has `status: "debugger"`); `registries` says which package registries commands can now reach (§4 of the Toolchains guide). While it runs, notifications `_abhed/toolchain/progress {sessionId, installId, status: "downloading", completed, total}`. Recorded `toolchain.install` with `by: "user"`, `surface: "acp"`, `consent: "studio"` (or `"studio-unconfirmed"` without `agreed`), and `toolchain.registry` for registries opened |
| `_abhed/toolchain/cancel` | `{installId}` → `{cancelled: boolean}`; the install's reply is then -32000 with `kind: "cancelled"`, and nothing partial is left |

**Errors.** `data` is `{problem: {kind, message, steps?}}`, as the Ollama
assist's. `toolchains_off`, `in_agent` and `unavailable` are -32001;
`unknown_language`, `bad_version`, `no_install_id` and `duplicate` are
-32602; `no_build`, `no_debugger`, `no_lsp`, `changed`, `install_failed` (including a download
that does not match its published checksum) and `cancelled` are -32000.

**Studio's side.** Before `install`, a modal showing `status.offer`'s
`title`, the version and why, the source, the URL, the size and the
checksum, the debugger's source and checksum when asked for, and the
package registries the install opens to commands (`registries`, or
`registries_note`), default No; `install` then carries `agreed`. The modal is the person's consent; the engine records it as such.
A *Stop* button sends `_abhed/toolchain/cancel`. After an install, the next
command of the agent finds the toolchain on `PATH`; Studio's own Run and Test
buttons may run again.

| | |
|---|---|
| Security | Downloads go only to each language's official release hosts, redirects included, and are checked against the checksum the project publishes (Ruby's against digests pinned in Abhed). A toolchain is unpacked into the per-user cache, refusing any entry or link that leaves it, and made read-only; commands can read it, never change it. Nothing is installed system-wide or with `sudo`. `sandbox.toolchains` (managed-lockable) is applied whatever Studio sends. |

### 3.3 Slash commands

`available_commands_update` (spec), sent after `session/new` and
`session/load`, and again when skills or custom commands change:

```ts
{ sessionUpdate: "available_commands_update",
  availableCommands: { name: string; description: string; input?: { hint: string } }[] }
```

Each entry carries `meta = { source: "builtin"|"user"|"workspace"|"managed"|"mcp"|"skill", sha256?: string }`.
Built-in names win over every other source. A command is a prompt the engine
expands; running one is recorded `command.invoked` with its source and hash.

**Not implemented in 1.2.2:** the update and the list: `/compact /fork /undo /diff
/hawkeye /tasks /memory /mode /model /clear /export`, plus custom commands and
skills. **Security:** workspace commands are listed only when the workspace is
trusted, since a command is instructions.

### 3.4 Usage and cost

`usage_update` (spec) `{used, size}` per model call. **Not implemented in 1.2.2:**
`cost: {amount, currency}` only when a price table is configured, and
`meta = {tokensIn, tokensOut, tokensCached, sessionTotalIn, sessionTotalOut}`.
No invented prices.

### 3.4a Next-prompt suggestion

When a turn completes, the engine may send, after the `session/prompt`
reply, one `agent_message_chunk` with empty text that carries
`_meta["zybuu.ai/abhed"] = {suggestion: string}`: a guess at what the person
may ask next, one line of at most 80 characters with control and format
characters removed, never one that urges past a safeguard (ignore, bypass or
override a policy, an approval, a rule, the sandbox) or towards a
destructive action (delete, `rm -rf`, force-push, drop, wipe, disable), or
one that asks to show a secret. It is
model text: Studio never sends it unless the person does. Studio shows it as the chat input's dimmed placeholder,
puts it in the input on Tab, and never sends it on its own; typing or the
next turn clears it. The suggestion is made after the turn has ended, so
the reply never waits for it: it arrives as a `session/update` once the
prompt has returned `end_turn`, usually within a few seconds, and the next
`session/prompt`, a wake or `session/close` cancels it. Studio offers it only
while that session has no prompt running and its input is empty. None is
sent after an error or a cancel, while an approval waits, on a replay
(`session/load`), or when `suggest.enabled` is false. It is recorded as
`suggestion.offered` after the run's `session.ended`, followed by a
`model.call` with `purpose: "suggestion"`, whose `usage_update` is not sent
(it is no measure of the context). `features` lists `suggestions`. Studio's own change is separate
from the engine.

### 3.4b Checked answers

A session labels its answers unless the configuration's `verify.mode` says
otherwise ([Verified answers](../guide/29-verified-answers.md)). After the
reply, the engine sends one `agent_message_chunk` whose text is the footer and
the contradicted and unverified statements as plain lines (empty for an answer
with nothing to check), and whose `_meta["zybuu.ai/abhed"] = {verdict: …}` is
the `answer.verdict` payload whole: each claim's `n`, `text`, `verdict`
(`verified`, `unverified`, `contradicted`), `reason`, `evidence` (record `seq`s)
and `conflict`, the counts, the contract's results and the revision note. An
editor that knows the key draws ✓, ? and ✗ badges and strikes contradicted
claims through; one that does not still shows the text. The `answer` tool's
call sends no tool card: its text is the reply. A replay (`session/load`)
sends the same chunk. In strict mode a run that ends unverified stops with
`end_turn` and `_meta["zybuu.ai/abhed"].reason` `unverified`: the turn has its
answer, and the checks could not verify it. `features` lists `answer-verdicts`.

### 3.5 Images

`promptCapabilities.image` stays `false` while the engine's messages are text
only. Studio never offers an image attachment while it is false.

---

## 4. Sessions and the record

The durable record is the engine's hash-chained local store: one JSONL file
per session under `~/.abhed/records/<tenant>/`, an append-only index, and a
head file, each line carrying `seq`, `prev` and `hash`. The CLI and Studio
share it. That store and its event types belong to the CLI's record work;
Studio consumes them through the methods below and builds neither.

### 4.1 List, load, resume, close

| Method | Shape |
|---|---|
| `session/list` (spec) | `{cwd?, cursor?}` → `{sessions: SessionInfo[], nextCursor?}` |
| `session/load` (spec) | `{sessionId, cwd, mcpServers: [], _meta?: {trust?: "untrusted"}}` → replays the session as `session/update` notifications, then replies `{configOptions, modes, _meta}` |
| `session/resume` (spec) | same params → no replay; replies as `session/load` |
| `session/close` (spec) | `{sessionId}` → `{}`: ends the engine's hold on the session; the record stays. Unless a running prompt records its own end, or background work its closing one, it records `session.ended` with reason `session_closed` after its terminals' last lines, carrying the last run's totals; so does the editor going away. The session stays resumable. |
| `_abhed/session/rename` | `{sessionId, name}` → `{}`, recorded `session.named` |

```ts
interface SessionInfo {          // spec fields
  sessionId: string; cwd: string; title?: string; updatedAt?: string;
  _meta?: { "zybuu.ai/abhed": SessionMeta };
}
interface SessionMeta {
  name?: string;                 // set by rename
  created: string;
  status: "live" | "idle" | "ended";
  endReason?: string;            // terminal reason when ended
  parent?: string; forkSeq?: number;   // a fork's origin
  model?: string; mode?: string;
  gitBranch?: string;
  events: number;
  head: { seq: number; hash: string };
  background: number;            // background tasks still running
  waitingAsks: number;           // held asks across the tree
  openElsewhere?: boolean;       // another Abhed process holds it
}
```

- **Replay.** `session/load` sends the recorded conversation as the same
  updates a live run would, with the recorded tool call ids and final
  statuses (every call has an id from 1.2.8; in an older record a call
  without one is named by its request's event id, and its result arrives as
  a `tool_call` of its own rather than being guessed onto a card), and each update's `meta.seq`. It **never re-runs a tool**. Then it
  restores the model, the mode (within today's ceiling), the approval scopes
  recorded as `approval.scope_granted` (only those today's policy still
  allows), the budget and the todo list.
- **One writer.** A session held by another Abhed process (the CLI) is
  refused with -32004 "open in another Abhed process"; Studio offers to fork
  it instead, matching the code rather than the words (engines before 1.2.8
  sent -32000).
- **Requested trust.** `session/load`, `session/resume` and
  `_abhed/session/fork` take `meta.trust: "untrusted"` as `session/new` does
  (§5.6): the session opens taking only the workspace settings that tighten,
  whatever trust is stored for the folder, and keeps that choice when it
  restarts for a changed workspace file. Studio must send it whenever the window
  is in Restricted Mode. Any other value is -32602. Without the field the
  session opens with the folder's stored trust, as before. A session already
  open on the connection is never narrowed in place: asking for it untrusted
  while it is open with the stored trust is refused with -32000, and Studio
  closes it and loads it again. Each records a `session.resumed` with `via`,
  `through_seq` and `workspace_trust` (§10), then the `config.refused` and
  `config.narrowed` of the configuration it opened under. When that event
  cannot be recorded the session does not run: load, resume and fork answer
  -32003 and close it, though a fork's copy is already written and stays in
  the session list, loadable later; a restart leaves the session read-only
  and says why.
  `_abhed/doctor` takes the same field, so a Restricted window's checks do not
  reach a provider only the trusted file names. A `_meta` with both the
  `zybuu.ai/abhed` and the legacy `abhed` key is -32602.
- **Verification before trust.** `session/load` and `session/resume` verify
  the chain first. The reply's `meta.record = {verified: boolean, head,
  firstBad?}`. Studio shows "unverified" and asks before continuing a session
  whose chain fails.
- **No deletion.** Records are kept until the person prunes them with
  `abhed record prune` in a terminal, which asks for confirmation and writes a
  tombstone. `session/delete` is not advertised and answers -32601. Studio has
  no delete action.
- **Title.** The first prompt, redacted, cut to 80 characters.

**Engine today:** none of these; sessions are memory-only.
**Not implemented in 1.2.2:** the whole row once the local store exists; `session/list`
reads the index, `session/load` reads the record.

### 4.2 Fork

`_abhed/session/fork {sessionId, throughSeq?}` → `{sessionId, parent,
forkSeq}`. With no `throughSeq` it forks at the head. The new session is
recorded `conversation.forked` in the child and `session.branched` in the
parent. Logins and ssh hosts do not carry over. Refused with -32002 while a
prompt or a wake runs in the source session (the SDK's `ErrForkDuringRun`).
The fork is returned open on this connection, as a `session/new` session is,
so its first prompt needs no `session/load`. Studio opens it in a new chat.

### 4.3 Compact and clear

- `_abhed/session/compact {sessionId, focus?}` → `{before, after}` tokens;
  recorded `compaction.started` and `compaction.completed`.
- `/clear` is Studio's action: `session/close`, then `session/new`. Nothing is
  deleted.

### 4.4 The event stream

```ts
// request
_abhed/events/subscribe { sessionId, afterSeq?: number }  → { head: {seq, hash}, subscription: string }
_abhed/events/unsubscribe { subscription }                 → {}
_abhed/events/page { sessionId, afterSeq?: number, limit?: number (≤ 500), types?: string[] }
                                                           → { events: RecordEvent[], head, more: boolean }
// notification, once per event after afterSeq, in seq order, including a backlog
_abhed/event { subscription, sessionId, event: RecordEvent }

interface RecordEvent {
  seq: number; id: string; ts: string;
  type: string;                         // §8
  actor: "user" | "agent" | "system" | "tool";
  trust: "trusted" | "untrusted";
  session: string;                      // the child's id for a mirrored subagent event
  payload: unknown;                     // as recorded: already redacted
  prev: string; hash: string;
}
```

- The stream is the record, not a second copy: `payload` is exactly what was
  written, after redaction; withheld values stay withheld.
- Delivery is at least once in seq order. Studio drops a seq it has seen.
- A subscription ends with its session's process or `unsubscribe`. At most
  eight per session; the ninth is refused.
- `types` filters a page only. Streams are unfiltered, so a view never infers
  "nothing happened" from a filter.

**Engine today:** only nine event types reach Studio, as session updates.
**Not implemented in 1.2.2:** the three methods and the notification over the local store.
**Security:** payloads marked untrusted are shown as data. The stream is
read-only.

### 4.5 Verify and export

- `_abhed/record/verify {sessionId}` → `{ok: boolean, events, head, firstBad?:
  {seq, reason}, store}`. It recomputes every line's hash and the head.
- `_abhed/export {sessionId, format: "jsonl" | "html"}` → `{path, bytes,
  sha256, head}`. The engine writes the file under `~/.abhed/exports/`, never
  in the workspace, and returns its path; Studio then offers a save dialog and
  copies the file. The JSONL keeps the chain, so `abhed record verify <file>`
  works offline. HTML is the HawkEYE report.
- **Honesty:** the chain is evidence against the agent and against accidental
  or partial edits. It is not proof against the owner of the machine, who can
  rewrite and re-chain. Studio's wording says "verified chain", never
  "tamper-proof".

### 4.6 HawkEYE

`_abhed/hawkeye {sessionId, format: "json" | "html"}` → for `json`, the
report the engine's `hawkeye` package builds over the durable record:

```ts
interface HawkeyeReport {
  session_id: string; started: string; ended: string; prompt: string;
  outcome: string;                         // terminal reason, or "running"
  models?: string[];
  totals: { events, turns, tool_calls, tokens_in, tokens_out, tokens_cached,
            cache_hit_rate, model_ms, tool_ms, duration_ms, peak_context,
            context_window, untrusted_observations, recalls };
  turns: Turn[]; calls: Call[];
  policy: { allowed, denied, asked_reviewer, by_step: Record<string, number> };
  subagents?: Subagent[]; files?: { path, reads, writes }[];
  findings: { severity: "info" | "warn" | "critical"; code; title; detail; seq? }[];
  integrity: { first_seq, last_seq, gaps?: number[], ordered, has_end };
}
```

For `html`, `{html: string}`: the same page `abhed hawkeye` renders.

- Studio refreshes the report when the event stream shows a turn ended, no
  more than once every two seconds.
- Findings go to Studio's Problems view with the finding's `seq` as the link.
- **Security:** the HTML report is shown only in a webview with no Node, no
  scripts and CSP `default-src 'none'; style-src 'unsafe-inline'`. The JSON is
  rendered as text.

**Engine today:** the report exists for the server; not over ACP.
**Not implemented in 1.2.2:** the method over the local record.

### 4.7 Retention

Records are kept until pruned. `abhed record prune --older-than <age>` runs in
a terminal, asks for confirmation and writes a tombstone. A managed
`record.retention_days` may enforce pruning. There is no pruning method over
ACP.

---

## 5. Control: modes, approvals, policy, trust

### 5.1 Modes

Spec session modes:

```ts
// session/new, session/load and session/resume results
modes: { currentModeId: string,
         availableModes: { id: "default" | "accept-edits" | "plan" | "auto" | "bypass";
                           name: string; description: string;
                           _meta?: { "zybuu.ai/abhed": { locked?: boolean; lockedBy?: "managed"; widens: boolean } } }[] }
session/set_mode { sessionId, modeId } → {}
update { sessionUpdate: "current_mode_update", currentModeId }
```

The same list is also a config option of `category: "mode"`.

- `availableModes` is what the engine allows *now*: the managed ceiling and
  the user's own mode list applied. `bypass` is present only when the user's
  configuration offers it and no managed policy forbids it.
- A mode outside `availableModes` is refused with -32001.
- The change is recorded `mode.changed {from, to, by: "user", via: "studio"}`,
  and the engine sends `current_mode_update`.
- Plan mode's exit is the engine's `plan.proposed` / `plan.decided` flow, shown
  as a normal ask: a `session/request_permission` whose `toolCall` (`kind:
  "think"`, `_meta.plan: true`, `planHash` and `steps` when the plan has a
  runbook) carries the plan and its steps as content, with the options
  `plan:accept-edits`, `plan:approve` and `plan:keep-planning`. A runbook with
  steps that cannot be undone is followed by a second request
  (`_meta.destructive: true`, `plan:confirm` or `plan:decline`). Accepting
  records `mode.changed` `via: plan-exit` and `plan.decided`, and the same
  prompt turn goes on with the plan.

**Engine today:** `session/set_mode` answers -32601; the mode is fixed at
construction. **Not implemented in 1.2.2:** a loop-level mode change through the same
configuration path the CLI uses, so managed ceilings hold.
**Security:** Studio needs a modal (default Cancel) before it sends `auto` or
`bypass`, and shows a red status bar entry while bypass is on. The engine does
not trust that modal: it enforces the ceiling itself.

### 5.2 Approvals

Spec `session/request_permission`, engine→client, exactly as `app/acp.go`
sends it:

```ts
{ sessionId,
  toolCall: { toolCallId, title, kind, status: "pending", rawInput,
              _meta: { "zybuu.ai/abhed": AskMeta } },
  options: [ { optionId: "once:<bind>",   name: "Allow once",          kind: "allow_once" },
             { optionId: "always:<bind>", name: "Always allow <scope>", kind: "allow_always" },  // only when offered
             { optionId: "reject:<bind>", name: "Deny",                 kind: "reject_once" } ] }

interface AskMeta {
  tool: string; step: string; reason: string; destructive: boolean;
  requestId?: string;        // the engine's action.requested id; <bind> is it
  supervisor?: { decision: string; code: string; rationale?: string; advice?: string; judge?: string };
                             // 1.2.9: the supervisor's verdict on the call, as recorded on action.requested
  scope?: string;            // what "always" grants, only when offered
  subagent?: string;         // a subagent's ask; toolCallId is "subagent-<requestId>"
  rule?: string;             // Not implemented in 1.2.2. The rule that asked, or "builtin:<name>"
  via?: string;              // Not implemented in 1.2.2. "pipeline <name> step <n>"
  held?: boolean;            // Not implemented in 1.2.2. Released from a background task's hold (§6.3)
  taskId?: string;           // Not implemented in 1.2.2. The background task asking
  diff?: { path; oldText?; newText }[]; // Not implemented in 1.2.2. For edit and write (§5.5)
}
```

- Option ids are bound to the request, so an answer meant for another ask is
  refused.
- "Always" is never offered for destructive, screen or ask-rule steps.
- An answer of "always" is recorded `approval.scope_granted {scope, by:
  "user"}` (**not implemented in 1.2.2**) so `session/load` can restore it.
- A call whose input the record withheld is refused without asking, with a
  message chunk that says so (done).
- An ask made while no prompt turn is open is held (§6.3).
- Studio's card shows the "why" line from `step`, `rule` and `reason`.
- A denied call's `tool_call_update` (`status: "failed"`, content `Denied:
  <reason>`) carries `meta.denied = {step, rule?, by}`, where `by` is
  `policy`, `reviewer` or `system` as on `action.denied`, so its card shows
  the same "why".

### 5.3 Destructive confirm

`meta.destructive: true` (done). Studio asks a second time in its own modal
(default Deny, the full command and reason shown) before sending an allow
option. No chat part or extension can answer that modal. Studio's own
tighten-only check also marks a command destructive when an older engine did
not say.

### 5.4 Policy view and explain

```ts
_abhed/capabilities { sessionId } → Capabilities   // §6.4; includes .policy
_abhed/policy/explain { sessionId, tool: string, args: unknown }
  → { decision: "allow" | "ask" | "deny"; step: string; rule?: string; reason: string; scope?: string }

interface PolicyView {
  mode: string; sandbox: { tier: string; network: boolean; backend?: string };
  managed: boolean;
  rules: { decision: "deny" | "ask" | "allow"; rule: string;
           layer: "managed" | "user" | "settings" | "workspace" | "flag" | "default" | "session" | "builtin";
           applied: boolean; ignoredBecause?: "workspace-untrusted" | "managed-override";
           note?: string }[];                     // why a session deny rule was pinned
}
```

- A configured rule's `layer` is the one loading credited it with, as
  `/permissions` shows it: `default` is a built-in rule, `builtin` an engine
  step that decides before any rule (`builtin:<step>`), and `session` a rule
  the session added for itself, or a deny rule it pinned (`note` says why).
  A rule set aside by a managed lock names the layer its file was read as.
- `explain` is a dry run: not recorded, not cached, and it never changes the
  per-session "always" scopes. Values shown are redacted.
- **Not implemented in 1.2.2:** `Result.Rule` in the policy engine, the rule on `action.*`
  events and on asks, the layered rule list and `explain`.
- Editing the user's rules is not an ACP method: Studio's main process runs
  `abhed config set --scope user …` and asks natively for widening keys.

### 5.5 Diffs on asks

**Not implemented in 1.2.2.** For `edit` and `write`, the engine computes the diff before it
asks and puts it in the ask's `content` as spec `{type: "diff", path, oldText,
newText}` and in `locations`. For files over 256 KiB only the changed hunks
are sent, with `meta.hunksOnly: true`.

### 5.6 Workspace trust

| | |
|---|---|
| Wire today | `session/new`, `session/load`, `session/resume` and `_abhed/session/fork` accept `_meta["zybuu.ai/abhed"].trust: "untrusted"` only (tighten-only; an empty value counts as absent, anything else is -32602) and return `_meta["zybuu.ai/abhed"].workspaceTrust = {workspace, file?, sha256?, trusted, reason, applied[], ignored[{key, value?, reason?}]}`. The legacy `_meta.abhed.trust` is still accepted on input; replies use only `zybuu.ai/abhed`, and both keys at once are -32602. |
| Added in 1.2.3 | `_abhed/trust/inspect {cwd}` → the same object plus `agents[{name, sha256}]`, `skills[{name, dir}]`, `commands[]` and `mcp[]` the file would bring. Notification `_abhed/trust/changed {cwd, oldSha256, newSha256}` when the file's bytes change during a session; the next prompt restarts the session under the new decision. |
| Studio | A banner, then a review editor built from `inspect`, then **Trust this exact file**, which asks in a native dialog raised by Studio's main process (path and SHA-256 shown) and then runs `abhed trust grant -sha256 <H> <dir>` with the bundled, hash-checked engine binary. |
| Security | There is no grant method, now or later. A grant is pinned to the reviewed hash, so a file changed after review is not trusted. Studio's own Restricted Mode must send `trust: "untrusted"` on every method that opens a session, a continued one included; up to 1.2.6 the engine ignored it on load and resume. |

---

## 6. Orchestration

### 6.1 Subagents in the foreground

The `task` tool's spawn and return reach Studio two ways:

- **As cards.** **Not implemented in 1.2.2:** a `tool_call` with `toolCallId: "sub-<child
  session>"`, `kind: "think"`, title `subagent <type>: <description>`, and
  `meta = {subagent: {session, agentType, depth, model, provider?, branch?,
  definitionSha256?}}` when `subagent.spawned` is recorded; a
  `tool_call_update` with the summary length, turns and tokens on
  `subagent.returned`.
- **As events.** `subagent.spawned`, `subagent.returned`, `subagent.ask`,
  `subagent.action` and `subagent.notice` on the event stream (§4.4).

A subagent's asks already arrive as cards with `meta.subagent` and
`toolCallId: "subagent-<requestId>"` (done). One ask at a time across the
whole tree.

### 6.2 Background tasks

A background task is a subagent started with `background: true`. ACP runs
the wake mode the configuration gives, `auto` by default: a result that
arrives between prompts opens a **woken turn** that acts on it. `notify`
(configured, or forced by the managed configuration) only delivers the
result to the conversation for the next prompt.

A woken turn is a turn the editor did not prompt:

```ts
_abhed/wake/started { sessionId, taskIds: string[] }     // notification
// then the turn's session/update notifications, as a prompt's; the first is
// an agent_message_chunk "Continuing with results from <task>." with
// _meta["zybuu.ai/abhed"].woken = { by: "policy", taskIds }
_abhed/wake/ended   { sessionId, taskIds, stopReason, reason? }  // notification
```

- Its asks go to the editor as a prompt's do; nothing is approved for it.
- `session/cancel` ends it and holds further wakes until the next prompt.
- A `session/prompt` sent while it runs waits for `_abhed/wake/ended`, then runs.
- Only the session's own tasks finishing start one, within
  `subagents.max_wakes_per_hour` and `subagents.wake_max_turns`
  (`wake_limit` maps to `max_turn_requests`). None starts while the
  workspace file changed since the session opened; the next prompt decides.

**Today (done in `app/acp.go`):**
- `subagent.spawned` with `background: true` opens a card
  `toolCallId: "bg-<taskId>"`, `status: "in_progress"`, `meta.taskId`.
- `subagent.notice` completes that card (`completed`, or `failed` for any
  other status) with the result text, whether or not a turn is open.
- `session/cancel` cancels the running prompt **and every background task**,
  with or without a prompt open ("Stop means stop").

**Not implemented in 1.2.2:**

```ts
_abhed/tasks/list   { sessionId } → { tasks: TaskInfo[] }
_abhed/tasks/cancel { sessionId, taskId } → { cancelled: boolean }
_abhed/tasks/resume { sessionId, taskId, prompt: string } → { taskId }   // a finished task, continued
_abhed/tasks/review { sessionId, taskId? } → { released: number }        // §6.3
// notification whenever a task's status, turns or waiting asks change
_abhed/tasks/changed { sessionId, task: TaskInfo }

interface TaskInfo {                 // the SDK's TaskInfo, plus
  task_id: string; description: string; agent_type?: string;
  provider?: string; model?: string;
  status: "running" | "completed" | "failed" | "cancelled" | string;  // or the cap that ended it
  reason?: string; turns?: number; started: string; summary?: string;
  tokens_in?: number; tokens_out?: number;
  waiting_asks: number;              // asks held for a person
  notice?: { delivery: "boundary" | "idle" | "wake"; content_chars: number };
  branch?: string;                   // worktree isolation
  kind: "task" | "shell";            // "shell": a bash run_in_background command
  command?: string; exit_code?: number; output_bytes?: number; last_line?: string;  // shells; redacted
}
```

- `description`, `summary`, `command` and `last_line` are model- or
  tool-written: they arrive redacted and with control and hidden characters
  shown as marked escapes (`⟨\e⟩`), the one-line fields with `⟨\n⟩` too.

- A background shell is listed and cancelled as a task: its `status` is
  `running`, `exited` or `killed`, and `_abhed/tasks/changed` is sent when it
  starts and ends (`shell.started`, `shell.ended`). `resume` and `review` do
  not apply to shells.

- `cancel` is recorded as the person's stop (`user_interrupt`).
- `resume` is the `task` tool's `resume` argument used by a person. It is a
  prompt, so it waits for the session to be idle (-32002 otherwise) and is
  recorded as a `user.message` to that child.
- **Security:** no auto-approval while idle; stopping the chat stops every
  task; a task never outlives `session/close` (its end is recorded
  `session_closed`).

### 6.3 Held asks and reviewing them

An ask from a background task while no prompt turn is open cannot be put to
the editor at once. Today (done) the engine marks the task's card with a
`tool_call_update` "Waiting for your approval; send a message to review it",
holds the ask until the next prompt opens (up to 30 minutes, then it is
refused and recorded `by: system`), and then sends the ordinary
`session/request_permission`.

**Not implemented in 1.2.2:** `_abhed/tasks/review {sessionId, taskId?}`, a person's
request to see the held asks now, without sending a prompt:

1. The engine opens a *review window* for the session and sends each held ask
   (of `taskId`, or all) as a normal `session/request_permission`, with
   `meta.held: true` and `meta.taskId`. It replies `{released: n}`.
2. The window stays open while asks are outstanding and for 5 minutes after
   the last answer; an ask a task makes during the window goes out at once
   with `held: true`.
3. Answers follow §5.2 exactly: bound option ids, a person's click only, the
   destructive modal, "always" only when offered.
4. A window closes on `session/cancel`, on `session/close`, or when a prompt
   opens (asks then go out inside the turn as today).

Studio's engine connection routes a permission request with `meta.held:
true` to its Tasks view, never to a chat turn, and answers `cancelled` if no
Tasks view is showing it. It never answers a held ask by itself.

### 6.4 Capabilities: agents, skills, MCP, extensions and the rest

`_abhed/capabilities {sessionId}` returns what this session can reach. It is a
superset of the server's `/v1/capabilities`, built by the same code.

```ts
interface Capabilities {
  model: { name: string; context_window: number };
  policy: PolicyView;                                        // §5.4
  tools: { name; description; mutates: boolean; source: "builtin" | "mcp" | "skill"; server?: string }[];
  agents: { name: string; source: "builtin" | "managed" | "user" | "workspace";
            sha256?: string; model?: string; tools: string[]; description?: string;
            trusted: boolean; refused?: string; path?: string }[];
  skills: { name; description; source: "user" | "workspace" | "managed";
            has_pipeline: boolean; allowed_tools?: string[]; dir?: string;
            trusted: boolean; refused?: string }[];
  mcp: { name: string; transport: "stdio" | "http" | "sse";
         status: "connected" | "starting" | "stopped" | "error"; error?: string;
         tools: string[]; source: "user" | "workspace" | "managed";
         pinned: boolean;                                   // digest-pinned command or image
         refusedTools?: string[] }[];                       // offered, not registered for their name
  extensions: { name: string; events: string[];            // Abhed hook extensions
                status: "running" | "stopped" | "not started"; error?: string; source: string }[];
  web: { search: boolean; fetch: boolean; allowed_hosts: string[]; ask: boolean };
  infra: { clusters: { name; context; logged_in: boolean }[]; ssh_hosts: string[] };
  secrets: { name: string }[];                               // names only
  index: { enabled: boolean; files?: number; chunks?: number; updated?: string; embedder?: string };
  rag: { name: string; docs: number }[];
  memory: { path: string; scope: "user" | "workspace" | "managed" | "auto"; bytes: number; loaded: boolean }[];
  memory_auto: boolean;                                      // opt-in, off by default
  studio: { host_terminal: boolean };                        // managed may set false
}
```

- **Refreshing.** Studio refetches when the event stream shows
  `mcp.status`, `extension.status`, `hook.fired`, `memory.loaded`,
  `memory.written` or a trust change, and when the person asks.
- `_abhed/mcp/restart {sessionId, name}` → `{status: "connected" | "error",
  error?}`, recorded `mcp.status {server, op: "restart", status, error?, by:
  "user"}`. A name that is not a configured, enabled server is -32602; while
  a prompt, a woken turn or another restart runs in the session it is
  -32002, and a prompt or wake waits for a restart in the same way. The
  restarted server lives as long as the session, not the request. `error` here and in `mcp[]` is
  redacted text; `refusedTools` names are untrusted text with hidden
  characters marked.
- `_abhed/index/status {sessionId}` and `_abhed/index/rebuild {sessionId}`
  (recorded `by: user`; progress as events).
- `_abhed/infra/status {sessionId}` is `infra` alone, for polling.
- **Client MCP servers are refused.** `session/new` must be sent
  `mcpServers: []`; a non-empty list is not used and the reply's
  `meta.mcpServersRefused` names them. MCP servers are configured in the
  engine's configuration, under workspace trust.
- **Security:** read-only, redacted. Untrusted workspace agents, skills,
  commands and MCP servers are listed with `trusted: false` and `refused`, so
  Studio can say why they are missing. Abhed hook extensions can veto, never
  permit. Secret values never appear.

**Engine today:** none over ACP; the server has models, sandbox, permissions,
tools, skills, MCP and extensions. **Not implemented in 1.2.2:** the method and every field
above that the server lacks.

### 6.5 Pipelines

A pipeline step is a normal tool call. **Not implemented in 1.2.2:** `meta.via:
"pipeline <name> step <n>"` on its `tool_call` and on its ask. Each step goes
through policy; a step needing approval in a headless run is refused.

---

## 7. The person's own actions

### 7.1 The Abhed terminal (sandboxed)

```ts
_abhed/terminal/create { sessionId, mode: "lines" | "interactive", cols, rows }
  → { terminalId, tier: string, network: boolean, recorded: true }
_abhed/terminal/input  { terminalId, data: string }     → {}
_abhed/terminal/resize { terminalId, cols, rows }       → {}
_abhed/terminal/kill   { terminalId }                   → {}
// notifications
_abhed/terminal/output { terminalId, data: string }     // base64 of the bytes
_abhed/terminal/exit   { terminalId, code?: number, signal?: string }
// engine → client request: confirm a destructive command
_abhed/terminal/confirm { terminalId, command, reason } → { confirmed: boolean }
```

- The shell runs in the engine's sandbox tier for the session. In `lines`
  mode each line is a `bash` call through policy; in `interactive` mode the
  shell runs whole under the sandbox and each entered line is recorded
  `terminal.input {line | withheld, by: "user"}`.
- A line the terminal did not echo (`read -s`, password prompts) is recorded
  as withheld, never as text. So is a line entered before the shell was back
  at its prompt, since what reads it may turn echo off after it arrived, and
  a pasted line still being read with echo off when it is judged. The web
  IDE's interactive terminal does the same. Only the process and none tiers
  can ask the terminal; in a container a typed-ahead line is judged by its
  echo alone.
- A line typed while a program has the terminal is told from one typed at the
  shell's prompt by the terminal's foreground process group: the shell runs
  with job control (`bash -i -m`), so each program gets a group of its own.
  The engine logs to stderr the shell's group, the first time a program has
  the terminal, and bash's notice when it has no job control; lines typed
  into a program are then judged as the shell's.
- `confirm` is answered only from a modal whose default is No.
- In `lines` mode the engine draws the prompt (the workspace folder and the
  path under it, then `$`), echoes typed text in bold, and shows each line's
  output followed by its dimmed `exit N · time`. It edits the line itself:
  Backspace deletes, and a key that sends an escape sequence (the arrows,
  Home, End, function keys, Alt with a key, a paste's bracketed-paste
  markers, a terminal's replies) is dropped whole, never typed. There is no
  history or cursor movement; an Esc alone at the end of an input is the Esc
  key, and a line break ends a sequence left unfinished. Lines entered while
  one runs wait their turn; Ctrl-C stops the one running and drops those
  waiting, as a shell flushes its input on an interrupt, and the terminal
  says `abhed: N typed-ahead lines were not run`.
- Studio's ACP `terminal` client capability stays `false`: the agent never
  runs commands in Studio's terminals.

**Not implemented in 1.2.2:** all of it, by moving the server's pty and line capture into a
shared package so the server and ACP use the same code.

### 7.2 The host terminal

Studio also offers its own host shell, labelled **"not sandboxed, not
recorded"** in its name, its banner and its tab. It is not an engine feature
and has no ACP method: the engine cannot sandbox or honestly record a shell it
does not run. A managed policy removes it: when `capabilities.studio.
host_terminal` is `false`, Studio does not offer the profile and refuses to
open one. The sandboxed Abhed terminal is the default profile.

### 7.3 Manual edits and dirty buffers

- `_abhed/manual/edited {sessionId, path, beforeSha256, afterSha256, patch}`
  → `{}`, recorded `manual.edit {by: "user"}`. Record-only: a person's save on
  their own machine is not policy-gated.
- `_abhed/buffers/dirty {sessionId, paths: string[]}` → `{}`: the files with
  unsaved changes in Studio. The engine refuses an agent edit to one of them
  ("the person has unsaved changes to this file").
- **Security:** paths are canonicalised by the engine and must lie in the
  session's workspace or added directories; others are dropped.

### 7.4 Per-hunk review

```ts
_abhed/review/list     { sessionId } → { files: { path, status: "modified" | "added" | "deleted", hunks: number }[] }
_abhed/review/baseline { sessionId, path } → { text: string, sha256 }
_abhed/review/accept   { sessionId, path, hunk?: number } → { remaining: number }
_abhed/review/reject   { sessionId, path, hunk?: number } → { remaining: number }
// notification
_abhed/review/changed  { sessionId, paths: string[] }
```

- `accept` records `change.accepted {path, hunk?, by: "user"}` and moves the
  baseline.
- `reject` writes the baseline back through the engine's manual path, as a
  policy-checked write recorded `by: "user"`: a protected path stays
  protected.
- Studio's renderer never writes a file. There is no auto-accept delay.

### 7.4.1 Taking a subagent's worktree back

```ts
_abhed/worktrees/list { sessionId } → { worktrees: {
  id, rel, branch, base,            // .abhed-worktrees/<id>, abhed/<id>, the shared commit
  files: { path, status: "modified" | "added" | "deleted", whole?: boolean, unsupported?: string,
           hunks: { old_start, old_count, new_start, new_count, old: string[], new: string[],
                    before?: string[], after?: string[] }[] }[] }[] }
_abhed/worktrees/take { sessionId, worktree, accept: { [path]: number[] }, remove?: boolean }
  → { taken: string[], conflicts: string[], refused: string[], left: number, removed: boolean }
```

- Listed under the `worktrees` feature. Paths are the workspace's, slash
  separated; hunk lines keep their newlines and are redacted as other text
  the engine sends. A file a read rule withholds, in the workspace or the
  worktree, has no hunks and `unsupported: "denied by policy"`.
- `accept` names hunks by their index in the list. The changes are read again
  at the take, so a stale list cannot put in a hunk the worktree no longer
  holds; an index or path it no longer has is `refused`.
- Each file taken is the person's write (or delete) through the engine's
  manual path, as `reject` is: policy-checked, checkpointed, recorded
  `by: "user"`; the editor's protected files are refused. A hunk the
  workspace changed at, or next to, is a `conflict`, left out, never forced.
- `remove` removes the worktree and its branch after the take, as the
  person's recorded delete of its folder; what was not taken is lost.
- The take is recorded as `worktree.taken`, and `_abhed/review/changed`
  names the files written. Refused while a prompt runs, like `reject`.

### 7.5 Checkpoints and undo

`_abhed/review/undoTurn {sessionId, turn}` → `{restored: {path, beforeSha256,
afterSha256}[]}`. Each file is restored through the engine's manual path and
recorded `file.restored {by: "user", checkpoint}`. Pre-images live in the
record's content-addressed blob store, so undo works after a restart. A
conversation rewind is `_abhed/session/fork` at the turn's seq.

### 7.6 Queue, steer and stop

- `_abhed/session/steer {sessionId, text}` → `{}`: "send now" while a prompt
  runs; recorded as a `user.message` with `meta.steered: true`.
- `_abhed/queue/list {sessionId}` → `{items: {id, text, queued}[]}` and
  `_abhed/queue/cancel {sessionId, id}` → `{}`.
- Stop is spec `session/cancel`, which also cancels background tasks (done).

### 7.7 Resolve an issue

`_abhed/resolve {cwd, issueUrl}` → `{sessionId}` starts a session running the
resolve flow under policy. Pushing and opening a pull request are asks. The
forge token comes from the secrets vault by name. **Not implemented in 1.2.2.**

---

### 7.8 Debugging (Run and Debug)

```ts
_abhed/debug/launch { sessionId, language: "go" | "python",
                      program?: string, test?: string,   // exactly one; inside the workspace
                      args?: string[], cwd?: string,      // cwd inside the workspace
                      env?: { [name]: string } } → { debugId }
_abhed/debug/send   { debugId, message: DapRequest } → {}   // passed on in the order sent
_abhed/debug/stop   { debugId } → {}
// notifications
_abhed/debug/message { debugId, message: DapMessage }    // every adapter message Studio is meant to see
_abhed/debug/status  { debugId, state: "launched" | "failed" | "note" | "ended", message?, problem?, dropped? }
```

- Listed under the `debug` feature. The adapter runs in the session's sandbox
  (container or `vm` tier, or the Linux process tier with its network closed
  or allowlisted, where bubblewrap gives it its own network namespace; with
  the network open its loopback would be the host's); elsewhere `launch`
  fails with `-32000` and `data.problem` `{kind: "tier-unsupported", message,
  steps, retry}`. Go also needs the bundled Linux helper (or `relay_binary`) as its socket bridge (`kind: "bridge-missing"`);
  bad params are `-32602` with `kind: "invalid-request"`.
- The program or test, its args, cwd and env make the debuggee's command line
  (`dlv debug|test …`, `python3 …`, `python3 -m pytest …`), which is put to
  policy and the supervisor as the person's `bash` call and recorded through
  the manual path (`action.requested` by user, its decision, and an
  `observation` when the session ends); a refusal is `-32001`. A policy ask
  is taken as answered, since the person asked for the launch; an ask the
  supervisor raises is sent to Studio as a normal
  `session/request_permission` (`toolCall.title` `debug: <command line>`,
  `meta.step: "monitor"`, `meta.debug: true`, options Allow once and Deny,
  no "always"), and the launch goes ahead only on Allow; Deny, a cancel or no
  answer is `-32001`. Then
  `debug.launched` and, at the end, `debug.stopped` are recorded.
- Studio sends DAP requests with `send`: `initialize` first, then breakpoints,
  `configurationDone`, stepping and the rest. The engine sends the adapter's
  `launch` itself once `initialize` is answered and keeps its response:
  Studio sees `status: launched` or `status: failed` instead; a failed one's
  `message` carries the adapter's error, its `body.error.format` with the
  variables filled in, up to 2,000 bytes, as untrusted text. Every `status`
  `message` is bounded the same way. Only the first response to the
  engine's launch is taken as its verdict. Studio's own
  `launch`, `attach`, and a `restart` with arguments are answered with a
  failed DAP response in a `message`; a non-request is `-32602`. The
  engine reads `seq`, `type`, `command`, `request_seq`, `success`,
  `message`, `arguments`, `event` and `body` by their exact keys: a message
  that repeats one, or has a key equal to one under Unicode case folding
  without being it (`Command`, or `argumentſ` with U+017F, which Go's
  encoding/json in Delve would read as `arguments`), is
  `-32602` from Studio, and from the adapter is dropped, never passed on,
  and counted (`dropped` in the final `status` and in `debug.stopped`).
  `send`s are passed to the adapter in the order the engine read them;
  Studio need not wait for each reply, and every `send` is answered, those
  still queued when the session ends with an error. A header line over 1 KiB
  or a header over 8 KiB from the adapter, or a message it cannot read, ends
  the session at once: the adapter is ended, `status: failed` says why and
  `debug.stopped` records `failed`. A launch whose supervisor question is
  answered after its session closed is not started (`-32000`).
- The adapter's `runInTerminal` and `startDebugging` never reach Studio: the
  engine answers them with an error (`status: note`); `console` is always
  `internalConsole`.
- A missing debugger ends the session at once with `status: failed` and
  `problem.kind: "adapter-missing"` and its steps. `stop` sends `disconnect`
  (`terminateDebuggee`) and ends the adapter after three seconds; closing the
  session or the connection stops its debug sessions.
- The session also ends by the protocol: once the adapter answers a
  `disconnect` Studio sent, or reports `terminated` or `exited` after Studio's
  `terminate`, the engine closes the adapter's input (debugpy's adapter waits
  for that), ends it if it has not gone within three seconds, sends
  `status: ended` and records `debug.stopped` with `reason: "ended"`. Output
  events pass as the adapter sends them; nothing is merged or held back.
- At most four per connection, counting launches still being judged; each
  ends after eight hours. A session closed with its ACP session or the
  connection records `debug.stopped` with `reason: "closed"`; `stop` gives
  `stopped`.
- **Studio treats every `_abhed/debug/message` as untrusted sandbox output.**
  The adapter's output can be written by the debuggee, which is workspace
  code. Studio never acts on a reverse request or a custom event from it
  (`runInTerminal`, `startDebugging`, `debugpyAttach` or any other), even
  though the engine already refuses and filters these; it renders text as
  text and opens no path or URL from it without the person's action.

### 7.9 Language servers

```ts
_abhed/lsp/start { sessionId, language: "go" | "python" | "typescript" | "javascript" | "rust" | <a sandbox.language_servers name>,
                   capabilities?: ClientCapabilities }   // the editor's; a broad default when absent
  → { lspId, capabilities: ServerCapabilities }
_abhed/lsp/send  { lspId, message: <LSP JSON-RPC message> } → {} | { refused: string }
_abhed/lsp/stop  { lspId } → {}
// notifications
_abhed/lsp/message { lspId, message: <LSP JSON-RPC message> }
_abhed/lsp/status  { lspId, state: "restarted" | "ended", capabilities?, restart?, reason?, message? }
```

- Listed under the `lsp` feature. One server per session and language: a
  second `start` returns the running one, waiting for it to initialize (or
  failing as it did). At most four per connection.
- The server runs in the session's sandbox on any tier but `none`; its
  command (operator-named, else the language's default) is judged as the
  person's `bash` call (`-32001` when refused) and recorded as `lsp.started`;
  a missing server is `-32000` with `data.problem.kind: "server-missing"`,
  whose steps start with the toolchain route (`_abhed/toolchain/install`
  with `lsp: true`, §3.2b) where Abhed installs that server.
  A supervisor's ask on the start goes to Studio as a normal
  `session/request_permission` (`meta.lsp: true`, `meta.step: "monitor"`,
  Allow once or Deny); it starts only on Allow. For rust-analyzer, a
  `rust-analyzer.toml` or `.rust-analyzer.toml` in a root, a folder under
  it or a folder above it overrides the engine's settings, so the start is
  put to the person the same way (`meta.step: "workspace"`, the reason
  naming the files; no answer is Deny) even when policy would allow it, and
  the answer is recorded. The server runs as
  `exec env GOTOOLCHAIN=local GOFLAGS= GOENV=off <command>`.
- The engine sends `initialize` (workspace folder, the given capabilities,
  and the engine's own `initializationOptions`, never Studio's) and
  `initialized`; Studio must not. File URIs are the host's paths: the sandbox
  shows the workspace at its own path. The engine's settings, sent there and
  as its answers to `workspace/configuration` whatever Studio replies:
  - rust-analyzer: `cargo.buildScripts.enable`, `procMacro.enable`,
    `checkOnSave` and `cargo.autoreload` off;
  - typescript-language-server: `tsserver.path` set to the TypeScript
    installed beside the server, never the workspace's. A shell step before
    the server looks only under the server's own install
    (`<bin>/../lib/node_modules/typescript-language-server`, Abhed's layout,
    and the package two folders above the command's link target, npm's),
    at `<pkg>/node_modules/typescript/lib/tsserver.js` and
    `<pkg>/../typescript/lib/tsserver.js`, links resolved; no module
    search, `HOME` or `NODE_PATH`, and a result under `HOME` is dropped.
    The engine refuses a result under a session root or the host's home
    folder. No result within 15 s, or the process ending first: it is
    killed and the start fails, `server-missing`;
  - gopls, basedpyright: their defaults (`null`).
  What each may still run inside the sandbox: gopls runs `go list` (and, for
  cgo packages, the cgo tool and the C compiler), no project code;
  rust-analyzer runs `cargo metadata`, which reads the workspace's
  `.cargo/config.toml` and can run a `rustc` or `rustc-wrapper` it names;
  typescript-language-server runs its own TypeScript; basedpyright runs the
  Python interpreter it finds (the sandbox's, or one a `pyrightconfig.json`
  names) to read import paths. An operator's `sandbox.language_servers`
  command is the operator's choice; the engine's settings still apply by
  language.
- `send` passes Studio's message through the filter, which takes only the
  standard LSP 3.17 client methods (and a server's own methods reviewed as
  running nothing; none yet): refused with `{refused}` (and, for a request,
  an LSP error answer in a `message`) for any other method (rust-analyzer's
  `experimental/runTest`, `rust-analyzer/runFlycheck`,
  `rust-analyzer/rebuildProcMacros` and `rust-analyzer/reloadWorkspace`
  among them), `initialize`, `initialized`,
  `workspace/didChangeConfiguration`, a `workspace/executeCommand` not on
  the server's safe list, a URI outside the session's roots, or an answer to
  nothing the server asked. Answers to `workspace/configuration` reach the
  server as the engine's settings above. A refused answer to a request the
  server is waiting on also sends the server an error answer for that id.
- URIs are checked in URI positions only (`uri`, `oldUri`, `newUri`,
  `targetUri`, `rootUri`, `scopeUri`, `baseUri`, gopls's `URI` command
  argument, the keys of a WorkspaceEdit's `changes` and of a report's
  `relatedDocuments`, and `rootPath`): one there that is not an absolute
  `file:` URI (a bare or percent-encoded path, another scheme), is under a
  root's `.abhed` folder, or leads out of the roots through a link (the
  deepest existing part of its path resolved) is outside. Links (`target`,
  `href`) may also be `http` or `https`; a `file:` link is checked as a URI
  and any other scheme is outside. Document text that starts with `file:`
  is content, not a URI.
- From the server: `window/showDocument` for a non-file or outside URI is
  answered by the engine (`success: false`); in notifications and answers
  each array item naming an outside file (and each outside key of
  `relatedDocuments`) is dropped and the rest passes; a
  notification outside as a whole is dropped, such an answer (or error
  answer) reaches Studio as `null` (a `null` result). A WorkspaceEdit (an
  object with `changes` or `documentChanges`) is never trimmed: with any
  file or file operation outside or in `.abhed`, it is outside as a whole,
  so a rename's answer is `null` and a code action carrying it is dropped
  from its list; an edit is applied whole or not at all. A request
  naming one, a `workspace/applyEdit` whose create, rename, delete or edit
  is outside or in `.abhed` included, is answered with an error and never
  reaches Studio; a request reusing an id still waiting for Studio's answer
  is answered with an error. Everything else passes to Studio.
- A crash restarts the server (backoff 1, 2, 4, 8, 16 s, at most five), judged
  and initialized again: `status: restarted` with its capabilities, after
  which Studio opens its documents again. Between the crash and the restart
  a `send` is `-32002` ("restarting"), not queued. `stop` sends `shutdown`
  and `exit` and ends it after three seconds. `lsp.stopped` records the
  reason (`stopped`, `closed` with its session or connection, `crashed`,
  `failed`) and the counts of messages passed and refused; requests are not
  recorded singly.
- Messages are read by their exact keys: one with a key twice, or two keys
  equal under Unicode case folding (`ſ` is `s`, `K` is `k`, as Go's
  encoding/json matches them), anywhere in it, is refused from Studio and
  dropped from the server. Header lines and their number are bounded, a
  message is at most 8 MiB and nests at most 256 deep.
- Studio's `send`s reach the server in the order they were read, through one
  queue per server (at most 256 messages or 32 MiB waiting; past that
  `-32002`, busy) and one writer; a server with more than 1,024 messages or
  32 MiB it has not read is ended.

### 7.10 Inline completions

```ts
_abhed/complete { sessionId, uri, position: { line, character }, prefix: string, suffix: string,
                  language?: string, trigger?: "auto" | "invoked", seq?: number,
                  context?: { uri, text }[] }        // other open files, optional
  → { items: { text: string, range?: Range }[], disabled?: string, refused?: string, limited?: string, cancelled?: string }
_abhed/complete/cancel { sessionId } → {}
```

- Listed under the `complete` feature; whether completions are on is in each
  answer: `disabled` names why not (no `model.completion`,
  `completion.enabled: false`, a provider the configuration does not offer,
  a managed `model.default` pin the managed file does not extend to
  `model.completion`, or `completion.local_only` with a remote model).
  Nothing is sent then. Only the provider's own key is sent.
- The engine sends at most the last 6,000 characters of `prefix`, the first
  2,000 of `suffix`, and the first 60 lines of up to three `context` files,
  4 KiB of each and 12 KiB in all, cut between characters. A document or
  context file outside the session's roots, one a read rule denies, one that
  looks like keys, or Abhed's state is not sent (`refused`); every path form
  (as named, links followed) is checked. Stored secrets and key-shaped text
  are withheld from the whole of each text before it is cut, and from
  `prefix`+`suffix` as one so a secret the cursor splits is withheld on both
  sides; placeholders (`%s`, `${…}`, `$NAME`, `{…}`, `<…>`, masks) are not
  secrets. A reply that carries one is dropped (empty `items`).
- One request in flight per session: a newer one cancels it (`cancelled`).
  `seq`, increasing per session, orders requests; one at or below the newest
  seen is dropped (`cancelled`). Without it, arrival order. A short debounce;
  at most five a second per session and ten per connection (`limited`). A
  model error is `-32000`; a closed session's request is refused with
  `-32602`.
- Fill-in-the-middle on the completions endpoint for local server types and
  `openai-compatible`, else a chat turn; a 404, 405 or 501 from the
  completions endpoint, or a 400 saying it or `suffix` is not supported,
  switches the session to chat. Each request is sent once, no retries; under
  `local_only` no provider follows a redirect off the machine. Text only, no
  tools.
- Recorded as `completion.enabled` just before the first request is sent,
  and again with `mode: "chat"` before a fallback's first chat request (if
  it cannot be recorded, nothing is sent) and `completion.summary` when
  the session closes, which counts `superseded` (replaced before sending)
  apart from `cancelled` (after); single completions are not recorded.

## 8. Health, setup and memory

### 8.1 Doctor

`_abhed/doctor {cwd?, _meta?: {trust?: "untrusted"}}` → `{checks: {id, title, status: "ok" | "warn" | "fail",
detail}[]}`, the same checks as `abhed doctor --json`: configuration, trust,
provider reachability (loopback only unless configured), sandbox tier,
record store and verification, MCP, index, managed policy. Output is
redacted. **Not implemented in 1.2.2:** both.

### 8.2 Setup

Setup is not an ACP method: it runs before a session can exist. Studio's main
process runs `abhed setup --json …` (writes `~/.abhed/config.json`
atomically, validates, runs doctor) and `abhed secret set NAME` in an Abhed
terminal for a hosted provider's key. Providers name keys by
`api_key_secret: NAME`, resolved from the vault, because an app launched from
the Dock has no shell environment. **Not implemented in 1.2.2:** `setup --json`,
`api_key_secret` and `config get|set --scope user --json`, which classifies
each key as tightening or widening.

### 8.3 Memory files and auto memory

- `capabilities.memory` lists the loaded memory files.
- **Auto memory is opt-in.** `memory_auto` is `false` unless the user's
  configuration turns it on; a managed policy can force it off, and a
  workspace can only turn it off. While it is off the engine writes no memory
  file of its own.
- A person's edit to a memory file in Studio is a manual edit (§7.3). The
  agent's writes to memory are policy-checked tool calls, recorded
  `memory.written {path, kind, by}`.

---

## 9. Enterprise: the team server (EE only)

`_abhed/team/*` is reserved for the Enterprise edition; a CE engine answers
-32601 and CE Studio shows nothing for it.

---

## 10. Event types Studio reads

Studio's Events and HawkEYE views read these types from the stream. Types
marked *new* are added by the CLI's shared interface work; Studio only reads
them.

| Type | Actor | Payload (main fields) |
|---|---|---|
| `session.started`, `session.ended` | system | reason on end |
| `session.resumed` | system | `via`, `through_seq`, `workspace_trust` |
| `user.message` | user | text; `steered?` |
| `agent.message`, `agent.delta` | agent | text |
| `agent.reasoning`, `agent.reasoning.delta` | agent | text |
| `action.requested` | agent | call_id, tool, args, subject, `rule?` |
| `action.approved`, `action.denied` | system/user | call_id, by, step, reason, scope, `rule?` |
| `observation` | tool (untrusted) | call_id, tool, content, is_error, exit_code |
| `subagent.spawned`, `subagent.returned` | agent | session, description, agent_type, depth, model, provider, background, task_id, branch |
| `subagent.ask`, `subagent.action` | system | request_id, tool, decision, reason |
| `subagent.notice` | system | task_id, status, content, delivery, wake |
| `session.woken`, `session.wake_set` | system | |
| `compaction.started`, `compaction.completed` | system | before, after |
| `context.offloaded` | system | results, before, after |
| `plan.updated`, `todo.updated` | agent | items |
| `model.call`, `model.switched` | system | tokens, latency, model; `purpose` (`suggestion`) on a call outside the conversation |
| `suggestion.offered` | system | text, turn |
| `change.accepted` | user | path, hunk |
| `conversation.forked` | user | from, through_seq |
| `terminal.input` | user | line or withheld |
| `monitor.verdict` | system | verdict, before, after (shown when present) |
| *new* `supervisor.correction` | system | call_id, code, decision, text |
| *new* `supervisor.replan` | system | failures, text |
| `message.dropped` | system | |
| *new* `mode.changed` | user | from, to, via |
| *new* `permission.changed` | user | op, list, rule, scope |
| *new* `workspace.dir_added` | user | path, access |
| *new* `command.invoked` | user | name, source, sha256 |
| *new* `memory.loaded`, `memory.written` | system/agent | files / path, kind |
| *new* `session.named`, `session.branched` | user | name / from, through_seq |
| *new* `file.restored` | user | path, before_sha256, after_sha256, checkpoint |
| *new* `plan.proposed`, `plan.decided` | agent/user | text / decision |
| *new* `model.fallback` | system | from, to, reason |
| *new* `hook.fired` | system | extension, event, verdict |
| *new* `record.repaired` | system | reason, truncated_bytes |
| `mcp.status` | user | server, op (`restart`), status, error |
| `ollama.start`, `ollama.install`, `ollama.pull` | user | op, outcome (`ok`, `failed`, `declined`, `refused`, `cancelled`, `manual`), provider, model, command, source, bytes, pid, consent, problem, error, surface (§3.2a) |
| Studio's additions: `approval.scope_granted`, `manual.edit`, `trust.changed`, `extension.status`, `task.cancelled`, `team.connected` | as named | as in the sections above |

An unknown type is shown with its raw payload as text. Studio never infers
meaning from a type it does not know.

---

## 11. The whole surface at a glance

"Engine 1.2.4" is what the engine serves. Where it differs from a section
above, the engine is right (§ intro) and the difference is listed after the
table.

| Capability | Wire | Kind | Engine 1.2.4 |
|---|---|---|---|
| Handshake, features | `initialize` + `meta`, `abhed version --json` | spec + meta | done |
| Chat, thinking, plan, usage | `session/prompt`, `session/update` | spec | done: reasoning deltas, stop reasons from the terminal reason, usage `meta`; no `cost` (no price table) |
| Next-prompt suggestion | `agent_message_chunk` + `_meta.suggestion` | spec + meta | done (§3.4a) |
| Checked answers | `agent_message_chunk` + `_meta.verdict` | spec + meta | done (§3.4b) |
| Models | `session/set_config_option` (`model`) | spec | done; `config_option_update` on `model.fallback` |
| Thinking level | config option `thought_level` | spec | not yet: never offered, as no provider reports reasoning control |
| Slash commands | `available_commands_update` | spec | done (see note 3) |
| Modes | `session/set_mode`, `current_mode_update`, config option `mode` | spec | done |
| Approvals | `session/request_permission` | spec + meta | done: `rule`, `via`, `diff`, `held`, `taskId`; `approval.scope_granted` |
| Policy view, explain | `_abhed/capabilities`, `_abhed/policy/explain` | ext | done |
| Workspace trust | `session/new` meta, `_abhed/trust/inspect`, `_abhed/trust/changed` | meta + ext | done (see note 6) |
| Sessions | `session/list`, `session/load`, `session/resume`, `session/close` | spec | done (local record only) |
| Rename, fork, compact | `_abhed/session/rename`, `/fork`, `/compact` | ext | done; `compact` refuses a `focus` |
| Event stream | `_abhed/events/subscribe`, `/page`, `/unsubscribe`, `_abhed/event` | ext | done |
| Verify, export | `_abhed/record/verify`, `_abhed/export` | ext | done |
| HawkEYE | `_abhed/hawkeye` | ext | done |
| Subagents | `tool_call` cards + events | spec + meta | done |
| Background tasks | `bg-<id>` cards, `_abhed/tasks/*`, `_abhed/tasks/changed` | spec + ext | list, cancel, review done; `resume` not yet |
| Worktree take-back | `_abhed/worktrees/list`, `_abhed/worktrees/take` | ext | done in the engine (1.2.9; `worktrees` in `features`); Studio's view not yet |
| Debugging | `_abhed/debug/launch`, `/send`, `/stop`, `_abhed/debug/message`, `_abhed/debug/status` | ext | done in the engine (1.2.9; `debug` in `features`): Go and Python; Studio's view is the Studio track's |
| Language servers | `_abhed/lsp/start`, `/send`, `/stop`, `_abhed/lsp/message`, `_abhed/lsp/status` | ext | done in the engine (1.2.9; `lsp` in `features`); the servers come with the toolchains |
| Inline completions | `_abhed/complete`, `_abhed/complete/cancel` | ext | done in the engine (1.2.9; `complete` in `features`) |
| Agents, skills, MCP, extensions, web, infra, secrets, index, RAG, memory | `_abhed/capabilities` | ext | done (see note 7) |
| MCP restart | `_abhed/mcp/restart` | ext | done (1.2.4; `mcp.restart` in `features`) |
| Local Ollama model | `meta.ollama` on a model switch, `_abhed/ollama/*`, `_abhed/ollama/progress` | meta + ext | done in the engine (1.2.9; `ollama` in `features`); Studio's card not yet |
| Index, infra polling | `_abhed/index/*`, `_abhed/infra/status` | ext | not yet (-32601; `index`, `infra` not in `features`) |
| Abhed terminal | `_abhed/terminal/*` | ext | done, both modes (see note 4) |
| Host terminal | none (Studio only), removable by managed `studio.disable_host_terminal` | — | Studio; the managed key is read into `capabilities.studio` |
| Manual edits, dirty buffers | `_abhed/manual/edited`, `_abhed/buffers/dirty` | ext | done |
| Per-hunk review, undo | `_abhed/review/*` | ext | done |
| Queue, steer, stop | `_abhed/session/steer`, `_abhed/queue/*`, `session/cancel` | ext + spec | done |
| Resolve | `_abhed/resolve` | ext | not yet (-32601; `resolve` not in `features`) |
| Doctor | `_abhed/doctor`, `abhed doctor --json` | ext | done |
| Setup, config, trust grant, secrets | engine command line from Studio's main process | — | `trust grant -sha256` and `secret set` exist; `setup --json`, `api_key_secret` and `config get|set` not yet |
| Memory area | `memory` feature | ext | not yet (memory files are in `capabilities`) |
| Team server (EE) | `_abhed/team/*` | ext | CE answers -32601 |
| Deletion | none; `abhed record prune` in a terminal | — | — |

Where the engine differs from the sections above:

1. **Fork (§4.2).** `session.branched` is recorded in the new session, naming
   the source and the last seq taken, as the CLI's branch records it; the
   source's record is not written to, since another process may hold it.
   A fork of a record that failed verification copies it marked untrusted.
2. **Export (§4.5).** `html` is the HawkEYE page, as this page says; the
   CLI's `abhed record export` still writes the transcript page.
3. **Commands (§3.3).** A custom command that runs shell lines, narrows the
   tools or names a model is refused over ACP (the CLI runs it). Skills are
   listed as commands of source `skill`; MCP prompts are not listed.
   `/mode` from a prompt refuses `auto` and `bypass`, which need Studio's
   picker; `/fork` and `/clear` answer that they are Studio's actions.
4. **Terminal (§7.1).** `lines` mode runs each line as the person's `bash`
   call through policy, in the session's sandbox, on a tool session of its
   own; a destructive line asks `_abhed/terminal/confirm`. `interactive`
   runs one shell under the sandbox with the workbench's line capture, now
   shared in `internal/termline`: each line is put to the deny rules at its
   Enter and recorded `terminal.input`, withheld when the terminal did not
   show it. As in the workbench, it is refused where only a line-by-line
   terminal applies every rule (`sandbox.terminal: "lines"`, or a managed
   file that itself sets `permissions.deny` screening `bash`, or sets
   `extensions`), and a destructive line
   there is not confirmed, since the shell, not the engine, runs it.
5. **Undo (§7.5).** `turn` is the turn number the record's
   `checkpoint.saved` events carry. An accepted hunk moves the baseline, so
   undo returns to it.
6. **Trust changes (§5.6).** The file's hash is compared at each prompt and
   each `_abhed/capabilities` call, not watched; on a change the engine sends
   `_abhed/trust/changed` and the prompt restarts the session from its record
   under the new decision.
7. **Capabilities (§6.4).** Skills report `source: "user"`; `rag.docs` is 0;
   clusters report `logged_in: false`; an MCP server is `connected` or
   `error`. `_abhed/tasks/changed` is sent on spawn, result, cancel and a
   change in waiting asks, not on each turn.
8. **Editor files (§2.6).** The file tools refuse `.vscode/**`,
   `.devcontainer/**`, any `*.code-workspace`, and any `.git` with its
   `config` and `hooks/**` at any depth, the config and hooks of the git
   folder a `.git` file names, whether the path exists or not. Names are
   compared without case and links are followed, in the workspace, its added
   folders and a subagent's worktree. Commands are kept from writing those
   paths that exist when the session starts, at the workspace's given and
   resolved paths, and cannot rename or remove `.git`, `.vscode` or
   `.devcontainer`. Nested repositories are searched for at the start, up to
   six folders deep, 64 repositories and 20,000 entries looked at, skipping
   `node_modules`. On macOS the
   sandbox also refuses any `.git`, `.git/config` and `.git/hooks` at any
   depth by pattern, so a repository made later is held and `git init` or
   `git clone` in the workspace fails, and refuses creating a missing
   `.vscode` or `.devcontainer`. On Linux, bubblewrap and containers can hold
   only paths that exist when a command starts, so a command there can still
   create a missing `.vscode`, `.devcontainer` or `.git` or a repository the
   search did not find; a git folder found then that lacks `hooks` or
   `config` gets an empty one, bound read-only. Commands are also kept from
   writing `config.worktree`, `commondir`, `gitdir`, `info/attributes` and
   `objects/info/alternates`, and the config and hooks of linked worktrees'
   and submodules' git folders (`.git/worktrees/*`, `.git/modules/**`): on
   macOS by pattern, made later too; on Linux those that exist, with an
   empty `config.worktree` made where git would read one, and a `commondir`
   in a git folder where git never writes one moved to the quarantine before
   the next command, which is not run. A new `*.code-workspace` can
   be made by a command on every platform.
9. **Modes (§5.1).** A mode changes only between prompts and while no
   background task runs (-32002 otherwise), since the policy engine is read
   by every call.
