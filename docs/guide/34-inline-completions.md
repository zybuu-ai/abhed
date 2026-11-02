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
marked, at a low temperature, and only the text of its reply is used.

A chat or reasoning model often answers in prose, so the reply is cleaned
first: reasoning written into the text is removed, a fenced block gives its
code, and code repeated from around the cursor is trimmed. A reply that still
reads as an explanation is not offered; Studio is told why (`dropped`). For
dependable ghost text, use a coding model with fill-in-the-middle.

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
refused unless the managed file sets `model.completion` too. A model
`model.allowed` does not admit is refused for completions, next-edit
suggestions and commit messages, whichever file names it
([Allowed models and MCP servers](02-configuration.md#allowed-models-and-mcp-servers)). A workspace file
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
expansion (`${PASSWORD:-…}`) or a bracketed value is treated as a secret,
and so is a JSON object or list under such a name, plural too
(`APP_SECRETS={"db": "…"}`, `API_TOKENS=[…]`), when one of its strings is a
literal of eight or more characters. This happens before the
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

## Next-edit suggestions

After you change something, Studio can ask where the change leads: rename
a variable on one line and the next uses are suggested as edits. Studio
sends the lines around the cursor and your last few edits; the engine asks
the next-edit model, in a chat turn, for those lines as they would read after
the next edit, and turns the answer into edits of whole lines in the same
file.

The model is `model.next_edit`, or the completion model when it is unset.
Everything above holds: `completion.enabled` and `completion.local_only`
cover it, a managed model pin binds it, the same files are never sent, and
stored secrets and key-shaped text are withheld. A line that held a withheld
secret is never edited, since the model did not see what is there; a
suggestion that rewrites most of the window, carries a secret or reads as
prose is dropped and Studio is told why. The record holds
`next_edit.enabled` and `next_edit.summary` as it does for completions, and
the metrics count them as `abhed_next_edits_total` and
`abhed_next_edit_tokens_total`.

## Commit messages

Studio's Source Control view can ask for a message for what is staged, or,
with nothing staged, for the changes in the working tree. The engine reads
the change itself, with the same confined git it uses for its own reads, so
Studio never sends a diff. A staged file the session may not read, one that
looks like keys, or Abhed's state is left out of what is sent; the rest has
stored secrets and key-shaped text withheld, the diff's added and removed
lines included (a block scalar whose value changed under an unchanged key
too), with ten lines of context around each change, and is cut to 32 KiB. A file the repository's ignore rules
name is left out even when it was force-added, so a repository that ignores
everything and force-adds its files (a dotfiles repository, say) gets no
message. The model is `model.commit`, or the completion model, under the
same rules as completions.

Withholding is by shape, not a guarantee. A stored secret's value is always
withheld; beyond that, Abhed recognises keys and tokens by their form and
values by the name they sit under. It leaves alone a bare value of letters
only (`password: hunterhunter`), one that reads as code (ending in `;`, or
holding `(`, `[` or `<`), a key whose last word only names a secret
(`password_env`, `token_id`, `auth_header`), and the lines of a block
scalar or a private key block whose opening line is not in what is read: a
next-edit window that starts inside one, or a commit diff whose changed lines
are more than ten lines into one. Keep secrets in a store, not in files you
edit or commit.

The record holds `commit_message.requested`, before the diff goes, with how
many files were sent and left out and how many bytes, and
`commit_message.generated` with the outcome; the metrics count
`abhed_commit_messages_total`.
