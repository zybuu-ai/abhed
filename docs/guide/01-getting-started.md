# Getting started

## Install

Abhed is a single static binary with no runtime to install.

```bash
go build -o ~/.local/bin/abhed ./cmd/abhed
abhed -version
```

It runs without internet access. Once built, the only outbound connection is
to the model endpoint you configure, plus anything you enable explicitly (web
search, web fetch, MCP servers, remote tools); copy the binary and a config into an
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

While it works, **you can keep typing.** The input box stays at the bottom of
the screen. A message sent mid-run steers it at the next step rather than
interrupting, so the files it has already read and the results it has already
gathered are kept; it shows under the reply as queued until the agent takes
it. A slash command typed mid-run is queued and runs when the turn finishes.
Esc stops the turn and keeps the session.

The reply streams as it is written, formatted as it arrives. Tool calls show
as one line each, with the first and last lines of their output and, for an
edit, the diff; Ctrl-O opens the whole transcript with every output in full.

When it needs approval it stops and shows the change, why it asks, and
numbered answers:

```
╭─ Write(notes.txt)
│ changing a file needs approval in default mode · policy step: default
│   ⎿  Created notes.txt with 1 line
│      1 + hello
│ Create notes.txt?
│   1. Yes
│   2. Yes, and don't ask again for write(notes.txt) this session
│   3. No, and tell Abhed what to do instead (esc)
╰─ number or ↑↓ then enter · esc to decline
```

Press a number, or move with the arrows and press Enter. Nothing is selected
at first, so Enter alone answers nothing. No key counts for the first 300 ms
the question is on screen, and a key only counts when it stands alone, with
300 ms of quiet before and after it: typing that was meant for the prompt,
or a key held down, never answers. "2" allows that scope for the rest of the
session; `/clear` and `/resume` start another session without it. "3" or Esc
refuses and stops the turn, so you can say what to do instead. A destructive
command offers no "always", and asks a second time, with No as the default.
The question, the diff and your answer stay in the transcript.

## The prompt

Enter sends; Shift+Enter, Alt+Enter, Ctrl-J, or a backslash then Enter start
a new line. A paste of more than a few lines shows as `[Pasted text #1 +25
lines]` and is sent whole, as one message; Tab after it opens it for editing.
Up and Ctrl-R reach the prompts you sent in this workspace, in this session
and earlier ones. Shift-Tab steps through the default, accept-edits and plan
modes; the footer always shows which is on. Ctrl-C clears the line, stops a
running turn, and at an empty prompt pressed twice exits. Every key is in
[The terminal](18-terminal.md).

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
| `/clear` | start a new conversation and session; `/cost`, `/diff` and `/undo` start over, the workspace is kept |
| `/resume <id>` | replay a recorded session and continue its conversation |
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
refused, as is an unknown `-mode`. A word after the flags must be a command (`abhed -h` lists them,
`abhed version` prints the version): anything else exits 2 rather than
opening a session, so pass a prompt with `-p`.

## Next

- [Configuration](02-configuration.md) — the settings that matter
- [Permissions](04-permissions.md) — before you run `-mode auto` on anything real
