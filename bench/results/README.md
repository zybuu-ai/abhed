# Benchmark results

Each rig run lives in a directory named by its date, with its plan, one
result per session, a summary and a README that says what the run can and
cannot show. `2026-09-14/` is older: the single-file Exercism suite, with
`results.json`, `results.csv` and one file per system, described in
`bench/RESULTS.md`.

Runs on the corrected rig are published here under a new date, with that
README, whatever they show.

## Withdrawn runs

Three runs of the multi-harness rig were published and have been withdrawn:
`pilot-2026-09-21`, `repilot-2026-09-21` and `2026-09-22-easy`. The rig they
ran on was later found not to give every harness the same conditions, and in
places to score wrongly. Every defect found, and whom it could favour:

| Defect | Could favour | Could hurt |
|---|---|---|
| Abhed alone was capped at 60 turns; its default is 100, OpenHands' 500, pi has none | the others | Abhed |
| Abhed ran with the operator's home directory, so the operator's own skills and `ABHED.md` shaped its sessions | Abhed, or not | Abhed, or not |
| The harnesses inherited the operator's whole environment, including provider credentials and variables that override Abhed's model | — | any |
| pi was told an 8,192-token output limit per turn and Abhed's own default happened to match; OpenHands had none | OpenHands | Abhed, pi |
| On a hosted model, Abhed and pi were told a 32k window instead of the model's own (131k on the one used) and OpenHands was told none (hosted runs only) | OpenHands | Abhed, pi |
| Task environments lacked each project's own test requirements, so agents that ran the wider test suite met import errors | — | any |
| The prepared environment was the agent's to change, so a package an agent installed reached the score and later sessions | — | any |
| An agent that committed its own work left an empty diff, and scoring laid the gold tests over its edited test files | — | any harness that commits; OpenHands does |
| Scoring ran in the agent's own workspace, so files it left behind stayed in the tree under test | — | any |
| A change the rig could not read, for example while its progress view held git's index lock, could be scored as a loss or recorded with an empty patch | — | any |
| Parallel sessions could share a workspace and a temp directory | — | any, on parallel runs |
| A session the machine slept through was scored rather than redone | — | any |
| A resume under the same date could mix sessions run with a different model, endpoint or timeout | — | any |
| The rig's own git read the operator's git configuration, which could change what a diff looked like | — | any |
| An organisation config at `/etc/abhed/config.json` could override the settings the rig gave Abhed | Abhed, or not | Abhed, or not |
| Processes a harness started could outlive its timeout, its exit or an interrupt of the rig | — | any later session on the machine |

The three runs ran one session at a time on a local model, so the defects
that need a parallel or hosted run did not touch them; the others could
have. What they showed, for the record and not as a comparison:
`2026-09-22-easy` resolved Abhed 5/7, pi 5/7 and OpenHands 4/7, with every
interval spanning zero; `pilot-2026-09-21` resolved none of its five
sessions; `repilot-2026-09-21` published its plan and suite but no session
results. They are withdrawn because the conditions were unequal, not because
of what they showed. They stay in the repository's history, for example
`git show e8f121db78c17a12effde27c2803d3f49c8a5f49:bench/results/2026-09-22-easy/README.md`,
and `bench/rig/METHOD.md` describes the rig as it now runs.
