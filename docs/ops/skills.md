# Abhed — Skills

A skill is a folder with a `SKILL.md`: frontmatter naming it and saying when to
use it, then instructions. It captures procedural knowledge that does not belong
in code — how this team cuts a release, the checklist for a schema migration,
the house format for an incident report.

```
~/.abhed/skills/
  incident-report/
    SKILL.md
  go-release/
    SKILL.md
    scripts/check.sh
```

```markdown
---
name: incident-report
description: Write a postmortem in this team's format. Use when asked for a
  postmortem, incident report, or writeup of an outage.
---

# Incident report format

Write these sections, in this order, and nothing else:

**Impact** — who was affected, for how long, and how badly...
```

## How it works, and why

**Only the name and description reach the system prompt.** The body is fetched
by a `skill` tool call when the agent decides the skill applies.

That indirection is the whole design. Putting every body in the prompt would be
simpler and quietly ruinous: twenty skills of a thousand tokens each is 20,000
tokens paid on *every request of every session*, whether or not any of them is
relevant. Measured on two real skills, the listing is ~138 tokens against ~482
for the bodies; the gap widens linearly with every skill added.

So a description is not decoration — it is the only basis the model has for
choosing. Say *when* to use the skill, not what it contains:

- Good: "Use when asked for a postmortem, incident report, or writeup of an outage."
- Bad: "Incident reporting utilities."

A skill without a description is rejected at load rather than silently ignored.

## Configuring

Defaults to `~/.abhed/skills`. To use other directories:

```json
{
  "skills": {
    "dirs": ["/srv/abhed/skills", "~/.abhed/skills"]
  }
}
```

Later directories win on a name collision, so a project can deliberately
override a team-wide skill. `{"skills": {"disabled": true}}` turns them off.

## Importing skills written elsewhere

Skills authored for another harness generally load unchanged. Two conventions
are handled explicitly:

**`$SKILL_DIR`.** Many skills say `bash $SKILL_DIR/scripts/run.sh`. The model
has no shell to expand that, so the tool reports the directory *and* states the
substitution — otherwise the command runs `/scripts/run.sh` and fails.

**Folded descriptions.** YAML block scalars work:

```yaml
description: >
  Answer questions from the enterprise document corpus using the
  knowledge base. Use whenever the user asks a documented question.
```

Without this, `description: >` parsed as the single character `>` — a
description the model could never match a request against, so the skill was
silently never invoked.

**Skill directories are readable.** Each loaded skill's own directory is granted
to every session, because its instructions reference the scripts and assets
shipped beside it. Denying that read sent the agent into a loop it could not
escape.

## Choosing which skills are active

To enable a subset without moving anything, point `dirs` at a directory of
symlinks:

```bash
mkdir -p ~/.abhed/skills-active
ln -s /path/to/skills/corpus-search ~/.abhed/skills-active/corpus-search
```

```json
{ "skills": { "dirs": ["~/.abhed/skills-active"] } }
```

Symlinks are followed, so the skill stays where it lives and nothing drifts out
of sync with its source.

A malformed `SKILL.md` is reported by name and skipped; the others still load.
A stub alongside seven working skills used to cost all eight.

## Skills from the workspace

A repository's own skills in `.abhed/skills/<name>/` load only after the
person trusts their exact content, as with its agent definitions.

A skill body is instructions by construction — that is what makes it useful,
and what makes it the most direct route for untrusted text to reach the model.
Loading them from any cloned repository unasked would let it carry its own
orders to the agent reading it. So the trust prompt and `abhed trust` show
each skill's name, description and a hash over every file in its folder; a
change to any of them, a script included, asks again; an untrusted workspace
loads none; and a skill named like one the organisation or the person
installed is never loaded in its place. To keep skills to the installed ones,
set `{"skills": {"disable_workspace": true}}` in the managed configuration.
See [Workspace trust](../architecture/workspace-trust.md#workspace-skills).

For instructions that should travel with a repository but need no trust,
use `ABHED.md`: it is project memory, read as untrusted text, and does not
get a tool that loads it as authoritative procedure.

## Referencing files

A skill's directory is reported when its body is loaded, so instructions can
point at files beside them:

```markdown
Run the verification script in `scripts/check.sh` from the skill directory.
```

## Verifying

`abhed doctor` lists what loaded:

```
skills      2 loaded: go-release, incident-report
```

A malformed `SKILL.md` is reported by name and skipped — one bad file does not
stop the agent starting.
