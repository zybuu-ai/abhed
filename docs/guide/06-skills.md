# Skills

A skill is a procedure Abhed should follow when a request matches it: how your
team cuts a release, the checklist for a schema migration, the house format for
an incident report. It is knowledge that does not belong in code.

## Format

A folder with a `SKILL.md`:

```markdown
---
name: go-release
description: Cut a tagged release — version bump, changelog, tag, verification.
  Use when asked to release, cut a version, or ship a build.
---

# Cutting a release

1. **Confirm the tree is clean.** `git status --porcelain`. Never tag a dirty tree.
2. **Run the tests.** A release with failing tests is not a release. Report the
   actual output.
3. **Determine the version** from the last tag, per semver.
4. **Tag and verify** with `git describe --tags`.
```

This is the Agent Skills format. A skill written for another harness that uses
it will load here unchanged.

```json
"skills": { "dirs": ["~/.abhed/skills", ".abhed/skills"] }
```

## Why only the description is in the prompt

Abhed puts each skill's **name and description** in the system prompt, about
fifteen tokens each, and loads the body only when the agent calls the `skill`
tool. Twenty skills of a thousand tokens each would otherwise cost 20,000 tokens
on every request of every session, whether or not any of them was relevant.

The consequence is that **the description is the whole interface**. It decides
whether a skill is ever used. Say when to use it, not what it is.

## Where skills come from

Only directories the operator configured — never the workspace the agent is
editing. A skill body *is* instructions by construction, so a repository that
could carry its own skills could carry its own instructions to the agent reading
it.

## Writing one that works

A skill body is followed by the model, not executed by the harness, and a
smaller model follows a long conditional procedure unreliably. If a step must always
happen, that is a job for an [extension](07-extensions.md) or a
[tool](05-tools.md), not a paragraph.

What works: numbered steps, one action each; explicit stop conditions; naming
what to report rather than assuming. What does not: several pages of prose, or a
rule stated once in the middle and expected to hold.

## Pipelines

A skill directory may also hold a `pipeline.json`, whose stages the harness
runs itself in the CLI (`abhed`, `abhed -p`); the server, console and SDK do
not run pipelines. Each tool step is an ordinary tool call of the loop that
called the skill: deny and ask rules, the mode, approvals and the record all
apply, and the record names the pipeline in `via`. With no one to approve
(`-p`), a step that would be asked about is refused, so give it an allow rule.
A pipeline is refused when it would call the `skill` tool or start beneath
another pipeline's step, including from a subagent a step started; that
skill then falls back to its instructions.
