# Abhed — Model providers and sampling parameters

Status: 2026-09-06

Abhed's value is the harness, and a harness is only as good as the models it can
drive. The adapter seam is what keeps "swap the model" a config change rather
than a port, so the provider list is a capability of the product, not a
convenience.

## Providers

`abhed providers` lists what the binary in hand supports — which is the set that
registered itself at init, so a build that omits the cloud adapters for an
air-gapped install reports honestly.

| Type | Wire format | Notes |
|---|---|---|
| `anthropic` | Messages API | explicit `cache_control` on the system block; thinking blocks |
| `openai` | chat/completions | |
| `gemini` | generateContent | `contents`/`parts`, thought parts, no call ids |
| `watsonx` | watsonx text/chat | project- or space-scoped |
| `vllm`, `sglang`, `llamacpp`, `tgi`, `ollama` | chat/completions | local servers; accept `top_k`, `min_p`, `repetition_penalty` |
| `groq`, `together`, `openrouter`, `mistral`, `deepseek`, `xai` | chat/completions | hosted gateways |
| `azure-openai` | chat/completions | `base_url` is the deployment endpoint |
| `bedrock-anthropic`, `vertex-anthropic`, `vertex-gemini` | as the underlying model | need a signed endpoint or a supplied token — see below |
| `openai-compatible` | chat/completions | generic escape hatch, kept for existing configs |

Three wire formats cover all of them. The OpenAI-shaped providers share one
adapter and differ only in default URL and which sampler knobs the server
honours; Anthropic and Gemini are genuinely different shapes and have their own.

### Cloud gateways and credentials

Abhed carries no cloud SDK, deliberately: a vendored AWS or Google SDK is a
large dependency an air-gapped bundle has to justify. So Bedrock and Vertex take
a token the operator supplies through `api_key` — from a sidecar, a short-lived
credential, or a signing proxy named in `base_url`.

That is a real limitation and it is stated rather than hidden. An adapter that
silently cannot authenticate is worse than one that says what it needs.

## Sampling parameters

Every provider takes a `params` object:

```json
{
  "model": {
    "default": "local",
    "providers": {
      "local": {
        "type": "vllm",
        "base_url": "http://127.0.0.1:8000/v1",
        "model": "qwen3-coder:30b",
        "params": {
          "temperature": 0.2,
          "top_p": 0.9,
          "top_k": 40,
          "repetition_penalty": 1.05,
          "seed": 42
        }
      }
    }
  }
}
```

| Parameter | Meaning |
|---|---|
| `temperature` | logit scaling; 0 is greedy |
| `top_p` | nucleus sampling |
| `top_k` | candidate-set cap (local servers, Anthropic, Gemini) |
| `min_p` | probability floor relative to the top token (local servers) |
| `repetition_penalty` | divides logits of tokens already produced (local servers) |
| `frequency_penalty`, `presence_penalty` | OpenAI's two-term formulation |
| `seed` | reproducible sampling where supported — what makes two eval runs comparable |
| `max_tokens` | response cap |
| `stop` | stop sequences |
| `effort` | reasoning budget as a level: `low`, `medium`, `high` |
| `think` | turns a hybrid model's thinking phase on or off |
| `thinking_budget` | thinking budget as a token count (Anthropic, Gemini) |

**An omitted parameter is not the same as zero.** Every field is a pointer, so
leaving `temperature` out leaves the model's own default alone, while setting it
to `0` asks for greedy decoding. Collapsing those two would make "don't touch
it" impossible to express.

**A parameter the provider cannot honour is refused at startup**, naming the
knob and the provider. This matters more than it looks: `min_p` sent to a hosted
API is ignored silently, and the evidence is nowhere in the output — the answers
are simply drawn from a distribution nobody chose. Better to fail the config
than to run something that looks configured and is not.

Precedence is `request > provider params > model default`, so a caller can pin
one knob without restating the rest.

## Adding a provider

One file with an `init` that calls `model.Register`. No factory switch to edit,
no config parsing to touch. If it speaks the OpenAI format, it is a single line
naming its default URL and its sampling set.


## Authenticating with a subscription

**A Claude Pro or Max token does not work outside Anthropic's own apps**, and
this is enforced rather than merely written down. Anthropic accepts the
credential and then refuses the request unless the first system block is its
own client's identity line; the refusal arrives as `429 rate_limit_error`, which reads as a
limit that will clear and is not one.

Abhed still reads `CLAUDE_CODE_OAUTH_TOKEN` and `oauth_token`: the mechanism is
correct, the restriction may not be permanent, and models outside the check do
answer. It recognises the refusal by its shape — a 429 carrying none of the
headers a real rate limit carries — reports what it actually is, and does not
retry a decision that will not change.

Working around it means sending Anthropic's client identity string from
something that is not that client. Abhed does not, and nothing built on Abhed should:
it circumvents an access control, misrepresents the product, and breaks the
moment the check changes.

Use an API key. And note that third-party usage under a subscription is billed
as extra usage rather than drawn from the plan, so it is metered in claude.ai
settings and not in the Console — a subscription token is not a Console
credential.

```bash
export ANTHROPIC_API_KEY=sk-ant-api03-...
```

An OpenAI-shaped endpoint authenticates with a bearer token either way, so a
key and a subscription token take the same path there; only the source differs.
`OPENAI_API_KEY` is read when no key is configured.

**A token and a key are never sent together.** Sending both would let the server
choose, which makes "which account paid for this" depend on someone else's
precedence rules rather than on what the operator configured.
