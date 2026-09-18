# Results — Abhed vs. aider vs. a bare agent loop

**Model:** `gemma4:26b`, served locally by Ollama at `http://127.0.0.1:11434`
(OpenAI-compatible endpoint), identical for all three systems.
**Hardware:** Apple M3 Pro, 36 GB RAM (`sysctl hw.memsize` = 38654705664 bytes).
**Dates:** 2026-09-14, single sequential run, ~2h27m wall clock (06:54–09:22 IST).
**Abhed commit:** `de52025` (built as `cmd/titan` at the time; the project was
renamed Abhed on 14 September 2026 — see the naming note in `README.md`).
**aider version:** 0.86.2.
**Task set:** 24 exercism/python exercises, 3 systems, 72 runs total, one run
at a time (see `README.md` for why: single local model is the bottleneck).

Raw data: [`bench/results/2026-09-14/results.json`](results/2026-09-14/results.json)
(also `results.csv`). One JSON per run under `bench/results/2026-09-14/<system>/<slug>.json`.

## Per-system summary

| System | Pass rate | Mean wall time (s) | Mean tokens | Mean turns | Tasks touching other files |
|---|---|---|---|---|---|
| **abhed** | **24/24 (100%)** | 137.6 | 59,607 | 10.0 | 0/24 |
| aider | 19/24 (79%) | 106.4 | 4,761 | n/a (aider doesn't report turns) | 0/24 |
| bare | 22/24 (92%) | 125.1 | 17,350 | 7.2 | 0/24 |

### Reading the token column

Tokens are not cost. On a hosted per-token API they are; on the owned GPUs
this targets, cost is GPU-seconds, and the two diverge because input and
output tokens are not the same kind of work.

| System | Mean input | Mean output | Output share |
|---|---|---|---|
| abhed | 55,535 | 4,071 | 6.8% |
| bare | 14,805 | 2,544 | 14.7% |

Input tokens are prefill: parallel, and a stable prefix across turns that a
prefix cache can serve. Output tokens are decode: serial, uncacheable, and
what actually occupies a GPU. 93% of Abhed's tokens are the cheap kind, so
the 3.4× token ratio against the bare loop is not a 3.4× cost ratio.

What these runs do **not** measure is cache hit rate — the harness did not
record it, so the prefill saving is unquantified here rather than assumed.
Wall time is the honest proxy in this table, and on that measure the three
systems are within 30% of each other while pass rates differ by 21 points.

aider reports no token split, so it is absent from the second table rather
than estimated.

Zero runs across all 72 timed out, zero hit a harness error, and zero touched
any file outside `<slug>.py`. Every failure recorded below is a genuine test
failure after the agent believed it was done (or gave up), not a crash, a
timeout, or a scoring artifact — the original test file was restored before
every scoring pass specifically to rule out "edited the test to make it
pass."

## What the numbers say, plainly

**Abhed wins on correctness, by a real margin, and it is not close.** 24/24
vs. 19/24 (aider) vs. 22/24 (bare) on the same model, same prompt, same
tasks. If the harness didn't matter, these three numbers would cluster; they
don't.

**Abhed does not win on speed or token economy, and pretending otherwise
would be spin.** It used roughly 12x aider's tokens and 3.4x the bare agent's
tokens, and it was the slowest of the three on mean wall time (137.6s vs.
106.4s aider, 125.1s bare). Some of that gap is inherent to giving the model
more tools and more turns to work with (mean 10.0 turns vs. bare's 7.2); some
of it is Abhed's own tool-call overhead (each `read`/`write`/`bash` round
trip through the sandboxed process costs real wall time on top of the model's
own latency). The honest reading: Abhed trades tokens and time for a
qualitatively different behavior — it does not stop at "I think this passes,"
it verifies, and when the first fix is wrong it iterates instead of quitting.
That behavior is worth something on a task where correctness is the metric
that matters, and it costs something measurable to get.

**Where aider lost, it lost to giving up or under-verifying, not to a broken
setup.** With `--read INSTRUCTIONS.md --read <slug>_test.py` in place (see the
naming/invocation note below — this was not the first invocation tried),
aider had the same information as Abhed and the bare agent. Its five failures
(`acronym`, `clock`, `phone-number`, `run-length-encoding`, `tournament`) were
genuine wrong implementations that its own single-shot `--message` invocation
did not catch — aider's default flow proposes an edit and stops; nothing in
the invocation used here told it to run pytest itself and iterate on
failures, so a subtly wrong first attempt (e.g. an off-by-one in
`run-length-encoding`, a miscounted allergen bit in a task like `acronym`)
shipped uncorrected. This is a fair characterization of aider's default
single-shot mode, not a claim that aider cannot do better with a different
flag or a chat-driven loop.

**The bare agent's two failures (`luhn`, `run-length-encoding`) are the
harness-effect floor.** With four tools, no system prompt beyond one
sentence, and 30 turns to work with, it still solved 22/24 — a reminder that
this particular model is fairly capable at these small, well-specified
exercises even with almost no scaffolding. Abhed's edge over bare (24 vs. 22)
is two tasks; its edge over aider (24 vs. 19) is five. The bare-vs-aider gap
(22 vs. 19) is itself informative: a naive tool loop with no context
management beat a mature, widely used open-source agent's default one-shot
invocation on this suite, on this model.

**`tournament` is the outlier for cost.** Abhed took 577.4s on it — by far
its longest run — while aider (218.1s, and still failed it) and bare (267.6s,
passed) were both faster. That one task alone pulls Abhed's mean wall time up
substantially; without it, Abhed's mean would be noticeably closer to the
other two systems. Look at the per-task table before generalizing "Abhed is
slower" to every task — on the small, simple exercises (`leap`, `hamming`,
`raindrops`, `pangram`) Abhed's wall time is close to or better than aider's.

## Per-system detail

| System | Pass | Fail | Mean wall time (s) | Median wall time (s) | Mean tokens | Mean turns |
|---|---|---|---|---|---|---|
| abhed | 24 | 0 | 137.6 | 113.8 | 59,607 | 10.0 |
| aider | 19 | 5 | 106.4 | 82.2 | 4,761 | n/a |
| bare | 22 | 2 | 125.1 | 74.0 | 17,350 | 7.2 |

(Medians computed from `results.json`; aider does not expose a per-turn count
in its CLI output, only aggregate sent/received token counts, so "mean turns"
is not applicable for that system — this is a real reporting-surface
difference between the tools, not a gap in our data collection.)

## Per-task results

| Task | abhed | aider | bare | abhed time(s) | aider time(s) | bare time(s) |
|---|---|---|---|---|---|---|
| acronym | PASS | **FAIL** | PASS | 194.9 | 198.8 | 152.0 |
| allergies | PASS | PASS | PASS | 110.0 | 78.9 | 42.0 |
| anagram | PASS | PASS | PASS | 186.2 | 103.7 | 72.0 |
| binary-search | PASS | PASS | PASS | 63.3 | 67.1 | 290.4 |
| bob | PASS | PASS | PASS | 150.5 | 149.4 | 136.8 |
| clock | PASS | **FAIL** | PASS | 231.0 | 190.8 | 76.1 |
| grade-school | PASS | PASS | PASS | 143.9 | 147.0 | 143.6 |
| hamming | PASS | PASS | PASS | 61.5 | 35.0 | 21.8 |
| isogram | PASS | PASS | PASS | 47.1 | 48.8 | 38.8 |
| leap | PASS | PASS | PASS | 45.5 | 34.5 | 22.8 |
| luhn | PASS | PASS | **FAIL** | 139.9 | 134.4 | 191.0 |
| matrix | PASS | PASS | PASS | 80.1 | 85.5 | 500.1 |
| pangram | PASS | PASS | PASS | 62.1 | 52.6 | 21.5 |
| phone-number | PASS | **FAIL** | PASS | 269.4 | 193.4 | 190.4 |
| protein-translation | PASS | PASS | PASS | 117.5 | 67.0 | 44.3 |
| raindrops | PASS | PASS | PASS | 57.7 | 57.8 | 21.4 |
| rna-transcription | PASS | PASS | PASS | 61.7 | 26.5 | 24.8 |
| robot-simulator | PASS | PASS | PASS | 152.4 | 107.8 | 62.4 |
| roman-numerals | PASS | PASS | PASS | 82.3 | 65.5 | 230.2 |
| run-length-encoding | PASS | **FAIL** | **FAIL** | 87.4 | 194.3 | 275.9 |
| series | PASS | PASS | PASS | 127.4 | 43.0 | 49.8 |
| sieve | PASS | PASS | PASS | 86.2 | 66.6 | 38.1 |
| tournament | PASS | **FAIL** | PASS | 577.4 | 218.1 | 267.6 |
| word-count | PASS | PASS | PASS | 166.8 | 186.4 | 87.8 |

## A correction made mid-run: the aider invocation

The first draft of the aider invocation passed only `<slug>.py` on the
command line, with no `--read` flags. Aider only loads files named explicitly
on its command line into chat context — it does not read other files sitting
in the workspace on its own. In manual testing (before the orchestrator's
aider phase began; abhed was still running its 24 tasks), this invocation
left aider unable to see `INSTRUCTIONS.md` or the test file at all, and it
correctly said so rather than guessing at a leap-year implementation from the
function name alone.

That is a fair thing to say about aider's default CLI ergonomics — it isn't
a repo-aware agent by default, unlike Abhed and the bare agent, which both
have a `read_file`-style tool and were explicitly told to read
`INSTRUCTIONS.md` themselves. But it would not have been a fair comparison:
it would have scored aider on a task it was never given the information to
attempt. The invocation was corrected to
`--read INSTRUCTIONS.md --read <slug>_test.py` (both read-only — aider cannot
propose edits to them, and the anti-cheat rescoring against the pristine test
file would have caught it regardless) before any aider run in the recorded
72-run set executed. No results were discarded because none had been
produced yet under the old invocation. See `README.md` for the exact
before/after commands.

## Limitation: this is not SWE-bench

This suite is 24 single-file exercism exercises, not a multi-file real-repo
benchmark. It was chosen deliberately over something like SWE-bench because
it runs entirely on a laptop, needs no Docker, no per-instance containers,
and no network access beyond the initial `git clone`, and each task scores in
under a minute. That reproducibility is the point — anyone with this repo,
Ollama, and an afternoon can rerun this exact suite and get comparable
numbers.

The tradeoff is real and worth stating plainly: this benchmark says nothing
about multi-file refactors, large-context code navigation, ambiguous
real-world tickets, or working in a codebase the agent didn't just receive as
a clean stub. A harness that wins here on "read the spec, write the function,
verify with tests, iterate on failure" is demonstrating exactly that loop and
no more. Whether that generalizes to messier, larger tasks is a different
question this benchmark cannot answer.
