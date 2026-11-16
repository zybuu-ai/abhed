# Abhed GitHub Action

Runs a task, or reviews a pull request, with [Abhed](https://github.com/zybuu-ai/abhed),
the secure harness for deep agents.

- Installs a pinned Abhed release, checked against its SHA-256 (and, by
  default, the release's Sigstore signature) before anything in it runs.
- Runs `abhed -p` with `-verify strict` by default, so a task whose answer
  is not verified fails the job (exit 8).
- Uploads the run's output, its exported record (verifiable anywhere with
  `abhed record verify record.jsonl`) and a transcript as an artifact.
- In `mode: review`, reviews the pull request's diff read-only and posts one
  review, with inline comments where a finding has a file and line. It never
  approves and never pushes.

```yaml
permissions:
  contents: read
steps:
  - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4.4.0
    with:
      persist-credentials: false
  - uses: zybuu-ai/abhed/integrations/github-action@<full commit>
    with:
      task: fix the failing tests; run go test ./...
      model-provider: anthropic
      model: claude-opus-5
      model-api-key: ${{ secrets.ABHED_MODEL_KEY }}
      permission-mode: accept-edits
      allow: bash(go test*)
```

Full examples are in [`examples/`](examples). The guides are
[GitHub Action](../../docs/guide/38-github-action.md) and
[Pull request review](../../docs/guide/39-pr-review.md), which holds the
threat model.

Tests: `python3 -m unittest discover -s tests` (set `ABHED_BIN` to also run
against a real binary on a scripted local model).
