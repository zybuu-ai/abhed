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

The request always carries a suffix, a newline at the end of a file:
Ollama applies a coder model's fill-in-the-middle template only when there
is one, and without it the model answers the code before the cursor as a
chat ("It looks like you're working on…").

A chat or reasoning model often answers in prose, so the reply is cleaned
first: reasoning written into the text is removed, a fenced block gives its
code, and code repeated from around the cursor is trimmed, including the
rest of the cursor's line written again at the end of a one-line answer. A
reply that still reads as an explanation, or opens as an English sentence
where code is written (not in a comment, a docstring or a string, one that
spans lines included, nor in
Markdown, plain text or a configuration file), is not offered; Studio is
told why (`dropped`). Code shaped like a sentence (`None if x is None else
x`) is not taken for one: a sentence must also have a contraction, a pronoun
or an article. Studio then parses the file with the suggestion in place, for
the languages it has a grammar for (as for next edits, below), and does not
show one whose own text fails to parse or that leaves more syntax errors
than the file had. This fails closed: a one-line block opener or an
unfinished line offered alone (`if err != nil {` in Go, `x := foo(`) adds
an error until its body or its close is written, so it is not shown; the
next keystroke asks again. The check is incremental (the file as it is is
parsed once per version), and a file over 256 KB is not checked: the
engine's prose filter stands. For
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
request, at most five requests sent a second per session and ten a second
over all of an editor's sessions. A request replaced during the wait is not
sent and does not count, so the last keystroke of a burst is the one asked
about. A provider whose completions endpoint answers 404, 405 or
501, or a 400 that says the endpoint or its `suffix` is not supported, is
asked through chat from then on in that session; any other 400 is a failure.
A closed session's requests are refused.

## How fast it is

Ghost text is only useful while you are still looking at the line, so
Studio and the engine keep each suggestion short and the model busy only
with the latest one:

- **Warm-up.** When you open a file Studio may complete in a window whose
  editor session is already open, or when that session opens with your
  first keystroke, Studio asks the engine to load the completion model:
  one fill-in-the-middle request with an empty prompt and one token, so no
  code is sent. It goes at most once every two minutes per session and per
  editor connection, and ends when the session closes or the editor's
  connection does. A cold local model
  takes seconds to load; this way it is loaded before you need it. Only a
  model whose address is on this machine is warmed, decided by its URL as
  `local_only` is: a loopback address, `localhost`, or a local server type
  at its default endpoint. A proxy on this machine that forwards to a model
  elsewhere counts as local, so its model is warmed through it (still with
  no code). Opening a file never starts the engine or opens a session for
  a warm-up alone.
- **Completions first.** A local server often answers one request at a
  time. A completion waiting for the server goes ahead of next-edit calls
  and agent turns that wait for it, and while one is asked for, next-edit
  calls to that server wait and one already sent is cancelled; a hosted
  server serves them side by side, so nothing waits there. A rename
  finished from the file needs no model and is shown at once; on the
  renamed line, in the languages the engine finishes renames in by itself
  (Python, Go, JavaScript, TypeScript, Java), Studio shows it instead of
  asking for ghost text.
- **Debounce and cancel.** Studio waits briefly after a keystroke before
  asking and sends its `seq`; a newer keystroke supersedes the older
  request, and Studio cancels a request whose place in the file has gone,
  so the model stops working on it. A request over the rate limit is asked
  once more if its place still stands.
- **One logical block.** A suggestion is the statement at the cursor and,
  when it opens a block (a line ending in `:` or `{`, say), that block's
  body and closing line: it ends at a blank line (outside a string such as
  a docstring) or at a line back at the statement's own indentation, and is
  at most 12 lines. The model's answer is streamed and ended as soon as the
  block is whole, so it does not write a whole function you will not take;
  the model's end-of-text and file markers are its stop sequences.
- **Again after Tab or Enter.** Accepting a suggestion, or pressing Enter
  (a new line, with its indentation), asks for the next one shortly after;
  a paste or another program's change does not.

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
used, and how long answers took: the median, 90th percentile and longest
time from a request reaching the engine to its answer (`latency_p50_ms`,
`latency_p90_ms`, `latency_max_ms`, of requests sent to the model only),
and the warm-ups with the longest (`warmups`, `warmup_ms`). Only counts and times are recorded, never code.
`abhed hawkeye` shows them for the session. The token totals are a lower
bound: a request cancelled after it went may have cost tokens the provider
never reported, and a streamed answer cut at its block is counted a token
a chunk when the provider reports no usage. The metrics count the same
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
prose is dropped and Studio is told why. The answer is bounded by the window
it rewrites (about half its bytes in tokens, at most 1,024); an answer cut at
that bound is dropped rather than read as a window with lines missing.
Studio cancels the request in flight on each keystroke. The record holds
`next_edit.enabled` and `next_edit.summary` as it does for completions, and
the metrics count them as `abhed_next_edits_total` and
`abhed_next_edit_tokens_total`.

A rename needs no model. When your edits on a line turned one identifier
into another (`step` into `stride`, say), the other uses of the old name
are the next edit, and the engine finds them in the lines Studio sent. They
are offered as one edit, taken with Tab, and nothing is sent to any model
for it:

- When a language server is running for the file, its references of the
  old name, asked at the use nearest the rename, are the uses.
- Otherwise, in Python, Go, JavaScript, TypeScript and Java, the engine
  scans the innermost function the rename is in (the window, at the top
  level of a file), skipping strings and comments, members (`user.name`,
  `p->name`, `A::name`), keyword arguments in a call (`f(name=1)`; a
  parameter's default on the function's own line is renamed) and, in
  JavaScript, TypeScript and Go, the keys of an object or composite literal
  (`{name: 1}`, `T{name: 1}`). A ternary's `? name :`, a `case name:`, a
  typed declaration and a Go map literal's key (`map[string]int{name: 1}`)
  are uses, and so are a nested map literal's keys with its type left out.
  A shorthand property (`{ name, age }`), a destructuring (`const { name }
  = o`, `for (const { name } of xs)`, a parameter's `({ name })`) or an
  object inside a JSX expression container (`style={{ name }}`) is both key
  and variable, so it is written out (`{ name: title, age }`) and the
  object keeps its key, whether the scan or a language server found it. A
  name in a block or arrow body (`{ name }`), a template's `${name}` or a
  JSX expression container (`<p>{name}</p>`, `<Item value = {name} />`) is
  a use, renamed in place. Where Abhed cannot tell which a brace is (an
  `= {` in JSX with no tag or declaration on its line, an object inside a
  template's `${ }`), the rename is left to the next-edit model rather
  than guessed. Where the old name is bound again in a nested
  function, lambda, comprehension or inner block, nothing is offered: a
  textual rename would change another variable, so it is left to you.
  Rust, C, C++ and JSX files (`.jsx`, `.tsx`, where attribute names and
  the text between tags read like names) take only a language server's
  uses; other languages get no rename from the file. A `.js` file that
  holds JSX (a tag where a value goes, a tag with an attribute anywhere,
  `</`, `/>` or `<>` in its code, even with its closing tag past the lines
  Studio sent) is treated like `.jsx`:
  only a language server's uses, read with the JSX rules, else the model.
- Not every one-word edit is a rename. A new name that is a reserved word
  of the language (`class`, `null`, `int`) or already used in that
  function, an old name with no other use (a typo fixed on its only use),
  an edit at a use of a name declared elsewhere in the function, or one
  outside any function the window shows, is not taken for a rename: the
  next-edit model is asked as for any other edit.

The summary counts these as `local`, and renames left to you as
`local_dropped`; neither is in the answer times.
`abhed_next_edits_total` counts them as the outcomes `local` and
`local_dropped`, and `abhed hawkeye` shows them.

Before Studio shows any next-edit suggestion, from a rename or a model, it
parses the file with the suggestion applied (tree-sitter, for Python, Go,
JavaScript, TypeScript, Rust, Java, C, C++, C#, Ruby, PHP and shell) and
drops one that leaves more syntax errors than the file had. A parse that
fails, or a file too large to check in full, drops the suggestion too. For
languages with no grammar, a suggestion that adds unbalanced brackets is
dropped.

## Commit messages

Studio's Source Control view can ask for a message for what is staged, or,
with nothing staged, for the changes in the working tree. The engine reads
the change itself, with the same confined git it uses for its own reads, so
Studio never sends a diff. A staged file the session may not read, one that
looks like keys, or Abhed's state is left out of what is sent; the rest has
stored secrets and key-shaped text withheld, the diff's added and removed
lines included (a block scalar whose value changed under an unchanged key
too, with or without a newline at the file's end), with ten lines of context
around each change, and is cut to 32 KiB. A file the repository's ignore
rules name is left out even when it was force-added, so a repository that
ignores everything and force-adds its files (a dotfiles repository, say)
gets no message. The model is `model.commit`, or the completion model, under the
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
