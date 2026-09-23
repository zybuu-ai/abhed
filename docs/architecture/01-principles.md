# Abhed — Design Principles

Status: Draft · 2026-09-02 · Owner: @yuvraj-singh79

Abhed is an on-prem, air-gap-capable deep agent platform. It targets the capability bar
set by the leading cloud coding agents, with the enterprise deployment posture of other
enterprise agent platforms, and it is deliberately model-agnostic.

Every principle below is tagged with its evidence status:

- **[V]** Verified — survived 3-vote adversarial verification against primary sources.
- **[C]** Computed — derived arithmetically in this repo (`docs/architecture/04-sizing.md`).
- **[E]** Engineering judgment — not established by research; validate before betting on it.

---

## P1 — The harness is the product, not the model. [V]

A controlled 3×3 factorial study (3 models × 3 harness configurations) on a stratified
100-task SWE-bench Verified subset found **harness-induced variance exceeded
model-induced variance by 7.80×** (18.48 pp² vs 2.37 pp²). Changing the harness moved
GLM-5.1 by 13.0 points; changing the model within a fixed harness moved scores by only
2.5–5.0 points. Six of nine model-pair/harness-pair comparisons showed **ranking
reversals** — which model is "best" depends on the harness you run it in.

Observationally, the same effect appears in public leaderboards: Claude Opus 4.5 scores
45.9% on SWE-bench Pro under the standardized SEAL scaffold vs 55.4% under its vendor's
own agent (+9.5pp). Grok 4 moves 58.6% → 72–75% between an open-source scaffold and xAI's
own.

> Honest qualification, from the verifiers: on some coding benchmarks the harness's
> dominant effect is on **cost and failure mode** rather than raw accuracy (confidence
> intervals often span zero). The defensible phrasing is *"the harness is a first-class
> variable,"* not *"the harness dominates accuracy."* The 7.80× figure rests on a single
> May 2026 paper, though a verifier independently re-derived every number from its
> published table, and three independent studies corroborate the direction.

Source: [Stop Comparing LLM Agents Without Disclosing the Harness](https://arxiv.org/abs/2605.23950),
Table 2. The authors say plainly that they do not claim the ratio is universal.

**Consequence for Abhed:** the harness is a separately engineered, separately versioned,
separately evaluated layer. This is what makes "better model → better agent" true rather
than aspirational: the harness is the constant that lets model quality show through.

## P2 — Context is a finite attention budget. [V]

Model recall degrades measurably as context grows ("context rot"), independently
corroborated across 18 frontier models. This is a gradient, not a cliff — models remain
capable at long context but show reduced precision for retrieval and long-range reasoning.

The mechanism is contested (Anthropic attributes it to n² pairwise attention; the
position-bias literature locates it in primacy-recency effects and training distribution).
Abhed does not need to resolve the mechanism — the design consequence is identical either
way: **never treat a large context window as a substitute for context engineering.**

## P3 — Three composable context mechanisms, in priority order. [V]

1. **Subagents (design-time isolation, primary).** A subagent explores using tens of
   thousands of tokens and returns a 1,000–2,000 token summary. The orchestrator's context
   grows by the summary, not the transcript.
2. **Just-in-time retrieval (runtime).** Lightweight identifiers (file paths, queries) plus
   `glob`/`grep`/`head`/`tail` at runtime, rather than pre-computed embeddings.
3. **Compaction (reactive fallback).** Summarize and reinitialize when approaching the limit.

Isolation is a **compression ratio, not free** — a subagent that does 8,000 tokens of work
and writes a 3,000-token report still costs the parent 3,000 tokens, and each subagent
re-prefills its own system prompt and memory file. See P8 for the GPU-second consequence.

## P4 — Persistent rules live in a re-injected memory file. [V]

Compaction discards early instructions by construction. Anything that must survive the
whole session belongs in a `ABHED.md`-style memory file that is re-injected on every
request — **not** in the initial prompt.

This is only affordable because the file sits in a cached prefix (P8). Two operator levers
are required: summarization directives readable inside the memory file, and a
`PreCompact` hook for archiving the full transcript before it is discarded.

> Watch item: a documentation-vs-shipped-behavior divergence is reported upstream for
> exactly this mechanism (memory content lost after compaction). **Validate empirically;
> do not assume the documented guarantee holds.**

## P5 — Control-loop architectures are composable primitives. [V]

The ReAct vs plan-and-execute vs CodeAct framing is a false trichotomy. A source-code
taxonomy of 13 open-source coding agents found **11 of 13 compose multiple primitives**;
7 of 13 use sequential ReAct as the primary spine, layering generate-test-repair,
plan-execute, multi-attempt retry, and tree search on top.

**Consequence:** Abhed ships ReAct as the default spine and treats the others as
composable strategies selectable per task class — not as an architecture to commit to once.

> Scope caveat: that corpus covers open-source agents pinned to June 2023 – March 2025
> commits. The vendors' own coding agents are essentially absent. Do not generalize the
> counts to closed-source agents.

## P6 — Agent state is an event-sourced stream. [V]

Model state as a chronological stream of actions and observations:
`Agent: EventHistory → Action`, `Runtime: Action → Observation`, exposed via a single
`step(state)` call. Tool-calling (including MCP JSON schemas) maps *into* this action
abstraction rather than replacing it.

This abstraction survived a full architectural rewrite of an open-source agent platform
(its V0 → V1 SDK), which is strong evidence it is the right extensibility point. Its
**deterministic replay** property is directly what an air-gapped platform needs for audit
logging and incident reconstruction.

> Note: two related claims about that system's V0 execution details (its CodeAct action
> space and its Docker sandboxing) were **refuted 0-3**. Adopt the event-stream abstraction;
> do not copy the V0 execution specifics.

## P7 — Permissions are a layered, ordered evaluation. [V]

Six-step ordered flow, directly transplantable to an enterprise approval system:

```
Hooks → Deny rules → Ask rules → Permission mode → Allow rules → Callback
```

Deny is absolute: a deny rule blocks the tool **even in the most permissive mode**. Tools
are scoped per-command (`Bash(npm *)`), not per-tool. For multi-tenancy, the load-bearing
primitive is an **org-level managed setting that a local user config cannot escalate past**.

## P8 — Prefix caching is the economic foundation. [C]

The P4 memory-file pattern and the P3 subagent pattern are only affordable if the serving
layer has working prefix caching. Computed for a 40-turn session re-sending a 60k prefix
on 8×H100:

| | prefill cost |
|---|---|
| No prefix cache | 48.5 GPU-s |
| With prefix cache | 2.8 GPU-s |
| **Reduction** | **~17×** |

**Compaction invalidates the prefix by construction** — every compaction event pays cold
prefill again. This makes compaction frequency a *capacity planning* variable on owned
GPUs, not merely a quality knob. Budget it explicitly.

## P9 — Prefer MoE for agent workloads. [C]

Coding agents are prefill-heavy. Prefill cost scales with *active* parameters, so a
sparse MoE model is dramatically cheaper per prefilled token than a dense model of similar
quality. Prefilling 190k tokens on 8×H100: **612 ms** for a 117B/5.1B-active MoE vs
**3,840 ms** for a 32B dense model — 6× faster despite 3.6× more total parameters.

The tradeoff is VRAM: MoE holds all experts resident. That is the right trade when the
binding constraint is latency and concurrency rather than memory.

## P10 — Evaluate behavior, not just scores. [V]

Agents with **identical pass rates exhibit materially different behaviors**. Log inspection
across 21,730 rollouts found agents searching for benchmark answers on HuggingFace instead
of solving tasks, and misusing credit cards in booking tasks. Scoring assigns the same
value to correct abstention as to harmful action.

Abhed's eval harness must therefore include automated log inspection from day one, not
score aggregation alone. *(The specific inference that this implies tool-permission
guardrails is ours, not the paper's — that paper measures, it does not prescribe.)*

## P11 — Reasoning effort is a measured parameter, not a global default. [V-negative]

A candidate finding that higher reasoning effort *reduces* accuracy was **refuted 0-3 and
must not be cited.** What survives is weaker and indirect: 9× cost variation for 2pp
accuracy differences, and 40× token variation per solved task across scaffolds.

On owned GPUs, reasoning tokens map to GPU-seconds and concurrency ceilings rather than to
a monthly bill. Effort level must be a per-model, per-task-class configuration parameter
validated on Abhed's own eval harness.

## P12 — Cross-model consistency is Abhed's differentiating asset. [E]

A provider-agnostic backend abstraction (single call site, `provider:model`) is the proven
pattern for swappability. Declarative rule-constraining — forcing heterogeneous models into
consistent behavior — is the only architectural answer in the evidence set to the
harness-variance problem of P1, **but it has zero independent validation.**

That gap is the opportunity. Building a cross-model consistency benchmark is simultaneously
the eval harness Abhed needs and the proof its abstraction works. Treat it as a design goal
to measure, not a property to assume.

---

## What the research did NOT establish

Five of nine research areas produced **zero surviving claims**. These are **unresearched,
not settled** — and the sections that follow are engineering judgment [E] or computed [C],
not verified:

| Area | Status |
|---|---|
| Inference serving (vLLM/SGLang/TensorRT-LLM comparison, guided decoding, tool-call parsers) | Unverified |
| Hardware sizing (quantization quality loss, QPS/GPU, vendor pricing) | Computed here; pricing unverified |
| Air-gapped ops (offline mirrors, weight signing, SSO/RBAC, compliance frameworks) | Unverified |
| Sandboxing & prompt-injection defense | **Unverified — and two claims were refuted** |
| MCP spec state, transports, auth, gateway design | Unverified |

**Sandboxing is the most dangerous gap.** Two sandboxing claims were refuted 0-3, leaving
the execution-isolation posture of every comparable agent unverified. For an enclave that
ingests untrusted repo content and brokered web-search results, this is disqualifying for a
production design and is the next research target. Abhed therefore treats
isolation as a *requirement to be independently established*, not a solved problem to copy.

Model names throughout this research (GPT-5.4, Kimi K2.6, GLM-5.1, Claude Opus 4.5/4.6,
MiniMax 2.5) date it to roughly May–September 2026 and will be stale within two quarters.
