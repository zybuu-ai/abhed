# Agent definitions

The `task` and `tasks` tools start subagents in one of four built-in roles:
`general`, `explore`, `test` and `review`. An agent definition adds a role of
your own: a markdown file whose frontmatter names the role, says when to use
it and what it may use, and whose body is the role's instructions.

```markdown
---
name: migration-reviewer
description: Reviews a database migration for locking, data loss and rollback
tools: read, grep, glob
model: fast
max_turns: 15
permission_mode: plan
---
Review the migration you are given. For each statement, say which locks it
takes and for how long on a large table, whether it can lose data, and how it
is rolled back. Report file:line for every finding. Do not edit anything.
```

The model sees each role's name and description on the `task` tool, and
`agent_type` offers exactly the roles this session has. A subagent started as
`migration-reviewer` gets the instructions above as its role, the three tools
named and nothing else, and runs in plan mode.

## The keys

| Key | Also written | Meaning |
|---|---|---|
| `name` | | the `agent_type` value: lower-case letters, digits and dashes, starting with a letter, at most 40. Defaults to the file name without `.md` |
| `description` | | when to use the role; shown to the model. Required, at most 300 characters |
| (body) | | the role's instructions. Required, at most 16 KiB |
| `tools` | a comma string, `[a, b]`, or `- a` lines | the tools the role may use. Omitted: every tool the session has |
| `disallowed_tools` | `disallowedTools` | tools removed after `tools` |
| `model` | | a configured provider name, or `inherit` (the default) — see below |
| `max_turns` | `maxTurns` | the role's turn cap, 1 to 100 |
| `isolation` | | `worktree`: the role always works in its own git worktree |
| `permission_mode` | `permissionMode` | `plan` or `default`, applied only where it narrows the session's mode |

Tool names are matched without regard to case, so `Read` is `read`, and
without regard to underscores and dashes where that is unambiguous, so
`WebSearch` is `web_search`. `mcp__<server>__*` names every tool of one MCP
server; it is the only wildcard.

These are the keys of the markdown agent-definition format in common use, so
most existing files load unchanged. Abhed reads only the directories below; it
never looks in another tool's directory on its own. To use such a directory,
list it in `agents.dirs`.

## Everything only narrows

A definition can make a subagent do less than its parent, never more:

- **Tools.** A role's tools are cut from the parent session's tools at the
  moment it is started. A name in `tools` that the session does not have
  refuses the start, naming the tool: "definition migration-reviewer names
  tools this session does not have: bash". It is not dropped in silence, so a
  typo cannot give a role less than its author meant, and a list cannot give it
  more than its parent has. `task` and `tasks` in a list stay subject to
  `limits.nested_subagents`.
- **Permission mode.** `plan` or `default` applies only when it is narrower
  than the session's mode. A definition asking for anything wider
  (`acceptEdits`, `bypassPermissions`, ...) is refused when it loads.
- **Policy.** The subagent is judged by the parent's rules: deny, ask,
  managed policy and extension vetoes all come along. A definition has no
  rules and no approver of its own; a subagent's asks go to whoever the
  parent's go to.
- **Turns.** `max_turns` caps the role, and the `task` call's own
  `max_turns` may lower it but not raise it. Nothing goes above the session's
  `limits.max_turns`. Without either, a subagent has 30 turns.
- **Isolation.** A `worktree` role works in its own checkout whether or not the
  call asked for isolation, so starting one asks as worktree isolation does,
  and plan mode refuses it.
- **Budget.** Every subagent draws on the session's one token budget and
  counts against `limits.max_subagents`.

A key Abhed does not honour and that would concern authority refuses the whole
definition, with a warning: `hooks`, `mcpServers`, `permissions`, any key
naming `allow` or `deny` other than `disallowed_tools`, sandbox or network
settings, and any other key that reads like one of the honoured ones (naming a
tool, a mode, a model, turns or permission, such as `denied_tools`). A key is
the same key quoted or not, and one nested under another key is checked too:
`settings:` holding `disallowedTools` refuses the definition rather than
passing unread. An author who wrote one expected a restriction, and must not get a
looser agent without being told. Cosmetic keys such as `color` are ignored
with a warning, and so is any other key Abhed does not know.

A definition's tool list is a list of capabilities, not a risk class. A tool
that only reads, such as a web fetch, can still carry data out, and it is
policed as it would be for the parent.

## Another model for a role

`model` names a provider from your configuration's `model.providers`, the
same names `abhed -model` and the console's model picker take. The `task` and
`tasks` calls take a `model` too, and it wins over the definition's; `inherit`,
or no value, is the definition's model and then the parent's.

- **A name, never an endpoint.** A value holding `://`, a `/` or a space is
  refused before anything is looked up. The key comes from the environment of
  the process running Abhed, as the session's own does.
- **Only what is offered.** On a server, only a provider sessions may run on
  (the console's list); a built-in provider the configuration never named is
  not one. An untrusted workspace file cannot add a provider, so it cannot add
  a model for a role either.
- **No fallback.** A model that is not configured, has no key, or cannot be
  built refuses the start: "model fast is not available: ...; available: a,
  b". Nothing runs, no spawn is counted, and the parent's model is never used
  in its place. A definition naming a provider that is not configured is
  refused when it loads.
- **One budget.** Every model's tokens count against the session's one
  `max_budget_tokens`. There is no weighting by price.

The model property appears on the tools only when more than one provider is
offered, so a single-model deployment pays nothing for it in the prompt.

## Where definitions come from

Highest first:

1. **Managed:** `/etc/abhed/agents/*.md`. Always loaded, and no other file can
   take one of these names.
2. **The workspace:** `.abhed/agents/*.md`, only when you trust that exact
   content (below).
3. **Yours:** `agents.dirs`, by default `~/.abhed/agents`. With several
   directories, a later one wins a name.

When two files define one name, the higher one wins and a warning names the
file it shadowed. The built-in names `main`, `general`, `explore`, `test` and
`review` are reserved: a file using one is refused, because a repository that
redefined `explore` to include a shell would change what the model believes it
is asking for.

A definition must be a regular file: a link, or a file with a second hard
link, is refused. `agents.disabled: true` loads the managed definitions only.

## Definitions in a repository need your trust

A definition in `.abhed/agents` came with the repository. It can choose a
model, and so where your code is sent; it sets turn caps and tool lists. So it
loads only once you trust the workspace for exactly those files, as a
workspace's `config.json` does
([Workspace trust](../architecture/workspace-trust.md)):

- The interactive CLI asks, showing each definition's name, model and tools.
  Declining keeps a `config.json` you already trusted.
- `abhed trust` shows them; `abhed trust grant` trusts them, and
  `-agents-sha256` pins the grant to what you reviewed.
- Headless runs use your stored decision, or `-trust-workspace` for one run.
- Editing, adding or removing a definition makes them untrusted again.

Until then they are not offered, and the warning names each one as
`agents/<name>`.

## When they are read

The CLI, `acp`, `rpc` and the SDK (with `Options.ConfiguredTools`) read the
definitions when a session starts. The server reads them at start, and an
administrator reloads them with:

```
POST /v1/admin/agents/reload
```

The answer lists the definitions loaded and a warning for each file that was
not. A reload reaches sessions started after it. A running session keeps the
roles it started with, since they are part of its prompt and of what its
record says it was offered. `abhed eval` offers the built-in roles only, so a
score does not depend on local files.

## In the record

`subagent.spawned` names the role and what it ran with: `definition`,
`definition_source` (`builtin`, `managed`, `workspace` or `operator`),
`definition_sha256` for a loaded file, `tools`, the tools the subagent had,
sorted, and `model` and `provider`, the model it ran on and the configured
provider it came from. `subagent.returned` names `model` and `provider` too,
and each of the subagent's `model.call` events names its model.
