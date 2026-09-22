# Easy-band pilot: three harnesses, one local model, one run

Run 22–23 September 2026 on an M3 Pro (36 GB) with Ollama, base model
`gemma4:26b`, window 32,768, thirty-minute cap per session. Tasks: the seven
valid instances SWE-bench Verified rates `<15 min fix` in the pytest and
pylint pool (`--difficulty easy`). One run per cell, so **this is a pilot,
not a result**: it says the band separates harnesses, not which is better.
Method: `bench/rig/METHOD.md`. Raw per-session JSON, and for Abhed the full
event record and HawkEYE report, are beside this file.

| Harness | Window | Runs | Resolved per run | Mean | SD | Tasks solved every run | Mean wall (s) |
|---|---|---|---|---|---|---|---|
| abhed | full | 1 | 71% | 71.4% | nan | 5/7 | 786 |
| openhands | full | 1 | 57% | 57.1% | nan | 4/7 | 829 |
| pi | full | 1 | 71% | 71.4% | nan | 5/7 | 461 |

Difference in mean resolve rate, paired by task, 95% bootstrap interval. An interval that spans zero is not a difference.

| Comparison | Window | Difference | 95% interval |
|---|---|---|---|
| abhed − openhands | full | +14.3 pts | -28.6 to +57.1 (spans zero) |
| abhed − pi | full | +0.0 pts | -42.9 to +42.9 (spans zero) |

## Per task

| Task | Abhed | OpenHands | pi |
|---|---|---|---|
| pylint-4970 | ✗ 22m | ✗ 9m | ✗ 10m |
| pytest-10081 | ✓ 24m | timed out 30m | ✓ 11m |
| pytest-5809 | ✓ 14m | ✓ 13m | ✓ 6m |
| pytest-6202 | ✓ 6m | ✗ 21m | ✓ 8m |
| pytest-7205 | ✓ 10m | ✓ 5m | ✓ 5m |
| pytest-7432 | ✗ 11m | ✓ 12m | ✓ 7m |
| pytest-7982 | ✓ 4m | ✓ 6m | ✗ 7m |

No harness broke an existing test (every PASS_TO_PASS held). `pylint-4970`
beat all three.

## What may be said

- The easy band does what it was chosen for: 14 of 21 sessions resolved,
  against 1 of 8 on the unbanded suite the day before. It separates
  harnesses; the whole suite on this model did not.
- Every interval spans zero. Abhed and pi resolved the same five of seven;
  OpenHands four. With one run and seven tasks, nothing here is a ranking
  and nothing goes on the site. Three runs per cell (63 sessions, about
  twelve hours on this machine) is the minimum for a published number.
- pi was fastest by a wide margin: 7.7 minutes a session against 13 for
  the other two, on the same model, with the same outcomes.
- Tokens: Abhed averaged 233k input tokens a session, pi 400k. OpenHands
  reports none (rig gap, #75).

## What HawkEYE showed in Abhed's sessions

Both unresolved sessions ended the same way: the model spent its whole
8,192-token output budget on reasoning and made **no tool call** — three
turns running on `pytest-7432`, which the loop then ended as *stalled*, and
the 26th turn of `pylint-4970`, which took 268 seconds. That is the spiral
seen in the first pilot, and it is a harness fix, not a model one: a turn
that hits the output cap with no call is a stall and should be answered as
one (#73).

Every session also carries a `cold-cache` finding, which is wrong: 0.8 s to
first token on a 23k-token prompt is a warm cache. Ollama reports no
cached-token count and the finding reads absence as zero (#74).

## Next

1. Fix the output-cap stall (#73) and re-run the band, three runs per cell.
2. Read OpenHands' token usage (#75) so the cost column has three rows.
3. Only then a number on the site, labelled as the easy band.
