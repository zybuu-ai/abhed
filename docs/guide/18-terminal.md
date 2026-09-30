# The terminal

`abhed` with no `-p` is an interactive session. The input box sits at the
bottom of the screen and stays there; the conversation scrolls above it.
Without a terminal (piped input, a CI job) the same session reads lines and
prints lines, and asks its questions as numbered prompts answered with a line.

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
| Tab | complete a command or a path after `@`; after a paste placeholder, open it |
| Ctrl-G | edit the message in `$VISUAL` or `$EDITOR` |
| Ctrl-L | clear the screen; the session is kept |
| `?` on an empty line | these keys, briefly |

`/vim` turns vim-style editing on: Esc leaves insert mode, and `h l w b e 0 ^
$`, `x X D C`, `dd cc dw cw de`, `i a I A o O`, `u p P` and `j k` work as they
do in vim. `/vim` again turns it off. The choice is kept.

**Pastes.** A paste of more than three lines, or more than 800 characters,
shows as `[Pasted text #1 +25 lines]` and is sent whole, as one message, with
what you typed around it. Tab with the cursor after the placeholder opens it
for editing. A terminal that does not mark pastes sends them as fast typing;
that is recognised by its speed, and treated the same way.

**History** is kept per workspace, in `~/.abhed/history/`, readable only by
you. It is outside the workspace, so the agent cannot write to it.

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

## Approvals

When a call needs you, the question takes the place of the input box: the
call, the command or the diff it would make, why it is asked and which step
of the policy decided, who asked if it was a skill's pipeline or a subagent,
and numbered answers.

- **1. Yes** runs it.
- **2. Yes, and don't ask again for …** allows the rule shown, for the rest
  of this session. It is offered only when the policy suggests a rule.
- **3. No, and tell Abhed what to do instead** refuses it and stops the turn.

Press the number, or move with ↑ ↓ and press Enter. Esc means No. Ctrl-C means
No at once.

A key counts as an answer only when it is meant as one:

- no key counts until the question has been on screen for 300 ms;
- nothing is selected at first, so Enter alone answers nothing;
- a number counts only with 300 ms of quiet before and after it, so a number
  in text you were typing, or a key held down, is not an answer;
- Enter after an arrow needs 300 ms since the arrow.

A destructive command, such as `rm -rf`, never offers "don't ask again", and a
Yes is followed by a second question whose Enter answers No. The question, the
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
`sandbox_tier`, `git_branch`, `cwd` and more), runs under the session's
sandbox, at most once a second, and is stopped after 2 seconds. Its colours
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
