# Models and providers

Abhed does not ship a model. It is model-agnostic by construction: the harness
is the same whichever endpoint you point it at, and a better model makes the
same harness better.

`abhed providers` lists what your binary supports — which is what registered
itself at build time, so a build that drops the cloud adapters for an air-gapped
install reports honestly.

## The providers

| Type | Notes |
|---|---|
| `anthropic` | Messages API; explicit prompt caching, thinking blocks |
| `openai` | chat completions |
| `gemini` | generateContent; thought parts |
| `watsonx` | project- or space-scoped |
| `vllm`, `sglang`, `llamacpp`, `tgi`, `ollama` | local servers; accept `top_k`, `min_p`, `repetition_penalty` |
| `groq`, `together`, `openrouter`, `mistral`, `deepseek`, `xai` | hosted gateways |
| `azure-openai` | `base_url` is the deployment endpoint |
| `bedrock-anthropic`, `vertex-anthropic`, `vertex-gemini` | cloud gateways; supply a token |
| `openai-compatible` | generic escape hatch |

Three wire formats cover all of them. The OpenAI-shaped providers share one
adapter and differ only in default URL and which sampler knobs the server
honours; Anthropic and Gemini are genuinely different shapes.

With tools offered, `openai`, `vllm` and `openai-compatible` also send
`"parallel_tool_calls": true`, so the model may ask for several calls in one
turn. Set `"extra": {"parallel_tool_calls": "false"}` on the provider for a
server that refuses the field, or `"true"` to send it to another type.

## Sampling

```json
"params": {
  "temperature": 0.2,
  "top_p": 0.9,
  "top_k": 40,
  "seed": 42
}
```

| | |
|---|---|
| `temperature`, `top_p`, `top_k`, `min_p` | the usual controls |
| `repetition_penalty`, `frequency_penalty`, `presence_penalty` | two formulations of the same idea |
| `seed` | reproducible sampling where supported |
| `max_tokens`, `stop` | response cap and stop sequences |
| `effort`, `think`, `thinking_budget` | three ways a model exposes a reasoning budget |

**An omitted parameter is not zero.** Leaving `temperature` out keeps the
model's own default; setting it to `0` asks for greedy decoding. Collapsing
those would make "don't touch it" impossible to say.

**A parameter the provider cannot honour is refused at startup**, naming the
knob and the provider. `min_p` sent to a hosted API is ignored silently and the
evidence is nowhere in the output — the answers are simply drawn from a
distribution nobody chose. Failing the config is the smaller harm.

Precedence is request → provider `params` → model default.

## Authentication

An API key, from config or the environment:

```json
{ "type": "anthropic", "model": "claude-opus-5", "api_key_env": "ANTHROPIC_API_KEY" }
```

### Subscriptions do not work, and this is not a Abhed limitation

A Claude Pro or Max subscription token is **restricted to Anthropic's own
clients**. Anthropic accepts the credential and then refuses the request unless
the system prompt is its own client's. Measured directly: same token, same
model, same second, the only difference being the first system block.

| First system block | Result |
|---|---|
| exactly Anthropic's client identity line | 200 |
| that line with anything appended | 429 |
| any other prompt, or none | 429 |

The refusal arrives as `429 rate_limit_error`, which is misleading — it is a
policy decision, not a limit that clears. Abhed recognises the shape (a 429
carrying none of the headers a real rate limit carries), reports it as what it
is, and does not retry.

Abhed reads `ABHED_ANTHROPIC_OAUTH_TOKEN` and `oauth_token` because the mechanism is
correct and the restriction may not be permanent. Today it is useful only for
models outside the check.

Working around it means sending Anthropic's client identity string from a
product that is not that client. That circumvents an access control,
misrepresents the product, and breaks the moment the check changes — so Abhed
does not do it, and neither should anything built on it.

Some models outside the check still answer. That is not a reason to rely on it:
third-party usage of a subscription is billed as *extra usage* rather than drawn
from the plan, so a path that looks free is metered somewhere the Console does
not show — a subscription token is not a Console credential, and spending under
it appears only in claude.ai settings.

**Use an API key.** From platform.claude.com, billed per token, no gate, and
visible where you would look for it:

```bash
export ANTHROPIC_API_KEY=sk-ant-api03-...
```

## Adding a provider without a rebuild

```json
"custom_providers": [
  {
    "name": "internal-vllm",
    "api": "openai",
    "base_url": "https://llm.internal.example/v1",
    "sampling": ["temperature", "top_p", "top_k", "seed"]
  }
]
```

`api` is named rather than guessed from the URL: a wrong guess produces rejected
requests whose errors point nowhere near the cause.

## Switching mid-session

`/model <name>` swaps the provider and keeps the conversation. The next turn
re-prefills, because the new provider has never seen this prefix — a real cost,
and still cheaper than rebuilding the session by hand. `/model` alone names the
model in use and the configured providers.

The new model answers from the next turn on: the system prompt names it, the
subagents that turn spawns run on it, and a conversation started after `/clear`
starts on it. `/resume` continues a recorded session on the model the CLI is
using now; when that session last ran on another, it says so, and the first
task records the move. The CLI follows your selection because one person at
one terminal chose it; a server session can be continued by anyone who owns
it, on any node, so it keeps the model its record names.

In `abhed serve`, the picker in the console and in the workbench shows only
when more than one provider is configured. With no chat open it chooses the
model the next one starts on; with one open it switches that session
(`POST /v1/sessions/{id}/model` with `{"provider": "<name>"}`). A session keeps
its model when it is continued from its record, after a restart or on another
node; if that provider is no longer configured it continues on the default,
and its next turn records the move. A switch is refused while a turn is
running, for a provider that is not configured or that a managed
`model.default` does not allow, and when the switch cannot be recorded; the
picker says why and goes back to the model still in use.

A switch is recorded as a `model.switched` event (`provider`, `model`,
`from`), each `model.call` names the model it went to, and the session list
and HawkEYE name the model in use.

## A local Ollama model

When the model you choose is one a local [Ollama](https://ollama.com) serves,
Abhed checks that it works before you rely on it, and gets it working with
you. A provider counts as Ollama's when its `type` is `ollama`, or when an
`openai-compatible` (or `openai`) `base_url` is on port 11434. Any other
OpenAI-compatible endpoint is not taken for Ollama, even at the address
`OLLAMA_HOST` names: one that is down is named as down, with no offer. Give
an Ollama on another port `"type": "ollama"`, as first run writes it. A server there that answers
the OpenAI API (`/v1/models`) but not Ollama's `/api/version`, such as a
proxy in front of Ollama, counts as running.

**When it checks.** At the start of an interactive `abhed` whose model is
Ollama's, on `/model <name>`, with `/ollama`, with `abhed ollama check`, in
the workbench and console pickers, and in Abhed Studio's model selector. It
looks for three things, in order: is Ollama installed (`ollama` on `PATH`, or
`/Applications/Ollama.app` on macOS), is its server answering at the
provider's address (`/api/version`), and is the model pulled (`/api/tags`).
Checking only reads; it never starts, installs or downloads anything.

**What it offers.** Whatever is missing, it says what is wrong in one line,
then the numbered steps you can run yourself, for example:

```
Ollama is installed but not running at http://127.0.0.1:11434.
To do it yourself:
  1. ollama serve
  2. ollama pull qwen3:8b
  3. Choose local again in Abhed (in the CLI: /model local).
```

and offers the next step, asking first. You can always say no; the steps stay
on screen and `/ollama` (or the card's *Check again*) checks again.

| Step | What runs | Asks |
|---|---|---|
| Start the server | macOS with the app and the default address: `open -a Ollama`. Otherwise `ollama serve` with `OLLAMA_HOST` set to `127.0.0.1` and the provider's port, detached in a process group of its own, output to `~/.abhed/logs/ollama-serve.log`. Abhed waits up to 30 seconds for it to answer | the first time; answer *always* to start it without asking from then on. `abhed ollama forget`, `/ollama forget`, Studio's forget or `POST /v1/ollama/forget` takes that back |
| Install Ollama | macOS: `brew install ollama` when Homebrew is installed, otherwise it opens the official download page (`https://ollama.com/download/mac`) and you install the signed app yourself. Linux: `brew install ollama` with Homebrew; on Arch it shows `sudo pacman -S ollama` for you to run; otherwise the official script, `curl -fsSL https://ollama.com/install.sh \| sh`, shown in full and run only by `abhed ollama check` at a terminal, since it may ask for your password. Windows: the official installer link (`https://ollama.com/download/windows`) | every time, showing the exact command, where it comes from and its size |
| Pull the model | `ollama pull <tag>` through the server's `/api/pull`, with progress; Esc, Ctrl-C, *Stop* or `_abhed/ollama/cancel` stops it, and Ollama keeps what it fetched, so pulling again resumes | every time, showing the command, the source (`registry.ollama.ai`), the download size from the registry's manifest, the free disk where Ollama keeps models, and a memory warning when the machine has less than the model wants |

Nothing is ever installed or downloaded without that answer. Every step runs
as you, never with `sudo`: a step that needs it is printed for you to run.
Abhed never binds Ollama to anything but loopback: a server it starts gets
`OLLAMA_HOST=127.0.0.1:<port>` whatever your environment says. It acts only on
an Ollama on this machine; for one at another address it shows the steps to
run there. **Abhed does not stop a server it started** when it exits: it is
your Ollama, and other programs may use it. Stop it as you would any other
(quit the app, or end `ollama serve`).

**Disk and memory.** Before a pull, Abhed asks the registry for the model's
size and refuses when the disk where Ollama keeps models (`OLLAMA_MODELS`, or
`~/.ollama/models`) has less free than the download and 2 GiB to spare. It
compares the system's memory with what the model wants: the figure in the
table below for a curated model, or for any other the download size plus a
quarter and 2 GB, said to be an estimate. Too little memory is a warning, not
a refusal, and it names the largest curated model that fits.

**Curated models.** `abhed ollama models` lists them. Each tag was checked on
24 October 2026 against the Ollama library (`ollama.com/library/<name>/tags`)
and the registry manifest (`registry.ollama.ai/v2/library/<name>/manifests/<tag>`);
the size is that manifest's, and a pull shows the size the registry gives then.

| Model | Tag | Download | Memory it wants |
|---|---|---|---|
| Qwen2.5-Coder 7B | `qwen2.5-coder:7b` | 4.7 GB | 8 GB |
| Qwen3 8B | `qwen3:8b` | 5.2 GB | 8 GB |
| Qwen3-Coder 30B | `qwen3-coder:30b` (what `qwen3-coder:latest` pointed to) | 18.6 GB | 24 GB |
| Gemma 4 26B | `gemma4:26b` | 18.7 GB | 24 GB |
| Gemma 4 31B | `gemma4:31b` | 20.4 GB | 32 GB |

**Turning it down or off.** `model.ollama.assist` decides what is offered:

| Value | Offers |
|---|---|
| `ask` (default) | start, install and pull, each asked as above |
| `local` | starting the server only; never an install or a download, and the registry is not asked for sizes: for a machine with no network, or a rule against downloads |
| `off` | nothing: the message and the steps only |

A managed configuration that sets it holds it, so `"model": {"ollama":
{"assist": "off"}}` in `/etc/abhed/config.json` keeps every surface to
guidance. An untrusted workspace file may only turn it down (`off` < `local`
< `ask`).

**Who may act.** Only you, choosing the model. No tool reaches any of this,
so the agent cannot start, install or pull anything this way, and inside an
agent's command `abhed ollama check`, `install` and `forget` are refused. On
`abhed serve` the steps act on the server's host, so only an administrator
may take them (or, where nobody signs in, a request from the host itself);
anyone else sees the message and the steps.

**Recorded.** Every attempt, and how it ended, is an event in the session's
record, attributed to you: `ollama.start`, `ollama.install` and
`ollama.pull`, with the command shown, the outcome (`ok`, `failed`,
`declined`, `refused`, `cancelled`, or `manual` when the step was left to
you) and why. HawkEYE lists them under *Local model (Ollama)*, the OCSF
export maps them ([OCSF](30-ocsf-and-logs.md#classes)), and `abhed serve`
counts them in `abhed_ollama_actions_total` and logs them. `abhed ollama
check` records its attempts in a short session of its own, which `abhed
sessions` lists and `abhed hawkeye <id>` reports on.

**The API.** The workbench and console use `GET /v1/ollama/status?provider=<name>`
(what was found, the next step's exact command, size and source, or why it is
not offered, and the steps), `POST /v1/ollama/start` (`{"provider", "session_id"?,
"always"?}`), `POST /v1/ollama/install`, `POST /v1/ollama/pull` (a stream of
JSON lines: `{"progress": …}`, then `{"status": …}` or `{"problem": …}`;
closing the request stops it) and `POST /v1/ollama/forget`. A refusal or
failure answers with `{"error", "problem": {"kind", "message", "steps",
"retry"}, "status"}`. Abhed Studio uses the `_abhed/ollama/*` methods
([the contract](../architecture/studio-acp-contract.md#32a-a-local-ollama-model)).

## Choosing one

`abhed doctor` tells you whether a model can drive the agent at all.
`abhed-modelcmp` compares candidates on tool calling and explanation quality,
which are different things and are not measured by the same benchmark.

Decode speed tracks *active* parameters, not total: a 27B dense model can be far
slower than a 30B mixture-of-experts.
