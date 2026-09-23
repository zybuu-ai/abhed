# Abhed — Resource Requirements & Sizing

Status: Draft · 2026-09-02

**Evidence status: [C] computed / [E] engineering judgment.** The deep-research pass
produced **zero verified claims** on hardware sizing, so nothing here is quoted from a
source. Everything below is derived from published model architecture parameters using
standard transformer memory arithmetic — reproducible via `docs/architecture/sizing.py`.
Vendor pricing is indicative and **must be quoted, not trusted**.

## 1. The arithmetic

```
weights_GB   = params × bytes_per_param                (FP8 = 1B, INT4 = 0.5B)
kv_per_token = 2 × layers × kv_heads × head_dim × dtype_bytes
kv_total_GB  = kv_per_token × context_len × concurrent_sequences
VRAM         = weights + kv_total + activations(~5-10%) + fragmentation
```

The second line is the one that decides your cluster. **KV cache, not weights, is the
binding constraint for a coding agent**, because agent sessions run long contexts.

## 2. Per-model memory profile

| Model | Weights FP8 | Weights INT4 | KV/token | KV @128k | KV @1M |
|---|---:|---:|---:|---:|---:|
| Qwen3-32B (dense) | 29.8 GB | 14.9 GB | 256 KB | 32.0 GB | 256 GB |
| gpt-oss-120b (MoE, 5.1B active) | 109.0 GB | 54.5 GB | **72 KB** | **9.0 GB** | 72 GB |
| Llama-3.3-70B (dense) | 65.2 GB | 32.6 GB | 320 KB | 40.0 GB | 320 GB |
| Qwen3-235B-A22B (MoE, 22B active) | 218.9 GB | 109.4 GB | 188 KB | 23.5 GB | 188 GB |

**Read this table carefully — it inverts the intuition.** A 117B MoE has a *smaller* KV
footprint per token (72 KB) than a 32B dense model (256 KB), because KV cost is driven by
`layers × kv_heads × head_dim` (GQA width), not parameter count. At 128k context, one
sequence on Llama-3.3-70B costs 40 GB of KV — **more than half the weights themselves.**

Practical consequence: **choose models by GQA geometry, not by parameter count**, when
long-context concurrency is the goal.

## 3. Deployment tiers

Concurrency below = simultaneously *active* sessions at 45k live context, FP8 weights.
Real user counts are 3–5× higher, since developers spend most of their time reading rather
than generating.

| Tier | Hardware | HBM | Model | Concurrent sessions | Fits |
|---|---|---:|---|---:|---|
| **T0** | CPU-only, 128 GB RAM | — | 7–14B INT4 | 1–2 | Dev/CI smoke tests only |
| **T1** | 2× RTX 6000 Ada | 96 GB | Qwen3-32B | **4** | Single developer / pilot |
| **T2** | 8× H100 SXM | 640 GB | gpt-oss-120b MoE | **151** | Team → department |
| | | | Qwen3-32B | 49 | |
| | | | Llama-3.3-70B | 37 | |
| **T3** | 8× H200 SXM | 1128 GB | gpt-oss-120b MoE | **293** | Department, long-context |
| | | | Qwen3-235B-A22B | 98 | |
| **T4** | 32× H100 (4 nodes) | 2560 GB | gpt-oss-120b MoE | **693** | Org-wide |
| **T5** | 64× H200 (8 nodes) | 9024 GB | gpt-oss-120b MoE | **2534** | Enterprise |

**T2 is the recommended entry point for a real deployment.** A single 8×H100 node serving
a sparse MoE supports ~150 concurrent agent sessions — roughly 500–750 developers at
realistic duty cycle — with no multi-node networking complexity.

## 4. Prefill economics — why the harness design drives hardware

Agent workloads are prefill-dominated: every turn re-sends a large stable prefix.
Computed on 8×H100 at 40% MFU:

| Scenario | Tokens | gpt-oss-120b (5.1B active) | Qwen3-32B (dense) |
|---|---:|---:|---:|
| System prompt + ABHED.md (cold) | 12,000 | 39 ms | 243 ms |
| Mid-session with repo context | 60,000 | 193 ms | 1,213 ms |
| Near compaction threshold | 190,000 | **612 ms** | **3,840 ms** |
| 20-subagent fan-out | 240,000 | 0.77 s | 4.85 s |

**Prefix caching (P8), 40-turn session re-sending a 60k prefix:**

| | Prefill cost |
|---|---:|
| No prefix cache | 48.5 GPU-s |
| With prefix cache | 2.8 GPU-s |
| **Reduction** | **~17×** |

Three consequences that should change your build:

1. **Prefix caching is not an optimization, it is a precondition.** Without it the
   re-injected-memory-file pattern (P4) costs 17× more GPU time. Verify hit rates in
   production; do not assume the serving engine delivers them. Measure with
   `abhed-bench` (below) rather than trusting this table.
2. **Compaction costs less than first assumed — if you place the prefix correctly.**
   An earlier draft of this document claimed compaction "invalidates the prefix by
   construction." **Measurement showed that is wrong for Abhed's architecture.** Because
   the system prompt and `ABHED.md` live in the *system* message, outside the compacted
   history, compaction discards the conversation tail while the cached prefix survives.
   Measured penalty: ~1.0×, not the large cold-prefill hit predicted.
   This holds only while the prefix stays outside the summarized region — an
   implementation that compacts the memory file into the summary loses the property.
3. **MoE wins the agent workload** by ~6× on prefill at equal quality tier (P9).

### Measured vs computed

`cmd/abhed-bench` measures all of this on a real endpoint:

```
abhed-bench -base-url http://gpu:8000/v1 -model Qwen/Qwen3-32B -turns 40
```

It reports cache hit rate, prefill savings, cold vs warm TTFT, and the compaction
penalty, and it says plainly when an endpoint reports no cached tokens at all — which
means the capacity model here does not hold for that stack. **Savings scale with turn
count**, so a short run legitimately shows less than the 17× computed for 40 turns:
an 8-turn validation run measured 7.6× prefill savings and 13.7× TTFT speedup.

## 5. Quantization [E — unverified, must be measured]

| Format | Memory | Expected quality | Use |
|---|---|---|---|
| BF16 | 2 B/param | Reference | Eval baseline only |
| **FP8** | 1 B/param | Near-lossless on Hopper+ | **Default for serving** |
| AWQ/GPTQ INT4 | 0.5 B/param | Small but task-dependent loss | Memory-constrained tiers |
| INT4 + FP8 KV | 0.5 B + half KV | Compounding loss | T0/T1 only |

Research produced **no verified quantization quality numbers.** Do not accept vendor
claims of "lossless." Run your own eval harness (P10) on your own task distribution before
promoting any quantized model — agentic tool-calling degrades differently from the
text benchmarks quantization is usually validated on, and that difference is exactly what
would hurt Abhed.

## 6. Supporting infrastructure (per T2 node)

| Component | Spec | Notes |
|---|---|---|
| CPU | 2× 32-core | Tokenization, scheduling |
| System RAM | 1–2 TB | ≥1.5× total HBM for weight staging |
| Local NVMe | 8–15 TB | Model weights; 2–3 versions resident |
| Intra-node | NVLink/NVSwitch | Required for tensor parallel |
| Inter-node (T4+) | 400 Gb IB or RoCEv2 | Only needed above one node |
| Power | 8–10 kW/node | H100 SXM ~700 W each |
| Cooling | High-density rack | Often the real constraint in existing DCs |

**Non-GPU footprint** (control plane, execution pool, data plane) — CPU-only, sized per
~200 concurrent sessions:

| Service | Replicas | CPU | RAM |
|---|---:|---:|---:|
| Control plane (orchestrator) | 3 | 8 | 16 GB |
| Execution pool (microVMs) | 20–50 | 4 | 8 GB |
| Postgres (HA) | 3 | 16 | 64 GB |
| OpenSearch / vector | 3 | 16 | 64 GB |
| Registry + object store | 2 | 8 | 32 GB |
| Observability | 3 | 8 | 32 GB |

## 7. Cost envelope [E — indicative only, quote before committing]

| Tier | Hardware capex | Annual power @$0.12/kWh | 3-yr TCO estimate |
|---|---:|---:|---:|
| T1 workstation | $25–40k | ~$2k | ~$50k |
| T2 8×H100 node | $250–350k | ~$10k | ~$400k |
| T3 8×H200 node | $300–450k | ~$11k | ~$500k |
| T4 32×H100 | $1.1–1.5M | ~$42k | ~$1.7M |
| T5 64×H200 | $2.5–3.5M | ~$88k | ~$4M |

Excludes facility, network fabric, support contracts, and engineering. GPU pricing moves
fast and varies enormously by vendor relationship — **treat these as order-of-magnitude
only.**

## 8. Sizing procedure

1. Measure real concurrency: peak *simultaneously generating* sessions, not headcount.
2. Pick target context: 45k covers most coding turns; 128k for whole-repo reasoning.
3. Compute KV: `kv_per_token × ctx × concurrency`.
4. Add weights at FP8, then 15% for activations and fragmentation.
5. Round up to the next tier — **KV is bursty, and OOM under load is a hard failure.**
6. Validate prefix-cache hit rate above 70% before trusting any concurrency estimate.

Reproduce all numbers: `python3 docs/architecture/sizing.py`
