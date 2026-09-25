# Permissions and safety

An agent that can run shell commands in your repository needs a real answer to
"what is it allowed to do". Abhed's answer is a policy engine that decides every
tool call, in a fixed order, with one rule that nothing can override.

## Modes

```json
"permissions": { "mode": "default" }
```

| Mode | Behaviour |
|---|---|
| `default` | ask before every mutation |
| `plan` | **read-only** — nothing is written, safe for exploring an unfamiliar repository |
| `accept-edits` | auto-approve file edits, still ask for shell |
| `auto` | approve by rule; anything unmatched still asks |
| `bypass` | approve everything an org policy has not forbidden. Dangerous, and refusable by managed settings |

In headless mode there is no one to ask, so anything needing approval is
refused. Use `-mode auto` with explicit `-allow` rules.

## Rules

A rule is a tool name, optionally followed by a pattern:

```json
"permissions": {
  "mode": "auto",
  "allow": ["bash(go test*)", "bash(npm run *)", "read"],
  "deny":  ["bash(curl *)", "write(*.pem)"],
  "ask":   ["bash(git push*)"]
}
```

`*` matches anything, newlines included; the pattern is matched against the
command or path.

For `bash`, an allow rule with a pattern approves only a single simple command.
A command with `;`, `&`, `|`, a newline, `$(`, `${`, a backtick, `<`, `>`, `(`
or `)` anywhere in it, even inside quotes, falls through to a prompt, and no
"always allow" scope is offered for it. `bash` on its own and `bash(*)` still
allow every command. An allow rule whose own pattern holds that syntax, such as
`bash(cd x && go test*)`, can never match, and a warning names it at startup.

Deny and ask rules match the whole command or any command inside it: split on
those operators, taken out of substitutions and subshells, and past leading
`VAR=value` assignments, redirections and wrappers such as `sudo`, `env`,
`nice`, `nohup`, `timeout`, `xargs`, `exec` and `command`. The split does not
parse the shell's quoting, so it can only add a denial or a prompt; the
sandbox, not the pattern, is the boundary. A command too long or complex to
split in full (over 64 KiB, over 1,024 parts, or a wrapper with too many
readings) is always asked about while any deny or ask rule for `bash` has a
pattern, in every mode.

A malformed rule is **refused at startup** rather than silently matching
nothing — for a deny rule, quietly accepting one that can never fire tells you
that you are protected when you are not.

## The order

Every call goes through the same six steps, and the order is the design:

1. **Hooks** — extensions, first, so they can veto
2. **Deny rules** — absolute for every tool call, the agent's and a person's; they survive every mode, including `bypass`. In the workbench's interactive shell, which the sandbox bounds, they screen each line as typed, best effort ([the workbench](16-workbench.md))
3. **Destructive commands** — force push, hard reset, disk writes, fork bombs and similar always confirm, in every mode, because there is no undo
4. **Ask rules** — force a prompt even where a later allow would match
5. **Mode**
6. **Allow rules**, then a default: read-only proceeds, mutations ask

Two consequences worth stating plainly. **A deny rule cannot be overridden** by
a mode, an allow rule, an extension, or an operator's own bypass. And **an
extension may veto but never permit**: it can block a call or force it to a
prompt, and cannot turn a denied action into an allowed one. A permission gate
an extension could remove would not be a guarantee.

## What the agent may touch

Writes are scoped to the workspace it was started in. `additional_dirs` extends
that, and is set by the operator — never by the model.

`.abhed/` — the configuration, the users file and the keys, in the workspace
and in the home directory — is out of the agent's reach in every mode. The file
tools refuse it and the sandbox hides it from commands, so no prompt can talk
the agent into dropping a deny rule or adding a user for the next start. It is
a boundary rather than a rule, because a rule lives in the file it would be
protecting. The operator edits that file by hand; the one exception is
`~/.abhed/skills`, which commands may read, since a skill can ship a script.

Content read from files, tool output and search results is **data, never
instruction**. Every event carries a trust tag, and untrusted content is marked
as such in the transcript and the console. A file that contains text shaped like
instructions is reported, not obeyed.

## When it asks

The prompt names the tool, the full arguments, and the rule that would have
allowed it:

```
Approval required — bash
  go test ./pkg/auth/
  It would be permitted by the rule bash(go test*), which is not configured.
```

Rejecting feeds the reason back so the model adapts rather than rephrasing the
same command. A denial that says only "no" makes a model retry forever.

## Recovering

`/undo` reverts the last turn's file changes. `/diff` shows what changed this
session. With Postgres storage, `/resume` replays a past session exactly, which
is how you find out what an agent did rather than what it said it did.

## The monitor

Between the deny rules and the ask rules sits an optional judge: given the
session's remit, the agent's most recent stated reasoning, the proposed call
and where every host and path in it was first seen, it returns allow, ask or
deny with a reason. It runs on calls policy would ask about, on any mutation
a mode waved through, and on any call naming a host or path the user never
mentioned; a read the user asked for, allowed by rule, never reaches it.

Two rules make it safe to let it read the reasoning:

- **A verdict can only tighten.** An allow may become an ask or a deny, an
  ask may become a deny, and nothing moves the other way. A judge that could
  be argued into permitting would have to be kept away from the text that
  argues; here the worst a persuaded judge can do is nothing.
- **Absence is not consent.** A judge that is unreachable, slow or
  incoherent raises the call to ask — or to deny in a headless run, where
  nobody can answer.

Every consultation is in the record as `monitor.verdict`, with the decision
before and after, the code, the rationale and the judge's version, and a
denial it caused says so at step `monitor`. The judge is any implementation
of `monitor.Monitor`; the local-model judge and its configuration follow in
the next release.

## Secrets

A command that runs tests, calls an API or opens a pull request needs a
credential. It does not get one from your environment: the agent's commands
see only what the sandbox passes through. It gets one **by name**, from a store
the operator fills, and only under a rule that names it.

```
abhed secret set GITHUB_TOKEN      # value prompted without echo, or piped on stdin
abhed secret list
abhed secret rm GITHUB_TOKEN
```

Values live in `~/.abhed/secrets.json` (`ABHED_SECRETS_FILE` overrides), mode
600, outside every workspace and under the directory the agent's tools and
sandbox cannot reach. They are never in a config file.

The model sees the **names**, and asks for one on a single command:

```json
{"command": "gh pr create --fill", "secrets": ["GITHUB_TOKEN"]}
```

That command, and only that command, runs with `GITHUB_TOKEN` in its
environment. Whether it may is decided by a rule:

```json
"allow": ["secret(GITHUB_TOKEN)"],
"deny":  ["secret(PROD_*)"]
```

A secret needs its own allow rule **in every mode**. `auto` and `bypass`
approve calls; they do not hand out credentials. A command that asks for a
name with no rule is refused, and the model is told which rule would permit
it. A deny rule wins over an allow rule, as everywhere else.

**Redaction.** The record is append-only, so a value that reached it could
never be taken out. Every event is checked before it is written: a stored
value, wherever it appears, becomes `[secret:NAME]`. Redaction matches the
stored values exactly; it is not a pattern guessing at what a key looks like.
When it fires, [HawkEYE](15-hawkeye.md) reports `secret-redacted`: the value
was caught, and the command or the model exposed it, which is worth knowing.

The reply and the reasoning stream as many fragments, and a value may be split
between two, so streamed text is held back by about the length of the longest stored value
and redacted together with what follows. The live view lags by that much, and
only when secrets are stored; the whole text is redacted as one event too.
A stored key file makes that lag visible, a few kilobytes of text, and values
that occur close together hold the text back until the last one is complete.
Text that cannot be redacted is never written as it was: it becomes
`[redacted: output withheld]`.

