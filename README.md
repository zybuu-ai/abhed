<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/abhed-lockup.svg">
    <img src="brand/abhed-lockup-light.svg" alt="Abhed" width="360">
  </picture>
</p>

<p align="center">
  <a href="https://github.com/zybuu-ai/abhed/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/zybuu-ai/abhed/actions/workflows/ci.yml/badge.svg?branch=main"></a>
  <a href="LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-2A8CF0?style=flat-square"></a>
  <a href="go.mod"><img alt="Go 1.26" src="https://img.shields.io/badge/go-1.26-00ADD8?style=flat-square&logo=go&logoColor=white"></a>
  <a href="docs/guide/03-providers.md"><img alt="20 model providers" src="https://img.shields.io/badge/model%20providers-20-2A8CF0?style=flat-square"></a>
  <a href="bench/RESULTS.md"><img alt="Benchmark 24/24" src="https://img.shields.io/badge/exercism%20benchmark-24%2F24-1E7F55?style=flat-square"></a>
  <a href="docs/trust/security-scans.md"><img alt="Security scans: 0 critical" src="https://img.shields.io/badge/image%20scan-0%20critical-1E7F55?style=flat-square"></a>
</p>

<p align="center">
  <a href="https://abhed.zybuu.com/docs/"><img alt="Documentation" src="https://img.shields.io/badge/docs-abhed.zybuu.com-0B3C8C?style=flat-square&logo=readthedocs&logoColor=white"></a>
  <a href="https://zybuu.com/abhed/"><img alt="Website" src="https://img.shields.io/badge/website-zybuu.com%2Fabhed-0B3C8C?style=flat-square"></a>
  <img alt="Runs on-prem, air-gap capable" src="https://img.shields.io/badge/runs-on--prem%20%C2%B7%20air--gapped-444?style=flat-square">
  <img alt="Egress by default: none" src="https://img.shields.io/badge/egress%20by%20default-none-444?style=flat-square">
</p>

# Abhed

An on-prem, air-gap-capable deep agent platform. Model-agnostic by construction:
the better the reasoning model you point it at, the better it performs.

```
$ abhed -p "fix the failing test" -mode auto -allow 'bash(go test*)'
● grep "func Add"      └ 2 line(s)
● read math.go         └ 5 line(s)
● edit math.go         └ Edited math.go.
● bash run tests       └ exit 0

Fixed the sign error in Add (math.go:4) and the tests now pass.
5 turns · 6000 in / 200 out tokens · 83% cached (5.9x prefill)
```

## Documentation

**[Start here](docs/guide/README.md)** — install, configure, extend, embed.

| | |
|---|---|
| [Getting started](docs/guide/01-getting-started.md) | first run, and the shape of a session |
| [Configuration](docs/guide/02-configuration.md) | every setting |
| [Models and providers](docs/guide/03-providers.md) | twenty providers, sampling, subscriptions |
| [Permissions](docs/guide/04-permissions.md) | what the agent may do, and who decides |
| [Tools](docs/guide/05-tools.md) · [Skills](docs/guide/06-skills.md) · [Extensions](docs/guide/07-extensions.md) · [MCP](docs/guide/08-mcp.md) | adding your own |
| [SDK](docs/guide/09-sdk.md) · [Automation](docs/guide/10-automation.md) | embedding and driving it |
| [Sessions and audit](docs/guide/11-sessions.md) | replay, forking, export |

`docs/architecture/` holds the design notes behind those decisions.

## The thesis

A controlled study found **harness-induced variance exceeds model-induced variance
by 7.80×** on SWE-bench Verified, with ranking reversals in 6 of 9 model-pair
comparisons. The scaffold around the model — context management, tool design,
subagents, permissions — is a first-class engineering variable, not glue code.

Abhed is built on that: the harness is a separately engineered, separately
evaluated layer behind a provider abstraction. Better model, better agent.
Better harness, better agent. Both compound.

## Quick start

```bash
go build -o abhed ./cmd/abhed
./abhed init                # write .abhed/config.json
./abhed doctor              # verify endpoint, tool-calling, sandbox, auth, storage, index, MCP
./abhed                     # interactive
./abhed serve -addr :8080   # web console + API
./abhed eval                # run the evaluation corpus
./abhed index               # build the retrieval index
```

Air-gapped: the binary is static, the console is self-contained, and nothing
leaves the machine except calls to the model endpoint you configure. Copy the
binary and a config into the enclave and it runs. The signed offline bundle and
its verifier are part of the Enterprise Edition.

Point it at anything OpenAI-compatible — vLLM, SGLang, TensorRT-LLM, llama.cpp,
Ollama, or a hosted API:

```bash
export ABHED_BASE_URL=http://your-gpu-host:8000/v1
export ABHED_MODEL=Qwen/Qwen3-32B
./abhed doctor
```

### Running against an LLM proxy or a hosted key

For a gateway that authenticates with an API key — LiteLLM, OpenRouter, a company
proxy, or any hosted OpenAI-compatible API — declare a named provider in
`.abhed/config.json` and keep the key in the environment, never in the file:

```json
{
  "model": {
    "default": "proxy",
    "providers": {
      "proxy": {
        "type": "openai-compatible",
        "base_url": "https://your-proxy.example.com/v1",
        "model": "claude-sonnet-4.6",
        "api_key_env": "ABHED_API_KEY",
        "context_window": 200000,
        "extra": { "user": "your-sso-id" }
      }
    }
  }
}
```

```bash
export ABHED_API_KEY=sk_...            # resolved via api_key_env, never written to disk
./abhed doctor                         # confirms the endpoint answers and tool-calling works
./abhed                                # interactive CLI
./abhed serve -addr 127.0.0.1:8090     # web console + API on http://127.0.0.1:8090
```

- **`api_key_env`** names the variable holding the key, so the secret stays out of
  the config and out of version control (`.abhed/` is git-ignored). Export it in
  every shell that runs `abhed`, or add it to your shell profile.
- **`extra.user`** is sent as the request's `user` field. Some proxies (LiteLLM
  among them) reject a call without it with `400 … must pass a 'user' field`; set
  it to your SSO/user id. Omit the line for endpoints that do not require it.
- **Multiple models:** add more named providers (each a different `model`, even on
  the same `base_url`) and switch live with `/model <name>` in the CLI or the model
  picker in the console. `default` selects the one used at startup.
- A self-signed or internal-CA proxy works as long as its CA is in the OS trust
  store; Abhed uses the system roots.

Measure whether your serving stack actually caches prefixes — the assumption the
whole capacity model rests on:

```bash
go build -o abhed-bench ./cmd/abhed-bench
./abhed-bench -model Qwen/Qwen3-32B -turns 40
```

## What's implemented

| Area | Status |
|---|---|
| Event-sourced loop, 8 emitted terminal reasons | ✅ tested |
| Tools: read, write, edit, glob, grep, bash, task | ✅ tested |
| Read-before-edit, exact-match, near-miss recovery | ✅ tested |
| Ordered policy engine, absolute deny, always-confirm destructive | ✅ tested |
| OpenAI-compatible adapter, streaming, reasoning-token stripping | ✅ tested |
| **Execution sandbox** (Seatbelt / bubblewrap / OCI / gVisor) | ✅ escape-tested |
| **Compaction** with PreCompact hook, tool-call integrity | ✅ tested |
| **Subagents** with hierarchical budgets, profile-scoped tools | ✅ tested |
| **MCP gateway** with tool-poisoning defense | ✅ tested |
| **Hybrid retrieval** — symbol, BM25, vector tiers | ✅ tested |
| **Server mode** — REST, SSE, remote approvals, tenancy | ✅ tested |
| **Web console** — self-contained, no CDN | ✅ tested |
| **Prefix-cache benchmark** | ✅ validated |
| **Postgres store** — append-only + row-level security | ✅ integration-tested |
| **Local accounts** — bcrypt, timing-safe sign-in, proxy-header identity | ✅ tested |
| **Eval harness** — assertions + behavioural flags | ✅ tested |
| **Adversarial suite** — 24 attacks | ✅ all blocked |

`make check` — 29 packages, 531 test functions. Drop `-short` for the slow
network-exfiltration checks; set `ABHED_TEST_DSN` for the Postgres integration tests.

## Architecture

```
Access    CLI · Web console · REST/SSE API
Control   Orchestrator → Context → Policy → Tool router    ← the harness
Inference OpenAI-compatible gateway (any model)
Execution tiered sandbox, no egress by default
Data      event store · hybrid index · MCP registry
          ═══ air-gap boundary ═══
Egress    broker (optional, default OFF)
```

| Doc | Contents |
|---|---|
| [01 Principles](docs/architecture/01-principles.md) | 12 principles, tagged by evidence status |
| [02 System architecture](docs/architecture/02-system-architecture.md) | Planes, loop, subagents, retrieval |
| [03 Security](docs/architecture/03-security.md) | Isolation tiers, injection, validation status |
| [04 Sizing](docs/architecture/04-sizing.md) | VRAM math, tiers, prefill economics, cost |
| [06 Tool contracts](docs/architecture/06-tool-contracts.md) | Exact schemas, semantics, error messages |
| [07 System prompt](docs/architecture/07-system-prompt.md) | Prompt layering, ABHED.md, anti-patterns |
| [08 Eval](docs/architecture/08-eval.md) | 4-layer harness incl. behavioral inspection |
| [09 UX](docs/architecture/09-ux.md) | CLI, approvals, modes, latency budget |
| [10 Data model](docs/architecture/10-data-model.md) | Events, schema, protocol, adapter interface |
| [Infrastructure tools](docs/ops/infrastructure.md) | Kubernetes, SSH and external retrieval |
| [Enabling authentication](docs/ops/enabling-auth.md) | Local accounts and proxy-header identity |

## Design decisions worth knowing

**Errors are written for the model, not the log.** A failed edit reports the
nearest matching line with context, so the model recovers in one turn instead of
guessing. This is the difference between a tool set that works and one that
frustrates the model into loops.

**Edits require a prior read**, match exactly, and never fuzzy-match. A near-miss
that "helpfully" applies produces a silent wrong edit — the worst outcome an
editing tool can have.

**The sandbox never silently downgrades.** If no backend meets the configured
minimum tier, Abhed fails with what it tried and how to fix it. A sandbox that
quietly weakens itself is worse than none, because operators stop checking.

**Deny is absolute.** It blocks even in bypass mode. Destructive commands confirm
in every mode. Managed org policy cannot be escalated past locally.

**Everything untrusted is tagged at ingest** — file contents, tool output, MCP
responses — because a coding agent's whole job is reading untrusted text and
acting on it. MCP tool *descriptions* are sanitized too: they are third-party
text injected into the model's context, which makes them an attack surface.

**Nothing volatile in the prompt prefix.** The date is day-granular; a per-second
timestamp would invalidate the prefix cache on every request.

## Evidence discipline

Claims carry provenance: **[V]** verified by adversarial research, **[C]** computed
and reproducible, **[E]** engineering judgment.

The design pass ran 112 agents across 6 research angles with 3-vote adversarial
verification: 15 verified findings, **6 refuted claims**, and **5 of 9 areas with
zero surviving claims**. Those gaps are documented rather than papered over.

**Measurement and adversarial testing have corrected the design five times.** The sizing doc claimed
compaction "invalidates the prefix by construction" — benchmarking showed that is
false for Abhed, because the system prompt and `ABHED.md` sit outside the
compacted history, so the cached prefix survives. And an end-to-end run exposed a
policy bug where `auto` mode rejected its own edits. Three more surfaced later:
row-level security was silently inert because a table owner bypasses it without
`FORCE`; the auth middleware was ordered so it read identity before establishing
it, making every request anonymous; and the bundle manifest listed its own digest,
so per-file verification could never pass. Each was found by a test written to
attack the thing rather than confirm it.

**Sandboxing evidence was refuted, not confirmed** — so isolation is proven by an
adversarial suite of 24 attacks rather than assumption (see
[validation status](docs/architecture/03-security.md)). That suite proves the
controls resist the attacks in it; it cannot prove a determined attacker fails,
because it only tries what its author thought of. **A human red-team engagement
remains outstanding and is not substitutable.** For untrusted repositories set
`sandbox.min_tier` to `container` or `vm` and commission one first.

## License

The Community Edition, everything in this repository, is open source under the
[Apache License 2.0](LICENSE). Use it, modify it, ship it, sell services on it.
The Team and Enterprise features described on the product page (OIDC, the
admin and access dashboard, scheduled runs, multi-tenant isolation, audit
export, the signed air-gap bundle, telemetry export) are a separate,
proprietary edition built on this module; they are not in this repository.
