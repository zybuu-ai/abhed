# The GitHub Action

`integrations/github-action` runs an Abhed task in a GitHub Actions job: it
installs a pinned release, runs `abhed -p` with `-verify strict`, keeps the
evidence as an artifact, and fails the job when the run fails. The same
action reviews pull requests in `mode: review`; that has a guide of its own,
[Pull request review](39-pr-review.md).

```yaml
name: Abhed task
on:
  workflow_dispatch:
    inputs:
      task: {description: What Abhed should do, required: true}

permissions:
  contents: read

jobs:
  task:
    runs-on: ubuntu-24.04
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4.4.0
        with:
          persist-credentials: false
      - id: abhed
        uses: zybuu-ai/abhed/integrations/github-action@<full commit>
        with:
          task: ${{ inputs.task }}
          model-provider: anthropic
          model: claude-opus-5
          model-api-key: ${{ secrets.ABHED_MODEL_KEY }}
          policy-file: .github/abhed-policy.json
          permission-mode: accept-edits
          allow: |
            bash(go test*)
          deny: |
            bash(git push*)
          max-budget-usd: "2"
```

Pin the action to a full commit, as you pin every action: a tag can be moved
to other code. The examples in `integrations/github-action/examples/` are
ready to copy.

## What it does

1. **Installs Abhed.** It downloads the release archive for the runner
   (Linux or macOS, x86-64 or arm64) and checks its SHA-256 against the
   value the action pins in `release.txt`, before anything in it runs. A
   mismatch fails the step with both hashes. With `verify-signature` (on by
   default) it also checks the release's Sigstore signature over
   `SHA256SUMS` with cosign, as [Verify a download](01-getting-started.md#verify-a-download)
   describes, and that the signed list names the archive with that hash.
   Another version needs its own hash: `abhed-version: v1.3.0` with
   `abhed-sha256:` the archive's line from that release's `SHA256SUMS`.
   `abhed-path` uses a binary already on the runner instead.
2. **Installs bubblewrap** on a Linux runner that lacks it (when `sudo` works
   without a password, as on GitHub's own runners), so commands run in the
   process sandbox. Abhed refuses to start rather than run commands with a
   weaker sandbox than the configuration asks for.
3. **Writes the run's settings.** `policy-file` is copied out of the
   workspace first, so the agent cannot change the copy the run reads, and
   passed as `-settings` ([The command line](18-cli.md)). The model inputs are
   laid over it as a provider named `github-action`; the key is not written
   there, only the name of the variable that holds it.
4. **Runs the task** with `abhed -p -output-format stream-json`, the task
   after `--` so one starting with `-` is not read as a flag.
5. **Keeps the evidence** and uploads it as the `abhed-evidence` artifact
   (`upload-evidence`, `evidence-name`, `evidence-retention-days`).
6. **Fails the job** when `abhed` exited non-zero (`fail-on-error`), after
   the upload, so a failed run still leaves its evidence.

## Inputs

| Input | Default | |
|---|---|---|
| `task` | | the task |
| `model-provider`, `model`, `model-base-url` | | the provider type (`anthropic`, `openai`, `openai-compatible`, ...), model id and base URL; empty uses the policy file's model |
| `model-api-key` | | the provider's key; pass a secret |
| `policy-file` | | a JSON settings file: `permissions`, `sandbox`, `limits`, `verify`, `model` |
| `permission-mode` | `default` | `default`, `accept-edits`, `plan` or `auto` |
| `allow`, `deny` | | rules, one per line; a line may hold several, comma-separated |
| `verify` | `strict` | `off`, `label` or `strict` |
| `max-turns`, `max-budget-usd` | | limits for the run |
| `env-passthrough` | | runner variables the agent's process may see, comma-separated |
| `isolate-home` | `true` | give the run a home of its own |
| `allow-persisted-credentials` | `false` | run beside a token `actions/checkout` left in `.git/config` |
| `allow-pull-request-target` | `false` | run on a `pull_request_target` event |
| `abhed-version`, `abhed-sha256`, `abhed-path`, `verify-signature`, `install-sandbox` | | the install, above |
| `fail-on-error`, `upload-evidence`, `evidence-name`, `evidence-retention-days` | | the end of the job |

Outputs: `exit-code`, `session-id`, `verdict` (`verified` or `unverified`),
`subtype` (how the run ended), `result` (the answer) and `evidence-dir`.
The exit codes are those of `abhed -p`
([Automation](10-automation.md#exit-codes)): `8` is an answer strict
checking could not verify.

In `default` mode a headless run is refused anything that changes a file or
runs a command unless an allow rule names it, and the step summary lists
each refusal with the exact flag that would allow it. Keep the allow list to
what the task needs.

## The evidence

| File | |
|---|---|
| `run.jsonl` | every recorded event as it happened, then the result line with the verdict |
| `record.jsonl` | the session's record, exported with its chain; `abhed record verify record.jsonl` checks it on any machine ([Exporting](12-records.md#exporting)) |
| `record-verify.txt` | that check, run on the runner |
| `transcript.html` | the session, to read |
| `summary.json` | the exit code and the result line |
| `stderr.log` | Abhed's notes: each refusal and what would allow it, warnings |
| `abhed-version.txt` | the binary's version |

The job's summary page shows the exit code, how the run ended, the verdict,
tokens and cost, each policy refusal, and each statement that was not
verified.

## Secrets and credentials

- **The model key** reaches Abhed in one environment variable, named in the
  settings as the provider's `api_key_env`. It is masked in the log, never
  on a command line, and never in a file. A command the agent runs in a
  sandbox tier gets an environment of its own, without it; on the `none`
  tier a command inherits Abhed's whole environment, key included, so do not
  set `sandbox.min_tier` to `none` in CI.
- **The agent's environment is not the runner's.** `abhed` runs under
  `env -i` with `PATH`, a home, the locale, `TMPDIR`, `CI`, the key's
  variable and the names in `env-passthrough`, nothing else. A GitHub token
  (`GITHUB_TOKEN`, `GH_TOKEN`, `ACTIONS_*`, the OIDC request token) is never
  passed, even when named. The process sandbox narrows this again for each
  command: it keeps `PATH`, `HOME`, the locale, `TMPDIR` and the toolchain
  variables (`GOPATH`, `GOMODCACHE`, `CARGO_HOME`, `JAVA_HOME`, ...) only.
- **The checkout's token.** `actions/checkout` keeps the job's token in the
  repository's git configuration unless `persist-credentials: false`; the
  agent can read that file. The action refuses to run beside it unless
  `allow-persisted-credentials: true`.
- **Its own home.** With `isolate-home` (the default) the run's home is a
  fresh folder under the runner's temp folder, so a self-hosted runner's
  `~/.abhed/config.json` neither loosens the run nor collects its record.
  Toolchain caches under the home start empty; set their own variables
  (`GOMODCACHE`, say) and pass them with `env-passthrough` to reuse them.
- **Permissions.** Give the job the least it needs: `contents: read` for a
  task whose changes stay on the runner. The action pushes nothing.
- **pull_request_target** runs with secrets and a write token on a pull
  request's event, and must never be given the pull request's code; the
  action refuses that event unless `allow-pull-request-target: true`.

## Keeping the pin current

`release.txt` names the release the action installs by default and its
archives' SHA-256, copied from that release's `SHA256SUMS`. A release cannot
pin itself, so the pin moves by a commit after each release is published,
and a workflow that pins the action to a commit gets the Abhed release that
commit pins. To run a newer release first, set `abhed-version` and
`abhed-sha256`.

## Testing the action

`python3 -m unittest discover -s integrations/github-action/tests` runs the
install, run and review scripts as the action runs them, against a fake
release served from a folder and a fake `abhed`. With `ABHED_BIN` naming a
built binary, it also runs them against the real engine on a scripted local
model, in both modes. No GitHub workflow, network or model is used.
