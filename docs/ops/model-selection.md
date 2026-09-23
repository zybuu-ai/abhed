# Abhed — Choosing a Model

Measured on an Apple M3 Pro, 36 GB unified memory, Ollama 0.33.2, September 2026.
Reproduce with `abhed-modelcmp`; the numbers below are from this machine and will
differ on yours.

## The short answer

For a **deep agent** — long autonomous tasks, architecture work, code review,
end-to-end builds — the choice is not "the model with the best benchmark". It is
the model that keeps working correctly for fifteen turns without inventing
things, at a speed that lets a person stay in the loop.

On this hardware those two properties point at different models, and the honest
answer is that neither candidate wins outright.

| | `gemma4:26b` (MoE) | `qwen3-coder:30b` (MoE) | `qwen3.8:27b` (dense) |
|---|---|---|---|
| Architecture | 128 experts, **8 used** | 128 experts, **8 used** (~3B active) | all 27.3B active, 65 layers |
| Decode | 34.9 tok/s | **47.4 tok/s** | 3.2 tok/s |
| Prefill | 80.4 tok/s | **114.9 tok/s** | 19.2 tok/s |
| Tool calls (4 cases) | 4/4 | 4/4 | 4/4 |
| Single-bug task | 73s, 10 turns | 77s, 15 turns | 473s, 8 turns |
| Wrote failing tests first | **yes** | no | no |
| Two-bug review: found both | **yes** | no — fixed 1, misnamed the other | not run |
| Ran what it was asked to run | **yes** | **no — and said it had** | yes |
| Reported output matched reality | **yes** | no | yes |

## Why the 15× speed gap

Both models fit in memory, so this is not a swapping problem. It is arithmetic:
a Mixture-of-Experts model activates ~3B parameters per token, a dense 27B model
activates all of them. Decode is memory-bandwidth-bound, so the dense model reads
roughly nine times more weight data per token and runs about fifteen times slower.

This is the same effect `docs/architecture/04-sizing.md` §P9 predicts for server
GPUs ("MoE wins the agent workload by ~6× on prefill at equal quality tier"). It
is larger on a laptop because unified memory bandwidth is the tighter constraint.

**Do not choose a local model by parameter count.** Check whether it is MoE, and
how many experts are active. `curl localhost:11434/api/show -d '{"model":"..."}'`
reports `expert_count` and `expert_used_count`.

## Why the fast model is not automatically the right one

`qwen3-coder:30b` is instruction-tuned for code, and it shows in two ways that
matter more than speed:

1. **It claimed to have run tests it had not run.** In one run a `bash` call was
   rejected by policy; the model reported the tests as passing anyway. In another
   it said it "couldn't execute `go test` due to environment limitations" when
   the command ran fine seconds later in the same workspace.
2. **Its explanations read like reference material.** Asked to explain LLMs
   simply, it produced a headed, bulleted outline. It *can* do better — asked
   directly, without Abhed's system prompt, it gave a genuinely good analogy —
   which points at the prompt as much as the model (see below).

A model that misreports whether it verified its own work is the single most
dangerous failure mode in an autonomous agent, because every downstream decision
inherits the false premise.

## The system prompt is part of the answer

`internal/agent/prompt.go` says:

> Answer concisely. The user is a working engineer, not an audience.

That is right for a coding turn and wrong for a teaching one. The same model,
same weights, produced a headed outline through Abhed and a clear analogy when
asked directly. Before blaming a model for its explanations, check what the
harness told it to be.

## Thinking modes

`qwen3.8:27b` has a thinking phase on by default. It is controllable **only on
Ollama's native `/api/chat`** with `"think": false`:

| Endpoint | Control | Works |
|---|---|---|
| `/api/chat` | `"think": false` | yes — 3m22s → 55s on one prompt |
| `/v1/chat/completions` | `"think": false` | **silently ignored** |
| `/v1/chat/completions` | `chat_template_kwargs.enable_thinking` | **silently ignored** |

Abhed speaks the OpenAI protocol, so `model.Think` is wired through config and
sent, but Ollama drops it. Set `think` in the provider config for servers that
honour it (vLLM, SGLang); on Ollama today it has no effect, and the model's
thinking phase cannot be disabled through Abhed.

## Recommendation

**`gemma4:26b` is the default.** It gives up ~26% throughput against
`qwen3-coder:30b` and buys back the thing that matters more.

The deciding test was a two-file review with a data race in `Store.List()` and
an authorization hole letting any user read any order. Neither bug was pointed
at. `gemma4:26b` found both, fixed both, ran `go build` and `go vet` itself, and
reported the real output. `qwen3-coder:30b` fixed the race, then added an
empty-string check on a query parameter and called *that* the security fix — the
authorization hole is still open — and claimed "permission restrictions" that
did not exist.

On the simpler single-bug task, gemma4 also wrote the failing tests *first*, saw
them fail with real output, then fixed the code and re-ran. That is the working
method the system prompt asks for, and it was the only model that followed it
unprompted.

- **Default, and interactive work:** `gemma4:26b`. 35 tok/s is comfortably
  interactive, and it verifies its own work.
- **When throughput dominates and you will check the output yourself:**
  `qwen3-coder:30b`, ~26% faster. Do not leave it unattended.
- **Avoid:** `qwen3.8:27b` on this hardware. Correct and honest, but dense, so
  3 tok/s makes it unusable interactively.
- **Neither is a frontier model.** Both are ~30B models on a laptop. For the deep-agent
  workload Abhed targets, a served `gpt-oss-120b` on the OCP cluster remains the
  intended production path; these are the development stand-ins.

## Reproducing

```bash
go build -o /tmp/abhed-modelcmp ./cmd/abhed-modelcmp
/tmp/abhed-modelcmp -models qwen3-coder:30b,qwen3.8:27b -json results.json
```

The tool checks four tool-calling behaviours (single tool, choosing among
several, declining when none is needed, non-trivial arguments) and captures
three prose answers with only decidable metrics — word count, headings, bullets,
whether an analogy appears. Prose quality is left to a person reading
`results.json`, because scoring it automatically would need another model's
opinion and would not be evidence.

## watsonx.ai and gpt-oss-120b

Abhed speaks watsonx directly (`"type": "watsonx"`), so an IBM deployment can
use a model served there rather than a local one.

```json
{
  "model": {
    "default": "gptoss",
    "providers": {
      "gptoss": {
        "type": "watsonx",
        "base_url": "https://us-south.ml.cloud.ibm.com",
        "model": "openai/gpt-oss-120b",
        "api_key_env": "WATSONX_API_KEY",
        "space_id": "…",
        "context_window": 131072,
        "max_output_tokens": 4096
      }
    }
  }
}
```

`space_id` **or** `project_id`, never both — the API rejects a request carrying
each, so `abhed doctor` fails at startup rather than on the first turn.
Authentication exchanges the API key for an IAM token, cached and refreshed a
minute before expiry.

### Two failures worth knowing about

**The system prompt is a separate field.** `Request.System` is not a message,
and an adapter that only walks `Request.Messages` sends none. Nothing errors:
the model answers, just without any working method, tool guidance or
environment. It showed up as the agent doing one tool call and stopping.

**gpt-oss-120b sometimes leaves its tool call in the reasoning channel.** The
reasoning ends with the bare arguments —

```
...Maybe there are more; let's do deeper search.{"pattern":"**/*.go"}
```

— and no `tool_calls` delta ever arrives. The loop has nothing to dispatch and
the turn is silently empty. Salvage now searches reasoning as well as content,
and matches bare arguments against the offered tools' schemas, recovering the
call only when exactly one tool fits. Two candidates means no recovery:
inventing a call the model did not make is worse than the stall.

Salvaged calls get a synthesised id, because watsonx returns 400 on a
`tool_calls` entry without one.

### Measured behaviour

On the two-bug review task (a data race in `Store.List()` and an authorization
hole), `gpt-oss-120b` ran 6 turns in 24s, read both files, and edited one — but
fixed **neither** bug. The edit compiled and changed nothing that mattered.

That is worse than `gemma4:26b`, which found and fixed both. Speed is not the
problem: 24s is faster than gemma4's 73s. Treat the watsonx path as working
transport with an unproven model, and run your own comparison before switching
a deployment to it.
