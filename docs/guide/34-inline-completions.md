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

The provider is resolved as a session's model is: the key its `api_key_env`
names is the only key sent, never an `OPENAI_API_KEY` or other key from the
environment, and it must be one the configuration offers. Each request is
sent once, never retried. Under `local_only`, a redirect from the endpoint
to another machine is refused, whatever the provider's type.

The managed configuration can turn completions off or keep them local, and a
lower layer cannot undo it. A managed `model.default` pins completions as it
pins the session's model: a `model.completion` naming another provider is
refused unless the managed file sets `model.completion` too. A workspace file
can only turn them off or set `local_only`, and cannot name the provider: the
code goes to it.

## What is sent

Only what is near the cursor: up to 6,000 characters before it and 2,000
after it, and the first 60 lines of up to three other open files when Studio
sends them, at most 4 KiB of each and 12 KiB in all. Every cut falls between
characters. A file outside the session's workspace, one a read rule keeps
from the agent, one that looks like it holds keys (`.env`, `*.pem`, SSH
keys), or Abhed's own state (`.abhed`) is never sent, as the document or as
context; the request is answered with nothing. A file reached through a link
is checked both as named and where the link leads, so a link out of the
workspace or to a key file is refused too.

What is sent has every stored secret's value withheld, as the agent's own
messages do, and anything shaped like a key replaced: a private key block,
a cloud, forge, chat or payment token, a JWT, a password in a URL, an
upper-case `*_TOKEN`, `*_PASSWORD` or `*_KEY` given a literal value (quoted,
or bare and not code such as a call or `settings.API_KEY`). A
placeholder is not a secret and is sent as written: a format verb (`%s`),
an expansion naming a variable with no default (`${NAME}`, `$NAME` in
capitals), a bracketed name (`{api_key}`, `<your key>`, `[REDACTED]`,
`{{ .Token }}`), `***`, `xxx`, `changeme`, `your_token_here`, or a URL
whose password is its user name (`postgres:postgres`). A default inside an
expansion (`${PASSWORD:-…}`), a bracketed value or JSON is treated as a
secret. This happens before the
text is cut to size, and over the code on both sides of the cursor as one,
so neither a cut nor the cursor can leave part of a secret. A suggestion
that carries one is dropped rather than offered. A token you are still
typing cannot be recognised until enough of it is there to match its shape;
a stored secret's value is withheld once it is complete. Requests to the
model go through the same egress path as the agent's, so under the allowlist
the rules apply.

The engine keeps the requests reasonable: one in flight per session (a newer
one replaces it, and Studio's `seq` decides which is newer when requests
cross), a short wait before asking so that a burst of keystrokes sends one
request, at most five a second per session and ten a second over all of an
editor's sessions. A provider whose completions endpoint answers 404, 405 or
501, or a 400 that says the endpoint or its `suffix` is not supported, is
asked through chat from then on in that session; any other 400 is a failure.
A closed session's requests are refused.

## What is recorded

Not each completion: there are too many. The record holds
`completion.enabled` just before a session first sends code to the provider,
naming it, its model, whether it is on this machine, and whether it was asked
by fill-in-the-middle or chat. It is recorded again, with `chat`, before the
first chat request of a session that fell back to chat. If it cannot be
recorded, nothing is sent.
`completion.summary`, when the session closes, counts the requests sent and
how many were answered, failed or cancelled after sending, and those refused
or superseded (replaced by a newer one) before anything went, with the tokens
used. The token totals are a lower bound: a request cancelled after it went
may have cost tokens the provider never reported. The metrics count the same
(`abhed_completions_total`, `abhed_completion_tokens_total`).
