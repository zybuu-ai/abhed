# The terminal

`abhed` with no `-p` is an interactive session. The input box sits at the
bottom of the screen and stays there; the conversation scrolls above it.
Without a terminal (piped input, a CI job), or on one that says it cannot
move the cursor (`TERM=dumb`, or no `TERM` outside Windows), the same session
reads lines and prints lines, without colour, and asks its questions as
numbered prompts answered with a line.

## Typing

| Keys | |
|---|---|
| Enter | send |
| Shift+Enter, Alt+Enter, Ctrl-J, `\` then Enter | new line |
| ← → , Ctrl-B, Ctrl-F | move a character (an accented letter, an emoji or a flag is one) |
| Alt-B, Alt-F, Ctrl-← , Ctrl-→ | move a word |
| Home, End, Ctrl-A, Ctrl-E | start and end of the line |
| ↑ ↓ , Ctrl-P, Ctrl-N | move between lines of a message, then through history |
| Ctrl-R | search history; Ctrl-R again for older, Enter to take, Esc to cancel |
| Ctrl-U, Ctrl-K, Ctrl-W, Alt-D | cut to the start, to the end, the word before, the word after |
| Ctrl-Y | paste back what was cut |
| Ctrl-_ | undo |
| Tab | complete a command or a path after `@`; after a paste placeholder, open it; on an empty line, take the suggested next prompt |
| → at the end of an empty line | take the suggested next prompt |
| Ctrl-G | edit the message in `$VISUAL` or `$EDITOR` |
| Ctrl-L | clear the screen; the session is kept |
| `?` on an empty line | these keys, briefly |

`/vim` turns vim-style editing on: Esc leaves insert mode, and `h l w b e 0 ^
$`, `x X D C`, `dd cc dw cw de`, `i a I A o O`, `u p P` and `j k` work as they
do in vim. `/vim` again turns it off. The choice is kept.

**Pastes.** A paste of more than three lines, or more than 800 characters,
shows as `[Pasted text #1 +25 lines]` and is sent whole, as one message, with
what you typed around it. Tab with the cursor after the placeholder opens it
for editing. A pasted placeholder is one character: Backspace removes all of
it. Pasted text is kept to what can be typed, so control characters in it
are dropped. A terminal that does not mark pastes sends them as fast
typing; that is recognised by its speed and treated the same way, except
that such a paste ending in a newline is sent at once, since nothing after
the last Enter tells it from one pressed by hand. Turn on bracketed paste
in the terminal to avoid that.

**A suggested next prompt.** When a turn completes, the input shows a dimmed
guess at what you may ask next, such as *Run the tests*. Tab, or → on the
empty line, puts it in the input to edit or send. It is the model's text, and
none is offered that urges past a safeguard or towards something destructive
([Configuration](02-configuration.md#suggestions)); it is never sent for you,
and Enter on an empty line still sends nothing. Typing anything dismisses it,
and the next turn replaces it. It comes from one small model call after the
turn has ended, so the prompt is back at once and the suggestion appears a
moment later; the record keeps it as a `model.call` with `purpose: suggestion`
and counts it in the session's tokens and budget. Typing or the next prompt
cancels it. None is made after an error or a
stop, while an approval waits, while you are typing, or when no input box is
drawn (piped input, `-p`). `suggest.enabled: false` turns it off
([Configuration](02-configuration.md#suggestions)).

**History** is kept per workspace, in `~/.abhed/history/`, readable only by
you (0600, never opened through a link). A vault secret in a prompt is
redacted before it is written, as it is in the session's record, and a large
paste is written as its placeholder, not its text. The folder is outside the
workspace: under a sandbox (the process tier and above) the agent cannot
reach it; with no sandbox, commands run as you and can.

## While the agent works

The line above the input shows what it is doing, how long it has taken and
how many tokens it has written: `⠹ Thinking… (12s · ↓ 1.2k tokens)`. It gives
way to the reply as soon as the reply starts.

| Keys | |
|---|---|
| Esc | stop the turn; the session is kept |
| Ctrl-C | on a typed line, clear it; otherwise stop the turn, and a second time exit |
| Ctrl-O | the whole transcript, with every tool's output and every reasoning block in full |
| Shift-Tab | the next permission mode, applied when the turn ends |

What you type while it works is shown as you type it. Enter sends it as a
steering message, applied at the agent's next step; until then it is listed
under the reply. A slash command waits until the turn ends.

The reply is formatted as it streams: headings, lists, emphasis, links,
quotes, tables and code. Code is highlighted for common languages, and a code
block is a block even when it arrives in pieces. Prose wraps between words at
the terminal's width.

Each tool call is one line, `● Edit(src/main.go)`, with paths relative to the
workspace. Under it: the first and last lines of its output with a count of
the rest, the exit code and output of a command that failed, and for an edit
or a write the diff, with line numbers and three lines of context. The diff is
shown whatever the mode, including accept-edits and auto, and stays in the
transcript. The model's reasoning is one line, `✻ Thought · 120 words`; Ctrl-O
or `/think` shows it.

## Subagents and background work

When the agent starts a subagent, in the foreground or the background, or a
background job runs, a list appears under the input:

```text
  ↑/↓ to select · Enter to view

❯ ● main                                                 1m 12s · 41k tokens in
  ● general   Review the parser change · read src/parse.go   48s · 12k tokens in
  └ ● explore Find every caller · thinking                   20s · 3.1k tokens in
  ✓ shell     go test ./...                                  31s
  ↓ 2 more
```

`main` is the conversation. Each other row is a subagent or a job, nested
under what started it: its type, its title, what it is doing now (the last
tool it called, or thinking), how long it has run and the input tokens its
model calls took. `●` is running, `✓` done, `✕` failed, `○` cancelled. A row
stays for two minutes after it ends; `/tasks` lists everything.

| Keys | |
|---|---|
| ↓ / ↑ | with nothing typed, select a row; ↑ from `main` is history, as are Ctrl-P and Ctrl-N always |
| Enter | on an empty line, open the selected row's record, read-only: its calls, their output and its messages |
| Esc | close the record; on the list, go back to `main` |

With a running subagent selected the input says `Message @<type>…`: what you
send goes to that subagent as your message, taken at its next step and
recorded in its own record. A row that cannot take messages (one that has
ended, or a job) says so, and what you send goes to `main`. Commands and `!`
lines always go to `main`. A subagent's approvals are asked in the usual
numbered dialog; nothing is approved for it.

When a background subagent or job ends, one line says so:

```text
● Agent "Review the parser change" finished · 5m 27s
● Background task "go test ./..." completed (exit code 0)
```

Its result reaches the conversation as well. While the session is idle the
agent acts on it in a short wake run, unless a configuration file sets
`subagents.wake` to `notify` or `off` (see
[Background tasks](14-parallel-subagents.md#background-tasks)).

`/tasks` (or `/bashes`) numbers the conversation's subagents and background
jobs; `/tasks view <n>` shows one's record and `/tasks kill <n>` stops a
background one. A foreground subagent ends with its turn: Esc stops it.

In the line mode there is no list: the lines that say a task ended, and
`/tasks`, work the same.

## Approvals

When a call needs you, the question takes the place of the input box: the
call, the command or the diff it would make, why it is asked and which step
of the policy decided, who asked if it was a skill's pipeline or a subagent,
and numbered answers.

- **1. Yes** runs it.
- **2. Yes, and don't ask again for …** allows the rule shown, for the rest
  of this session. It is offered only when the policy suggests a rule.
- **3. No, and tell Abhed what to do instead** refuses it and stops the turn.

**Approvals are answered by number only.** Nothing is selected when the
question appears, Enter never approves, and no letter does — not `y`, `a`,
`j`, `k` or any other, and no letter moves the selection. You answer with an
explicit 1, 2 or 3. ↑ ↓ move the highlight, but Enter on it only declines:
on No it answers No, on a Yes it answers nothing. There is no default answer
to fall back on. Esc means No. Ctrl-C means No at once.

In the line mode (piped input, `TERM=dumb`) the same question is printed
with its numbered answers, and only a line holding one of those numbers
answers it; anything else, an empty line included, asks again. The command
and the diff are shown with hidden characters marked, as in the dialog.
The line mode has no key-timing guard: it reads whole lines, so a number
followed by Enter while the question is open answers it, typed or pasted.
A line typed before the question appeared is your next message, not an
answer.

A key counts as an answer only when it is meant as one:

- no key counts until the question has been on screen for 300 ms;
- nothing is selected at first, so Enter alone answers nothing;
- a number counts only with 300 ms of quiet before and after it, so a number
  in text you were typing, or a key held down, is not an answer, and it
  leaves nothing selected;
- a paste is never an answer;
- Enter, even on No, needs 300 ms since the key before it.

A destructive command, such as `rm -rf`, never offers "don't ask again", and a
Yes is followed by a second numbered question — 1 No, 2 Yes, run it — whose
Enter answers No. The question, the
diff and your answer stay in the transcript, as they are in the record.

## The footer

Under the input, always:

```
  ⏵⏵ accept edits · shift+tab                                ? for shortcuts
  model-name · 23% context · 12k tokens · 2 background · ~/repo (main)
```

The first row is the permission mode. The second is the model, how full its
context window is, the session's tokens, background tasks, and the workspace
with its git branch. Items give way from the right at narrow widths.

**A status line of your own.** `statusline.command` in `~/.abhed/config.json`
replaces the second row with the first line a command prints. It gets the
status as JSON on stdin (`model`, `provider`, `mode`, `context_tokens`,
`context_percent`, `tokens_in`, `tokens_out`, `background_tasks`,
`sandbox_tier`, `git_branch`, `cwd` and more) and runs at most once a
second, under the process sandbox with the network off whatever the session
allows, and is stopped after 300 ms; only its first 4 KB are read. Where
that sandbox is missing, or its script is where the agent could change it,
it is not run, and the footer says why (see [the command
line](18-cli.md)). Its colours
are kept; anything else it prints to move the cursor or retitle the window is
dropped. A workspace's own `.abhed/config.json` may set it only once you have
trusted that file (see [workspace trust](../architecture/workspace-trust.md)).

```json
{ "statusline": { "command": "jq -r '\"\\(.model) · \\(.git_branch)\"'" } }
```

## Themes

`/theme dark`, `light`, `high-contrast` or `colorblind` switches the colours
and redraws the screen; `/theme auto` goes back to choosing. The choice is kept
in `~/.abhed/ui.json`. Without one, the theme comes from `ABHED_THEME`, then
`COLORFGBG`, then the terminal's own answer about its background colour.
`NO_COLOR` turns colour off whatever else is set. The colour-blind theme uses
blue and orange where the others use green and red.

## Resizing

The screen is drawn again at the new size: the input box, and the recent
conversation rewrapped for the new width. It works down to 40 columns.

## Coming from other agent CLIs

Most of the keys above will be familiar. What differs: an approval never
takes a key pressed as it appears, Enter alone never approves, and every
answer, with the diff it approved, is in the session's record.
