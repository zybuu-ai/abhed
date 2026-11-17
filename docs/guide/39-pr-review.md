# Pull request review

The [GitHub Action](38-github-action.md) in `mode: review` reviews a pull
request's diff and posts one formal review: an event, a summary, and an
inline comment on each finding that names a file and a line in the diff. It
never approves, never pushes, and runs no code from the pull request.

```yaml
name: Abhed review
on:
  pull_request:
    types: [opened, synchronize, reopened, ready_for_review]
  workflow_dispatch:
    inputs:
      pr: {description: The pull request to review (for one from a fork), required: true}

permissions:
  contents: read
  pull-requests: write

jobs:
  review:
    if: ${{ github.event_name == 'workflow_dispatch' || !github.event.pull_request.draft }}
    runs-on: ubuntu-24.04
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4.4.0
        with:
          ref: ${{ github.event_name == 'workflow_dispatch' && format('refs/pull/{0}/head', inputs.pr) || '' }}
          fetch-depth: 0
          persist-credentials: false
      - uses: zybuu-ai/abhed/integrations/github-action@<full commit>
        with:
          mode: review
          pr-number: ${{ inputs.pr }}
          model-provider: anthropic
          model: claude-opus-5
          model-api-key: ${{ secrets.ABHED_MODEL_KEY }}
          max-turns: "30"
```

The full workflow is `integrations/github-action/examples/abhed-review.yml`.
`fetch-depth: 0` is needed: the diff is taken between the pull request's base
and head commits, and both must be in the checkout.

## What it does

1. **Finds the pull request.** On `pull_request`, from the event; on
   `workflow_dispatch`, from `pr-number`, looked up with the job's token.
   Its base and head must be commit ids. On `workflow_dispatch` the
   checkout must be at that head: if the pull request moved after it was
   checked out, the review stops rather than post on a commit it never read.
2. **Takes the diff** with `git diff base...head`, with hooks off, no
   external diff and no textconv, so nothing the repository configures runs.
   A diff over 400 KB is cut, and the agent told so.
3. **Reviews it read-only.** `abhed -p` runs in `plan` mode with the tools
   `read`, `glob` and `grep` only (`-tools`), without the repository's memory
   files, extensions or skills (`-bare`), and with the workspace's
   `.abhed/config.json` untrusted, so it can only tighten. Its model is the
   action's own inputs (`model-provider` and `model` are required), and
   from a policy (`policy-file`, read with git from the pull request's base
   commit, never its head, a symbolic link refused; or the `policy` input)
   it takes only `permissions.deny`, `permissions.ask` and `limits`.
   Everything else a policy sets (hooks, MCP servers, extensions, skills,
   agents, monitors, telemetry, extra folders, other providers, the
   sandbox, the mode) is set aside, and the log names what was. The diff is the
   run's input; the answer must match a JSON schema of a summary and
   findings (path, line, severity, title, body). Answers are labelled
   (`verify: label`), and the label goes in the review.
4. **Posts one review** in a separate step, the only one given the token
   (`github-token`, the job's `GITHUB_TOKEN` by default):
   - The event is `COMMENT`. With `request-changes-on: major` (or `critical`,
     `minor`, `nit`), a finding at that severity or above makes it
     `REQUEST_CHANGES`. There is no setting that approves.
   - A finding whose line is in the diff becomes an inline comment on that
     line of the new version; the others are listed in the review's body
     with their file and line. At most 50 inline comments.
   - Text from the model is made safe to post, in every field and path.
     Outside code, every `&`, `<` and `>` is escaped, so no HTML renders, no
     comment or tag among them however it is nested, and so are backticks
     and backslashes, so nothing there can pair with or escape a code
     delimiter. Code is kept readable: each fenced block and inline code
     span is rebuilt with delimiters of the action's own, longer than any
     run of backticks inside, so its content is shown as written and cannot
     close it early; a fence left open is closed. A path is one code span, or
     `(no file)` when it names none. A finding listed in the body has its
     text as blocks of its own inside the list item, so a fence in it starts
     a line, and findings that would pass GitHub's size limit are left out
     whole, never cut, so the closing note is always there. CI parses
     thousands of generated reviews with commonmark.js to hold this.
     Each `@`, in code or not, is followed by a zero-width joiner, so no
     mention notifies anyone; and every field is bounded. Markdown
     itself is kept, so a finding can still be laid out to mislead (a
     link's text and target differ, say): read it as the model's text.
   - If GitHub refuses an inline line, the review is posted again with the
     findings in its body.
5. **Uploads the evidence**: the run's output, its exported record and the
   transcript. The diff, task and schema stay in the runner's temp folder.

`review-instructions` adds your maintainers' own points to the review task.
`post-review: false` only writes the review to the evidence.

### On a release before 1.3.0

`-tools` and `-bare` are new in 1.3.0, and the action pins v1.2.9 until
1.3.0 is published. On an older release the review still runs: `plan` mode,
with deny rules for every built-in tool of 1.2.9 but `read`, `glob` and
`grep` (the answer's own tool excepted; `toolchain`, `todo`, `search`,
`recall` and `task_status` included), and a warning in the log. Two things
differ: a deny rule names the tools that exist, where `-tools` names the
only ones allowed, and the repository's `ABHED.md` reaches the model,
another place for the pull request's text to try to steer it. Set
`abhed-version` and `abhed-sha256` to a 1.3.0 release to close both.

## Pull requests from forks

On `pull_request`, GitHub gives a pull request from a fork no secrets and a
read-only token. The review then has no model key: it is skipped with a
notice, and the job passes. A maintainer who has looked at the pull request
reviews it with **Run workflow** and its number (`workflow_dispatch`). That
run has the secrets and a token that can post, checks out the fork's head,
and reads it exactly as above: nothing from the pull request is built, run
or trusted.

A model that needs no key (a local `ollama` or `openai-compatible` server
on a self-hosted runner) is not skipped. Give such a runner to
`pull_request` jobs from forks only if you accept running the review for
any pull request; it still runs nothing from it.

Never use `pull_request_target` for this. It runs with the base repository's
secrets and a write token on an event a fork controls, and checking out the
fork's code there is the classic way to hand both to it. The action refuses
that event unless `allow-pull-request-target: true`.

## Threat model

What is defended, against whom, and what is not.

**Assets.** The model key; the job's GitHub token; the repository's
contents and its other secrets; the integrity of the review itself.

**The adversary** writes the pull request: its code, its comments, its file
names, its `.abhed/` folder, its `ABHED.md`, and its commit messages, and may
open it from a fork. Everything the agent reads is theirs.

| Threat | Defence |
|---|---|
| The pull request's code runs on the runner (a test, a build script, a git hook, a textconv driver) | Nothing is built or tested. The diff is taken with hooks, external diff and textconv off. The agent has `read`, `glob` and `grep` only, in `plan` mode: no `bash`, no writes, no network tools. |
| The pull request's configuration loosens the run, or points the model key elsewhere | The workspace's `.abhed/config.json` is untrusted: only what tightens applies. `-bare` drops its memory files, extensions and skills. A `policy-file` is read with git from the base commit, never from the head or the disk, and one committed as a symbolic link is refused. A review takes only deny and ask rules and limits from any policy, and its model only from the action's inputs, so no setting can bind the key's variable to another host. |
| Prompt injection: text in the diff tells the agent to approve, to post something, or to leak a secret | The review's event is chosen by the action from the findings' severities, never by the model, and is never `APPROVE`. The model's text is posted only as review text, with every `@` defused and every `&`, `<` and `>` outside code escaped. The agent holds no token and cannot reach the network, so it has nothing to leak and no way to send it. Injection can still make a review wrong or noisy: it is advice, and the job never merges anything. |
| The job's token is used by the agent | The token is in the posting step's environment only. The agent runs under `env -i`, which drops every GitHub credential, and the action refuses a checkout that kept the token in `.git/config` (`persist-credentials: false`). |
| The model key is taken | It is a secret, masked in logs, passed in one variable to Abhed only, never on a command line or in a file. A fork's `pull_request` run never has it. The agent has no command tool to read its environment. |
| The workflow is changed by the pull request | On `pull_request` the workflow file comes from the pull request, so a pull request can change its own run, but that run has no secrets (fork) or comes from someone with write access already (same repository). `permissions:` limits the token to `contents: read` and `pull-requests: write`. |
| A tampered Abhed binary | The release archive is checked against the SHA-256 the action pins, and by default against the release's Sigstore signature, before it runs, and the binary's hash is checked again before each later run of it. The run's `TMPDIR` and home are outside the runner's temp folder, which holds the binary and the job's file commands. Pin the action itself to a full commit. |
| A tag or action moved under you | Every action this one uses is pinned to a full commit. |

**Not defended.**

- A wrong review. The model can miss a bug, invent one, or be steered by
  the pull request's text. Findings name files and lines you can check, and
  the evidence holds the full record of what the agent read.
- A maintainer's `workflow_dispatch` run on a pull request is as safe as the
  read-only run above, and no safer: the model key is present, and the
  model sees the pull request's text.
- A self-hosted runner shared with other jobs. The review runs nothing from
  the pull request, but the runner's own state is outside this model.
- Cost. A large or adversarial diff costs tokens; set `max-turns` (and
  `max-budget-usd` from 1.3.0) and a job `timeout-minutes`.
- An old base branch. A pull request may target any branch, and its
  policy is that branch's: protect the branches pull requests may target,
  or give the policy in the `policy` input.
- Who reads the evidence. On a public repository anyone signed in can
  download it; see [The evidence](38-github-action.md#the-evidence).

## Mentioning the bot

A run started by an `@abhed` comment on an issue or pull request needs an
`issue_comment` event, which, like `pull_request_target`, runs with secrets
and a write token on input anyone can write. It is not offered here;
`workflow_dispatch` is the maintainer's way to ask for a review.
