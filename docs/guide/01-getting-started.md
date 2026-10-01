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

The first time you run `abhed` on a terminal with no configuration
anywhere, it offers to set one up:

- It looks for Ollama (at `OLLAMA_HOST`, or `localhost:11434`) and lists
  its models.
- Or it takes an OpenAI-compatible endpoint: its base URL, and the **name**
  of the environment variable that holds its key. A key itself is never
  written: an answer that looks like a key (a known prefix such as `sk-`,
  `hf_`, `gsk_` or `AIza`, or a long run of mixed letters and digits) is
  refused and not echoed, and a name that is not set in your shell is taken
  only if you answer yes. Before a key goes to another machine over plain
  `http://`, it asks, defaulting to no.
- It checks that the model can call a tool, since a model that cannot will
  do little as an agent.
- It asks once whether the agent may keep memory notes of its own between
  sessions. The answer defaults to No: a note the agent writes is a way for
  text planted in a file to persist.
- It writes only your own `~/.abhed/config.json` (mode 0600), and only after
  you confirm. `s` skips it and writes nothing.

Or, in the directory you want the agent to work in:

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

The session opens at once: a check for a container runtime, and for the
model endpoint, run behind the prompt. The banner says `process or stronger
(checking)` until the sandbox is chosen, and commands wait for that choice.
An endpoint that is down is named with what to do about it, and a task
then fails at once rather than after the retries.

Type a task. Abhed reads code, runs commands, edits files, and asks before
anything it is not permitted to do unattended. A quoted task on the command
line starts the session with it: `abhed "the tests in pkg/auth are failing"`,
or `abhed -- the tests in pkg/auth are failing`.

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
the rest of the session; `/clear` and `/resume` start another session without
it. A key answers only on its own, on an empty line, with 300 ms of quiet
before and after it (600 ms after `A`); Enter alone never accepts. Anything
else, "Actually no" included, is typing: it is kept as a steering message and
sent with Enter. Ctrl-C refuses the request and stops the turn; a second
Ctrl-C, if the turn has not stopped, ends the session, recorded as
`user_interrupt`.

With input piped in as lines, the same holds line by line: only a line that
is exactly `a`, `y`, `r`, `n` or `A` answers a waiting approval. Any other
line is never taken as the answer because of where it falls: during a run it
steers the run, and with no run live (a background task's ask) it is sent to
the model as a prompt. Either way a note says the approval is still waiting.
Every answer prints which ask it answered (`accepted: bash touch made.txt
(subagent scan logs)`), so a key sent by position shows what it approved.

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
| `/compact [focus]` | compact the context now, keeping what focus names |
| `/context` | what fills the context window |
| `/memory` | the memory files in effect; `/memory add`, `/memory auto` |
| `/init` | have the agent write `ABHED.md` |
| `/commands` | your custom commands; `/commands trust` reviews the workspace's |
| `/tree` | the session's steps |
| `/fork <step>` | rebuild the conversation up to a step and continue from there |
| `/rewind` | take code and/or the conversation back to before a prompt, recorded |
| `/clear [name]` | end this session and start a new one; `/cost`, `/diff` and `/undo` start over, the workspace is kept |
| `/resume [id]` | continue a recorded session; with no id, pick one |
| `/rename <name>` | name the session, for `/resume` and `-r` |
| `/branch` | go on in a copy of this session, leaving it as it was |
| `/export [path]` | write the transcript, HTML in `~/.abhed/exports` by default |
| `/model [name]` | show or switch the model, keeping the conversation |

`@path` attaches a file, `!cmd` runs a shell command and `# note` saves a
note to memory; see [Input, memory and commands](18-input-and-memory.md).

Sessions are kept in a local record and survive the process: `abhed -c`
continues the last one in this workspace, and `abhed -r` picks one. See
[Sessions and the local record](12-records.md).

## Without a terminal

```bash
abhed -p "explain what pkg/auth does" -mode plan
abhed -p "fix the failing tests" -mode auto -allow 'bash(go test*)'
git diff | abhed -p "review this change"
abhed -p "add a test for Valid" -output-format stream-json > events.jsonl
```

`-p` runs one task and exits with a code a script can branch on. The output
formats, stdin, structured answers, limits and every exit code are in
[Automation](10-automation.md#headless).

A single word after the flags must be a command (`abhed -h` lists them,
`abhed version` prints the version). Anything else exits 2 with a hint
rather than opening a session: quote a task, put it after `--`, or pass it
with `-p`.

## Next

- [Configuration](02-configuration.md) — the settings that matter
- [Permissions](04-permissions.md) — before you run `-mode auto` on anything real
