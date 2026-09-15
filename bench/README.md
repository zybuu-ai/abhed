# Abhed vs. aider vs. a bare agent loop — a reproducible local benchmark

This directory holds a small, honest benchmark comparing three coding-agent
harnesses driven by the **same local model**, so what's being measured is the
harness, not the model:

1. **abhed** — this repository's own CLI (built here as `cmd/abhed`).
2. **aider** — [aider-chat](https://aider.chat), a widely used open-source
   coding agent.
3. **bare** — a ~150-line hand-written tool-use loop
   (`bench/bare_agent.py`) against Ollama's OpenAI-compatible API, with no
   system prompt beyond one sentence, no compaction, and no permission
   policy. This is the "harness effect" floor.

All three are pointed at the same Ollama-served local model
(`gemma4:26b` at `http://127.0.0.1:11434`) and given the identical task
prompt for each exercise.

See `RESULTS.md` for the numbers and their honest interpretation.

**A note on naming.** The run was made at commit `de52025`, when the binary
was still called `titan`; the project was renamed Abhed on 14 September 2026
because the old name collided with Amazon's Titan models. The result files
and this write-up use the current name. The harness that was measured is the
same code; nothing changed with the name.

## What's being tested

24 single-file Python exercises from
[exercism/python](https://github.com/exercism/python) (MIT licensed), spanning
a spread of difficulty: `leap, hamming, isogram, raindrops, bob, acronym,
pangram, anagram, word-count, rna-transcription, series, sieve, luhn, matrix,
binary-search, run-length-encoding, roman-numerals, allergies, clock,
robot-simulator, phone-number, protein-translation, tournament, grade-school`.

For each exercise, a task workspace contains:
- `<slug>.py` — the unimplemented stub
- `<slug>_test.py` — the exercise's real test suite
- `INSTRUCTIONS.md` — the exercise's instructions (introduction + instructions)

Each system gets the identical prompt:

> Read INSTRUCTIONS.md and implement `<slug>.py` so that the tests in
> `<slug>_test.py` pass. Run `python3 -m pytest -q` to check, and fix
> failures. Do not modify the test file.

## Why exercism, not SWE-bench

This is deliberately **not** SWE-bench or any Docker-based multi-file
real-repo benchmark. Those require per-instance containers, a network, and
significant compute; they are not reproducible on a laptop in an afternoon.
Exercism's single-file exercises run entirely locally, need no Docker, take
seconds each to score, and still exercise the full loop that matters here:
read instructions, write code, run tests, iterate on failures. The tradeoff is
real — this suite says nothing about multi-file refactors, large-context
navigation, or ambiguous real-world tickets. Treat the numbers below as a
signal about harness scaffolding on a small, well-specified task, not a
general capability claim.

## Reproducing this benchmark

All commands assume macOS with Ollama already serving the model:

```
curl http://127.0.0.1:11434/v1/models   # confirm gemma4:26b is present
```

### 1. Clone the exercise set

```
git clone --depth 1 https://github.com/exercism/python.git <scratch>/exercism-python
```

### 2. Build abhed

```
env -u GOROOT go build -o <scratch>/bin/abhed ./cmd/abhed
```

(`GOROOT` is unset because it is misconfigured on the machine this was
authored on; drop `env -u GOROOT` if yours is fine.)

### 3. Python environment

System `pip` was broken on the authoring machine, and the on-PATH `python3`
(Homebrew 3.14) had no working `pip` either. A `uv`-managed venv sidesteps
both:

```
uv venv -p 3.13 <scratch>/venv
uv pip install -p <scratch>/venv/bin/python pytest aider-chat audioop-lts
```

`audioop-lts` is required: `aider-chat` imports `pydub` for an optional voice
feature, and `pydub` imports the stdlib `audioop` module that Python 3.13
removed. Without the backport, `aider --version` crashes before doing
anything — this is a real, reproducible aider/Python-3.13 interaction, not a
benchmark artifact.

### 4. Make `python3` resolve to an interpreter with pytest

The three runners and the scorer all shell out to `python3 -m pytest`. On the
authoring machine the on-PATH `python3` was a broken Homebrew 3.14 install
with no usable `pip`. Rather than mutate global/Homebrew state (out of scope,
and PEP 668–protected for good reason), we prepend a one-file shim directory
to `PATH` for every runner invocation, so all three systems and the scorer see
the exact same `python3`:

```
mkdir <scratch>/shim
cat > <scratch>/shim/python3 <<'EOF'
#!/bin/sh
exec "<scratch>/venv/bin/python3" "$@"
EOF
chmod +x <scratch>/shim/python3
```

`run_bench.py` does this automatically (`base_env()`); reproduce it manually
only if running a system by hand.

### 5. Build the task workspaces

```
<scratch>/venv/bin/python <scratch>/make_tasks.py
```

This copies each exercise's stub, test file, and instructions into
`<scratch>/tasks/<slug>/`, stashes the reference solution aside (never into
the workspace) for sanity-checking, and keeps a pristine copy of each test
file for anti-cheat rescoring.

### 6. Abhed config used for every task

```json
{
  "model": {
    "default": "local",
    "providers": {
      "local": {
        "type": "ollama",
        "base_url": "http://127.0.0.1:11434/v1",
        "model": "gemma4:26b",
        "context_window": 32768,
        "params": { "temperature": 0.2, "top_p": 0.9 }
      }
    }
  },
  "permissions": { "mode": "accept-edits" },
  "sandbox": { "min_tier": "process", "allow_network": false },
  "storage": { "driver": "memory" },
  "limits": { "max_turns": 30 }
}
```

Abhed is invoked as:

```
abhed -C <workspace> -mode accept-edits -allow 'bash(python3 -m pytest*)' \
  -max-turns 30 -p "<prompt>"
```

Sandbox tier stays `process` (sandbox-exec on macOS, writes confined to the
workspace) for every run — it is never disabled.

### 7. aider invocation

```
OLLAMA_API_BASE=http://127.0.0.1:11434 aider --model ollama_chat/gemma4:26b \
  --yes-always --no-auto-commits --no-git \
  --read INSTRUCTIONS.md --read <slug>_test.py \
  --message "<prompt>" <slug>.py
```

Run inside the task workspace. `<slug>.py` is the one editable chat file;
`INSTRUCTIONS.md` and `<slug>_test.py` are passed with `--read` so they sit in
aider's context **read-only** — aider will not propose edits to them, and the
scorer's anti-cheat rescoring (restoring the original test file before
grading) guards the case even if it tried.

This project's first draft of the invocation used only `<slug>.py` on the
command line and no `--read` flags. Aider only loads files explicitly named on
the command line into its chat context — it does not read other files sitting
in the same directory on its own — so that invocation left it unable to see
the instructions or the tests at all; in manual smoke-testing (before the
full 24-task run reached the aider phase) it correctly said so rather than
guessing, and failed the task. That is a fair thing to say about aider's
default CLI ergonomics, but it is not a fair comparison to Abhed and the bare
agent, both of which have a `read_file`-style tool and were told to read
`INSTRUCTIONS.md` themselves. The fix was made before any aider run in the
recorded benchmark executed (the orchestrator processes systems in order
abhed, then aider, then bare, and was still on abhed when the invocation was
corrected) — no aider results were discarded, because none had been produced
yet. This note exists so the invocation's history is not hidden after the
fact.

### 8. Bare agent

```
<scratch>/venv/bin/python bench/bare_agent.py \
  --workspace <task-workspace> --prompt "<prompt>" --max-turns 30
```

`bench/bare_agent.py` is ~150 lines: four tools (`read_file`, `write_file`,
`list_files`, `run_pytest`), a one-sentence system prompt, no context
compaction, no permission policy, calling Ollama's
`/v1/chat/completions` directly with `tools=[...]`.

### 9. Run everything

```
cd abhed
nohup <scratch>/venv/bin/python bench/run_bench.py --date YYYY-MM-DD \
  > bench/logs/run_YYYY-MM-DD.log 2>&1 &
```

`run_bench.py`:
- runs all 24 tasks x 3 systems, **sequentially** (one model, one bottleneck)
- kills any single run that exceeds 12 minutes and scores it a fail with
  reason `timeout`
- after each run, copies the **original, untouched** test file back over
  whatever the agent left (defeats the "edit the test to make it pass"
  failure mode), then runs `python3 -m pytest -q` in a fresh subprocess with a
  60s timeout to get the real pass/fail and passing-test count
- records whether the run touched any file other than `<slug>.py`
  (ignoring `__pycache__`, `.pytest_cache`, and aider's own metadata files,
  which are run byproducts, not edits)
- writes `bench/results/<date>/<system>/<slug>.json` per run, plus combined
  `results.json` and `results.csv`
- is resumable: rerunning with the same `--date` skips any task/system pair
  that already has a result file, unless `--force`

Expect 3-5 hours total on one machine, since only one model call happens at a
time across all 72 runs.

### 10. Summarize

```
<scratch>/venv/bin/python bench/summarize.py --date YYYY-MM-DD
```

prints the Markdown tables used in `RESULTS.md`.

## Files

| Path | What |
|---|---|
| `bench/bare_agent.py` | the bare tool-loop baseline |
| `bench/run_bench.py` | orchestrates all 72 runs, scores them, writes results |
| `bench/summarize.py` | turns `results.json` into Markdown tables |
| `bench/results/<date>/` | one JSON per run, plus combined `results.json`/`results.csv` |
| `bench/RESULTS.md` | the numbers and their interpretation |

Everything under `bench/` in this repo is the runner and the report. The task
workspaces, the exercism clone, the built `abhed` binary, the Python venv, and
the raw per-run transcripts live outside the repo (a scratch directory), since
they are either large, regenerable, or third-party source — not something to
commit.
