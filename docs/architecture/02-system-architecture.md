# Abhed — System Architecture

Status: Draft · 2026-09-02

## 1. Plane separation

Abhed splits into four planes so the air-gap boundary falls on a single, auditable line.

```
┌──────────────────────────────────────────────────────────────────────────┐
│  ACCESS PLANE                                                            │
│  abhed CLI (Go, static binary) │ Web console (TS) │ REST/gRPC + SSE API  │
│  IDE bridge (LSP)              │ OIDC/SAML SSO    │ Tenant-scoped RBAC   │
└─────────────────────────────────┬────────────────────────────────────────┘
                                  │  session protocol (event stream)
┌─────────────────────────────────▼────────────────────────────────────────┐
│  CONTROL PLANE  — the harness. Abhed's actual product surface. (P1)      │
│                                                                          │
│   ┌────────────┐   ┌──────────────┐   ┌───────────────┐  ┌────────────┐ │
│   │ Orchestr.  │──▶│ Context Mgr  │──▶│ Policy Engine │─▶│ Tool Router│ │
│   │ step(state)│   │ compact/JIT  │   │ 6-step order  │  │ MCP+native │ │
│   └─────┬──────┘   └──────────────┘   └───────────────┘  └─────┬──────┘ │
│         │                                                       │        │
│   ┌─────▼──────┐   ┌──────────────┐   ┌───────────────┐        │        │
│   │ Subagent   │   │ Event Store  │   │ Model Router  │        │        │
│   │ Supervisor │   │ append-only  │   │ provider:model│        │        │
│   └────────────┘   └──────────────┘   └───────┬───────┘        │        │
└───────────────────────────────────────────────┼────────────────┼────────┘
                                                │                │
┌───────────────────────────────────────────────▼──────┐  ┌──────▼────────┐
│  INFERENCE PLANE                                     │  │ EXECUTION     │
│  OpenAI-compatible gateway                           │  │ PLANE         │
│  ├ vLLM / SGLang / TensorRT-LLM  (GPU tiers)         │  │ sandbox pool  │
│  ├ prefix cache (load-bearing, P8)                   │  │ per-session   │
│  ├ guided decoding + per-family tool-call parsers    │  │ FS + net scope│
│  └ embedding + rerank endpoints                      │  │ no egress     │
└──────────────────────────────────────────────────────┘  └───────────────┘
                                  │
┌─────────────────────────────────▼────────────────────────────────────────┐
│  DATA PLANE   Postgres (sessions/audit) │ Object store (artifacts)       │
│               OpenSearch/vector (RAG)   │ Registry (models, MCP, images) │
└──────────────────────────────────────────────────────────────────────────┘
                                  ╎
                    ══════════ AIR-GAP BOUNDARY ══════════
                                  ╎
┌─────────────────────────────────▼────────────────────────────────────────┐
│  EGRESS BROKER (optional, default OFF) — the ONLY component that talks   │
│  outward. Separate host, separate netns, allowlist, full content audit.  │
└──────────────────────────────────────────────────────────────────────────┘
```

**Why this split:** the control plane is where the 7.80× harness variance lives (P1), so
it must be independently versionable and testable. The inference plane is swappable by
construction (P12). The execution plane is the blast radius. The egress broker is the only
thing that crosses the air gap, so it is the only thing that needs air-gap-grade review.

## 2. The agent loop

Event-sourced (P6). One turn = one model round trip plus its tool executions.

```
                     ┌───────────────────────────────┐
                     │  Event Store (append-only)    │
                     │  Action | Observation | Note  │
                     └───────┬───────────────▲───────┘
                             │ history       │ append
                             ▼               │
   ┌───────────┐      ┌──────────────┐      │      ┌──────────────┐
   │ Context   │─────▶│    Agent     │──────┴─────▶│   Runtime    │
   │ Assembler │      │ history→act  │   action    │ act→observ.  │
   └─────▲─────┘      └──────┬───────┘             └──────┬───────┘
         │                   │ no tool call?              │
         │                   ▼                            │
         │            ┌──────────────┐                    │
         │            │  TERMINATE   │◀── + ~10 other exit paths
         │            └──────────────┘    (max turns, budget, hook
         │                                 reject, interrupt, error,
         └── re-inject ABHED.md every turn  shutdown, retry exhaustion)
```

Normal termination is a response with no tool calls. **A production harness needs roughly
ten other exit paths** — max-turn cap, budget cap, pre-tool hook rejection, user interrupt,
worker shutdown, structured-output retry exhaustion, and error states. Each must be a
distinct, logged terminal event, not an exception.

### Context assembly order (every turn)

```
[ cached prefix ─────────────────────────────][ volatile ──────────────]
  system prompt │ tool defs │ ABHED.md         │ event history │ JIT reads
  └── stable across turns → prefix cache hit   └── grows; triggers compaction
```

Keep everything stable at the front. A cache hit on that prefix is the 17× economics of
P8. Order matters more than it looks: one volatile token early in the prompt invalidates
the entire downstream cache.

### Compaction

Triggered at ~95% of the window, or by explicit operator/model request.

```
 history ──▶ PreCompact hook ──▶ archive to event store (never discarded)
                   │
                   ▼
            LLM summarization ──▶ new window = summary
                                              + N most-recent-file reads (bounded budget)
                                              + ABHED.md (re-injected, never summarized)
                                              + plan file
```

Budget-bound the retained file set (a total token cap and a per-file cap), skip files
already present in retained messages, and emit a `compact_boundary` event so the audit
trail can reconstruct what was dropped. **Every compaction is a cold-prefill event** — count
them as a capacity metric (P8).

## 3. Subagent model

```
        orchestrator (owns plan, budget, final answer)
              │  spawn(task, tools, budget)
     ┌────────┼────────┬────────────┐
     ▼        ▼        ▼            ▼
   search   test     review      explore     ← fresh context each
     │        │        │            │           (own system prompt +
     └────────┴────────┴────────────┘            ABHED.md, no parent turns)
              │ 1–2k token summary only
              ▼
        orchestrator context grows by summary, not transcript
```

Hard requirements, all of which have bitten the systems this is modeled on:

- **Budget is hierarchical.** Subagent spend counts against the parent's cap. On exhaustion,
  spawning fails cleanly and running subagents are stopped.
- **Nested spawning off by default.** Bound concurrency explicitly (~20 default).
- **Fresh ≠ free.** Each subagent re-prefills its own prefix; 20 subagents × 12k tokens =
  240k tokens of prefill (0.77 s on a 117B MoE, 4.85 s on a 32B dense — see P9).
- **Fork mode** (inherits parent context) is a distinct, explicit mode.

## 4. Retrieval: agentic-first, index-assisted

Evidence favors just-in-time agentic search over pre-computed embeddings — a costly signal,
since Anthropic built the vector-DB path and abandoned it. **But this is contested** (a
vector-DB vendor disputes it; an Amazon Science result puts keyword-via-tool-use at >90% of
RAG performance), and the strongest supporting statistic was refuted. So Abhed is
agentic-first with retrieval as an *accelerator*, and measures the difference:

```
 Tier 0  glob / grep / head / tail / LSP        default, always available
 Tier 1  repo map (tree-sitter/SCIP symbols)    cheap structural prior
 Tier 2  hybrid BM25 + vector + rerank          large monorepos, NL doc queries
```

Route by query shape: identifier-like → Tier 0; structural → Tier 1; natural-language over
docs/runbooks → Tier 2. Log which tier resolved each task so the routing policy is
empirical rather than assumed. This is the honest position given contested evidence: build
both, instrument, let your corpus decide.

## 5. Model abstraction

One call site, `provider:model`. The adapter absorbs everything that differs per family:

| Concern | Why it must be in the adapter |
|---|---|
| Tool-call format | JSON vs XML vs Pythonic vs harmony; per-family parsers |
| Reasoning tokens | Strip/retain/stream `<think>`; never let them reach tool parsing |
| Prompt template | Chat template drift breaks caching silently |
| Structured output | Guided decoding vs retry-on-parse-failure |
| Context window | Compaction thresholds are model-specific |
| Effort control | Per-model, per-task-class; measured (P11) |

**Capability probe on registration.** A new model runs a conformance suite before it can
serve traffic: tool-call round trip, schema adherence under load, long-context retrieval,
reasoning-token leakage, refusal rate. Store the profile in the registry. This is the
mechanism that makes P12 real rather than aspirational — and the resulting cross-model
consistency benchmark is Abhed's differentiating asset.

## 6. Trust boundaries

```
 tenant ─┬─▶ access (authn/authz) ─┬─▶ control (policy) ─┬─▶ tools ─┬─▶ execution
         │   OIDC, tenant scope     │   6-step ordered    │  MCP     │  sandbox
         │                          │   deny is absolute  │  gateway │  no egress
         └──────────────────────────┴─────────────────────┴──────────┴─ audit (all)
```

Untrusted content — repo files, tool output, MCP responses, brokered search results — is
**never** treated as instruction. It is tagged at ingest and stays tagged through the event
store. Given that the sandboxing evidence was refuted rather than confirmed, Abhed treats
prompt injection as an open threat requiring defense in depth, not a solved problem:

1. Provenance tagging on every observation.
2. Policy evaluated on the *action*, independent of the text that motivated it.
3. Execution isolation sized to the assumption that injection sometimes succeeds.
4. Egress default-deny, so a successful injection has no channel to exfiltrate through.
