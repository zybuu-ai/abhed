# Method: several harnesses, one model, multi-file tasks

This rig exists to answer one question: **with the model held fixed, how much
does the harness matter on real multi-file work?** The earlier suite in
`bench/` (24 single-file Exercism tasks, one run) could not answer it.

Results are published whatever they say, with the per-run JSON.

## What is compared

Harnesses that can drive the **same local model** through an OpenAI-compatible
endpoint. A comparison across different models measures the model, so closed
products that cannot use the same one are out of scope — that is a limit of the
comparison, not a claim about them.

| Harness | Invocation | Source, read 21 Sep 2026 |
|---|---|---|
| Abhed | `abhed -mode bypass -max-turns 60 -p <prompt>`, shipped default config with only the model changed | `docs/guide/10-automation.md` |
| pi | `pi --provider bench --model <id> --mode json -p <prompt>`, model declared in `~/.pi/agent/models.json` | pi-mono `packages/coding-agent` README and `docs/models.md` |
| OpenHands | `openhands --headless --json --override-with-envs -t <prompt>`, `LLM_MODEL=openai/<id>`, `LLM_BASE_URL`, `LLM_API_KEY` | OpenHands docs: CLI headless, command reference, local LLMs |

Each runs **unattended in the mode its own documentation gives for that**:
nothing prompts. OpenHands' headless mode "always runs in always-approve mode";
pi has no permission system; Abhed's `bypass` mode still enforces its deny
rules and its sandbox, because that is what the product is.

pi and OpenHands read configuration from the home directory, so each run gets
a throwaway `HOME`. The operator's own settings are never read or written.
Both are installed under the rig's cache (`.cache/tools`), not globally.

Every harness runs with **stdin closed**. pi merges piped stdin into its
prompt, so with an inherited stdin it waits for input that never comes and
never calls the model. The first `doctor` run on pi sat for ten minutes that
way with the model idle — a rig fault that would otherwise have been recorded
as thirty-minute timeouts against pi.

Verified on 21 Sep 2026 with `gemma4:26b`: Abhed 0.2.1-dev, pi 0.73.1 and
OpenHands CLI 1.16.0 each passed `doctor`. The OpenHands and pi invocations
above are the documented ones and needed no change.

**`rig.py doctor --harness X` must pass before `run` will include X.** It asks
the harness to create one file on the benchmark model. A harness that cannot
make a tool call in this setup would score zero for a reason that has nothing
to do with the harness; the earlier suite's aider result carried exactly that
kind of caveat, and this is the guard against repeating it.

## Tasks

SWE-bench Verified instances, from the repositories whose tests are pytest
node ids and whose dependencies install from wheels into a plain virtualenv:
`pytest-dev/pytest`, `pylint-dev/pylint`, `pallets/flask`.

A workspace is a copy of the *installed* tree, not a clone: installing a
package can write files git does not track — pytest's `_version.py` comes from
setuptools_scm — and without them the package does not import. The first
version of this rig cloned, and the validity gate rejected every instance.

Left out, and why:

- **Most of Verified** (django, sympy, astropy, scikit-learn, matplotlib,
  sphinx, xarray): compiled extensions or a bespoke test runner. Those need the
  official per-instance containers. This rig trades coverage for running on a
  laptop with no container runtime.
- **`psf/requests`**: its suite calls the live httpbin.org, so a run depends on
  somebody else's server and cannot be repeated offline; several instances are
  Python 2 bugs that do not reproduce on a current interpreter.

**An instance enters the suite only if it behaves here** (`rig.py validate`):
at the base commit the FAIL_TO_PASS tests must fail, and with the gold patch
every FAIL_TO_PASS test must pass. Anything else is dropped and the reason kept
in `invalid.json`.

**One stated deviation.** Dependencies are not pinned to the versions the
dataset was built with, so a few PASS_TO_PASS tests fail from drift alone. A
test that fails *with the reference fix applied* cannot tell a regression from
the environment, so it is set aside for that instance — provided the gold patch
still passes at least 90% of the instance's PASS_TO_PASS tests. Below that the
instance is dropped. Which tests were set aside, per instance, is written to
`suite.json` beside the results.

The suite is therefore whatever survives on the machine that runs it. On the
machine this was built on (macOS arm64, 21 Sep 2026): 30 instances in the
pool, 29 built, **19 valid** — 15 from pytest and 4 from pylint — of which 8
had between one and five drifted tests set aside. flask's single instance
fails on dependency drift and is out.

The harness sees the problem statement and a checkout at the base commit with
**no git history** — the fix is in the repository's future, and `git log` must
not be a way to find it. It does not see the gold tests.

## Scoring

The SWE-bench rule, re-implemented on virtualenvs:

1. The agent's changes to any file the gold test patch touches are reverted.
2. The gold test patch is applied.
3. The test files are run. The instance is **resolved** only if every
   FAIL_TO_PASS and every PASS_TO_PASS test passes.

This is not the official containerised evaluation and must not be reported as
a SWE-bench Verified score. It is checked two ways instead: every instance
passes the base-fails/gold-passes gate above, and **`rig.py selftest`** runs a
harness that does nothing (must score 0) and one that applies the gold patch
(must score 100) through the whole pipeline. Neither needs a model.

## Conditions

| Condition | Window | |
|---|---|---|
| `full` | 32,768 | |
| `tight` | 24,576 | not lower: OpenHands documents 22,000 as its minimum, and a window a harness says it cannot work in measures nothing |

The window is set **at the endpoint** (`rig.py models` writes Ollama variants
with `num_ctx`), so every harness meets the same hard limit. Abhed and pi are
also told the window through their documented setting. No such setting was
found in OpenHands' CLI documentation; it runs on its defaults.

## Runs and statistics

- **Three runs per cell at minimum.** Local models are not deterministic, and
  one run cannot tell a two-task gap from noise.
- The order of (run, task, condition, harness) is shuffled with a fixed seed,
  so thermal throttling or a busy afternoon falls on every harness alike.
- Reported per harness and window: the resolve rate of each run, their mean
  and standard deviation, how many tasks were solved on every run, wall time.
- **Differences are paired by task** with a 95% bootstrap interval over tasks
  (10,000 resamples, fixed seed). An interval that spans zero is reported as
  not a difference, in those words.

## Known limits

- A small suite from two or three repositories. It says nothing about
  languages other than Python, or about repositories much larger than these.
- One model. A harness tuned for frontier models may rank differently on one.
- Verified's problem statements are public, and the model may have seen the
  fixes in training. That inflates every harness alike; it does not explain a
  difference between them.
- The harnesses are moving targets. Versions are recorded with each run.
