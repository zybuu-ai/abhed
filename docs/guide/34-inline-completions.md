# Inline completions

Abhed Studio's ghost text, the code it suggests as you type, comes from a
completion model the engine asks. It is text only: no tools, no commands,
nothing changed until you accept a suggestion in the editor.

## Turning it on

Name the provider in `model.completion`:

```json
"model": {
  "default": "claude",
  "completion": "coder",
  "providers": {
    "coder": {"type": "ollama", "model": "qwen2.5-coder:7b"}
  }
}
```

Unset, completions are off and Studio says so. A local coding model with
fill-in-the-middle support is the recommendation: `qwen2.5-coder` on Ollama,
vLLM or llama.cpp. For those types (`ollama`, `vllm`, `llamacpp`, `tgi`,
`sglang` and `openai-compatible`) the engine asks the provider's completions
endpoint with the code before and after the cursor. Abhed does not start
Ollama for this; it uses it only when it is configured and running. Any other
provider, a hosted one included, is asked through a chat turn with the cursor
marked, and only the text of its reply is used.

| Setting | Default | |
|---|---|---|
| `model.completion` | none | the configured provider completions use |
| `completion.enabled` | `true` when `model.completion` is set | `false` turns them off |
| `completion.local_only` | `false` | `true` uses only a provider on this machine (a loopback address, `localhost`, or a local server type left at its default endpoint); any other is refused |

The managed configuration can turn completions off or keep them local, and a
lower layer cannot undo it. A workspace file can only turn them off or set
`local_only`, and cannot name the provider: the code goes to it.

## What is sent

Only what is near the cursor: up to 6,000 characters before it and 2,000
after it, and the first 60 lines of up to three other open files when Studio
sends them. A file outside the session's workspace, one a read rule keeps
from the agent, or one that looks like it holds keys (`.env`, `*.pem`, SSH
keys) is never sent, as the document or as context; the request is answered
with nothing. Requests to the model go through the same egress path as the
agent's, so under the allowlist the rules apply.

The engine keeps the requests reasonable: one in flight per session (a newer
one replaces it), a short wait before asking so that a burst of keystrokes
sends one request, and at most five a second.

## What is recorded

Not each completion: there are too many. The record holds
`completion.enabled` the first time a session asks the provider, naming it,
its model, whether it is on this machine, and whether it was asked by
fill-in-the-middle or chat; and `completion.summary` when the session closes,
with how many were asked, answered, refused, failed and cancelled, and the
tokens used. The metrics count the same (`abhed_completions_total`,
`abhed_completion_tokens_total`).
