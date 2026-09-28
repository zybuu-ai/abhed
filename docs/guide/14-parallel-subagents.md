# Parallel subagents and worktree isolation

The `task` tool runs one subagent and waits. The `tasks` tool runs several at
once. The agent chooses it when subtasks are independent — investigate three
modules, try two fixes, review each service — and the model's own prompt
decides that, not a flag.

```json
{
  "tasks": [
    {"prompt": "…", "description": "audit pkg/auth", "agent_type": "review"},
    {"prompt": "…", "description": "audit pkg/store", "agent_type": "review"}
  ],
  "isolation": "worktree"
}
```

Up to eight per call. Concurrency is bounded by `limits.max_parallel_subagents`
(zero means all at once), and each subagent still counts against
`limits.max_subagents` and the token budget exactly as a single `task` does.

## Isolation

The interesting case is when subagents **write**. Two agents editing one
checkout overwrite each other, and the parent cannot tell whose change
survived. With `"isolation": "worktree"` each subagent gets:

- its own **git worktree** under `.abhed-worktrees/<id>` — a separate checkout
  of `HEAD`, so its files are its own;
- its own **branch**, `abhed/<id>`;
- its own **scoping boundary** for files: the child's file tools are rooted in
  the worktree and cannot reach the parent's tree or a sibling's. Its commands
  start in the worktree and run in the parent's sandbox, so, as there, they
  are confined to the workspace rather than to the worktree.

The worktrees live beside `.abhed/`, not in it: `.abhed/` is Abhed's own
state, which the agent can neither read nor write, so a worktree there could
not be worked in.

Nothing touches the main tree. When the tasks return, the parent receives each
subagent's summary and, for each worktree, what changed and how to take it:

```
## Task 1 — audit pkg/auth
<summary>

Worktree .abhed-worktrees/k3f9q2 (branch abhed/k3f9q2): 3 file(s) changed, UNCOMMITTED.
 auth/local.go | 12 ++++---
 …
To take these changes: review with `git -C .abhed-worktrees/k3f9q2 diff`, commit
there, then `git merge abhed/k3f9q2` from the main tree. To discard:
`git worktree remove --force .abhed-worktrees/k3f9q2 && git branch -D abhed/k3f9q2`.
```

Changes are left **uncommitted in the worktree**, on purpose. Merging is a
decision, and the point of isolation is that a person — or the parent agent,
with the diff in front of it — makes that decision rather than discovering it.

Worktrees are created up front and serially before any subagent starts, so a
git failure stops the whole call before a token is spent. The directory is
added to `.git/info/exclude` so the main tree's `git status` stays clean
without touching the repository's tracked `.gitignore`, and `glob`, `grep` and
the code index pass over it. A link planted at `.abhed-worktrees`, or at a
worktree's own path, is refused rather than followed.

## Requirements and limits

- Isolation needs the workspace to be a git repository, and `git` on the
  path. Asking for it elsewhere is refused with that reason, before spawning.
- A worktree the subagent left exactly as it was made — no change, no commit,
  no untracked or ignored file — is removed, with its branch. Any other is
  kept, as is one whose state cannot be read; they are small (a checkout,
  shared object store) and the parent's report says how to take or remove
  each one.
- Without isolation, subagents share the parent's workspace and boundary —
  fine for read-only work, and the default, because most parallel work is
  investigation.

## Approvals and the record

A subagent is policed as its parent is. The same policy decides each call,
and anything it routes to a person goes to the parent's approver:

- in the interactive CLI, your prompt, which names the subagent asking. Asks
  from subagents running together come one at a time, and one whose turn is
  interrupted while it waits is not asked;
- in `-p`, nobody can be asked, so they are refused as `headless`, as the
  parent's own asks would be.

Subagents are offered in the CLI only; the server, `abhed rpc`, `abhed acp`
and the SDK do not have `task` or `tasks`.

A subagent runs inside its parent's session, so an "Always allow" chosen
earlier in the session covers its calls, and one chosen at a subagent's
prompt lasts for the rest of the session, as it would for the parent.

Each subagent keeps its own record, whose events carry the parent's session
as `parent_id`. The parent's record holds `subagent.spawned` and
`subagent.returned` naming that `session`, and a `subagent.action` for every
call of the subagent's that was refused or put to an approver. Calls the
policy allowed on its own are in the subagent's record only. HawkEYE's
report on the parent lists each subagent's session, a `subagent-denied`
finding for each refused call and a `subagent-destructive` warning for each
destructive command that was allowed.

A subagent cannot start one of its own unless `limits.nested_subagents` is
on. When it is, the nested subagent's `subagent.*` events are passed up, so
the top-level record holds every subagent at every depth.

## What it is not

It is not a multi-agent framework with roles and message passing. Abhed's
model is simpler and stays simple: a subagent is a fresh loop with a complete
prompt, it returns a summary, and the parent decides what to do with it.
Running several at once, in separate checkouts, is the whole of what this
adds.
