# Getting started

## Install

Abhed is a single static binary with no runtime to install.

```bash
go build -o ~/.local/bin/abhed ./cmd/abhed
abhed -version
```

It runs without internet access. Once built, the only outbound connection is
to the model endpoint you configure, plus anything you enable explicitly (web
search, MCP servers, remote tools); copy the binary and a config into an
enclave and it works there.

## Point it at a model

Abhed does not ship a model. It needs an endpoint, and the fastest one to get
running is a local server:

```bash
ollama serve
ollama pull qwen3-coder:30b
```

Then, in the directory you want the agent to work in:

```bash
abhed init      # writes .abhed/config.json
abhed doctor    # check the endpoint before you rely on it
```

`abhed doctor` is worth the ten seconds. It checks that the endpoint answers,
**and that the model can emit a structured tool call** — a model that describes a
tool call in prose instead of emitting one cannot drive an agent at all, however
well it writes, and finding that out during a real task wastes the run.

```
checking endpoint... ok
checking tool calling... ok
  called glob with {"pattern":"*.go"}

Ready.
```

## First run

```bash
abhed
```

Type a task. Abhed reads code, runs commands, edits files, and asks before
anything it is not permitted to do unattended.

```
⬢ the tests in pkg/auth are failing — find out why and fix it
```

While it works, **you can keep typing.** A line sent mid-run steers it at the
next step rather than interrupting, so the files it has already read and the
results it has already gathered are kept:

```
⬢ actually, only look at token.go
  steering — applied at the next step
```

A slash command typed mid-run is queued and runs when the turn finishes.

When it needs approval it stops and shows the change:

```
● write notes.txt
  + hello
  [a]ccept  [r]eject  [A]lways allow write(notes.txt)
```

Press `a` or `y` to accept, `r` or `n` to reject, `A` to allow that scope for
the rest of the session. A key answers only on its own, on an empty line, with
300 ms of quiet before and after it (600 ms after `A`); Enter alone never
accepts. Anything else, "Actually no" included, is typing: it is kept as a
steering message and sent with Enter. Ctrl-C refuses the request and stops the turn; a second Ctrl-C, if
the turn has not stopped, ends the session.

## The prompt

Arrow keys move and recall history; Home, End, Ctrl-A, Ctrl-E, Ctrl-U, Ctrl-K
and Ctrl-W do what they do in a shell. Ctrl-C stops the running turn without
ending the session; at the prompt it clears the line. Ctrl-D exits.

| Command | |
|---|---|
| `/help` | list commands |
| `/mode <name>` | `default`, `accept-edits`, `plan`, `auto` |
| `/diff` | what changed this session |
| `/undo` | revert the last turn's file changes |
| `/cost` | tokens, cache hit rate, compactions |
| `/compact` | compact the context now |
| `/tree` | the session's steps |
| `/fork <step>` | rebuild the conversation up to a step and continue from there |
| `/export [path]` | write the transcript, HTML by default |
| `/model [name]` | show or switch the model, keeping the conversation |

## Without a terminal

```bash
abhed -p "explain what pkg/auth does" -mode plan
abhed -p "fix the failing tests" -mode auto -allow 'bash(go test*)'
abhed -p "add a test for Valid" -output-format json > events.jsonl
```

Exit codes: `0` completed · `2` turn limit · `3` budget · `4` policy denied ·
`5` retries exhausted · `130` interrupted.

`-output-format` is `text` or `json`, one event per line; any other value is
refused. A word after the flags must be a command (`abhed -h` lists them,
`abhed version` prints the version): anything else exits 2 rather than
opening a session, so pass a prompt with `-p`.

## Next

- [Configuration](02-configuration.md) — the settings that matter
- [Permissions](04-permissions.md) — before you run `-mode auto` on anything real
