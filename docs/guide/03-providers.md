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
apps**. Anthropic accepts the credential and then refuses the request unless the
system prompt is its own client's. Measured directly: same token,
same model, same second, the only difference being the first system block.

| First system block | Result |
|---|---|
| exactly Anthropic's client identity line | 200 |
| that line with anything appended | 429 |
| any other prompt, or none | 429 |

The refusal arrives as `429 rate_limit_error`, which is misleading — it is a
policy decision, not a limit that clears. Abhed recognises the shape (a 429
carrying none of the headers a real rate limit carries), reports it as what it
is, and does not retry.

Abhed reads `CLAUDE_CODE_OAUTH_TOKEN` and `oauth_token` because the mechanism is
correct and the restriction may not be permanent. Today it is useful only for
models outside the check.

Working around it means sending Anthropic's client identity string from a
product that is not that client. That circumvents an access control, misrepresents the
product, and breaks the moment the check changes — so Abhed does not do it, and
neither should anything built on it.

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
and still cheaper than rebuilding the session by hand.

## Choosing one

`abhed doctor` tells you whether a model can drive the agent at all.
`abhed-modelcmp` compares candidates on tool calling and explanation quality,
which are different things and are not measured by the same benchmark.

Decode speed tracks *active* parameters, not total: a 27B dense model can be far
slower than a 30B mixture-of-experts.
