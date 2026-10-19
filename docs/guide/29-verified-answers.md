# Verified answers

Abhed does not promise the model is right. It shows you which statements it
could check against what actually happened in this session, and it marks the
rest.

When a run ends, each statement of the agent's answer is held against the
session's own record: the commands it ran and what they printed, the files it
read and changed, and the files as they are now. Every statement comes out as
one of three:

| | | What it means |
|---|---|---|
| ✓ | **verified** | a tool result from this session, still current, shows it, and a check found the two consistent |
| ? | **unverified** | nothing in this session shows it: general knowledge, a guess, or a result that has since changed |
| ✗ | **contradicted** | the record says otherwise. It is shown struck through, beside the evidence against it, never as fact |

"Verified" is a narrow word here. It means the statement matches what this
session observed, not that the world agrees: a test suite that passes can still
test the wrong thing, and a file the agent read can be wrong. What Abhed
guarantees is the other direction: nothing is shown as verified unless it is
tied to evidence from this session that a check could compare it with, and a
statement the record contradicts is never presented as fact. The checks are
plain comparisons, with no model involved, so they hold with a small or weak
model as well as a strong one.

## Modes

```json
"verify": {
  "mode": "label"
}
```

| `mode` | What it does |
|---|---|
| `off` | nothing is checked |
| `label` | each answer is checked and labelled; the run ends as it would have. No tool is added and the prompt is unchanged. A model call is added only when a statement is contradicted: the agent then gets **one** revision turn to correct it |
| `strict` | as `label`, and the agent is given the `answer` tool. When a statement about the session's work is not verified, the agent gets **one** revision turn with the findings; if it is still not verified, the run ends `unverified` |

Unset, the surface chooses. The interactive terminal, the workbench and
console, and Abhed Studio use `label`, because a person reads the answer there
and labelling costs nothing unless the answer contradicts the record. `abhed -p`, `abhed rpc`, unattended server runs
and the SDK check nothing unless asked, so a script's output does not change
under it.

Per run or session:

- `abhed -verify strict` (or `label`, `off`), interactive or with `-p`;
- `{"method":"start","verify":"strict"}` on `abhed rpc`;
- `{"prompt":"…","verify":"strict"}` on `POST /v1/sessions`;
- `abhed.Options{Verify: "strict"}` in the SDK.

A managed `verify.mode` binds: a flag, a session or an SDK option may make
checking stricter (`off` < `label` < `strict`), never looser. A workspace's
`.abhed/config.json` may also only raise it, and only above the interactive
default: an untrusted workspace cannot turn labels off.

## How each statement is checked

The answer is split into statements: one per sentence, list item or table
row. Code blocks, headings, questions, courtesies ("let me know if…"), advice
("run `make test` to confirm") and stated intentions ("I'll update the docs")
are left out, and at most 40 statements are checked. From each statement Abhed
takes what can be compared: quoted text and backticked text, file paths, host
names and addresses, versions, dates, numbers, and a number with the word it
counts (`12 tests`, `replicas: 3`). Then:

- **The evidence must exist and be from this session.** A statement can only
  rest on a tool result the agent's own calls produced in this session. A cited
  `call_id` or `#seq` that is not in the record, or names a message rather than
  a tool result, does not count. A person's own calls at the workbench do not
  count as evidence for the model, though they do count as changes.
- **The evidence must still be current.** A file read is out of date once that
  file is edited or a command that may have changed it runs; a command's output
  is out of date after any change at all. A shell line is read with the same
  parser policy uses, quoting and redirections as the shell reads them, and
  each simple command in it is judged on its own, with runners and launchers
  (`env`, `time`, `sudo`, `timeout`, `xargs`, `npx`, `uv run`, `poetry run`,
  `pnpm exec` and the like) taken off first. A command counts as changing
  nothing only when it is a known reader in its reading form: `ls`, `cat`,
  `grep`, `jq`, `git status` and `git diff`, `go test`, `npm test`, `pytest`
  and the rest of a short allowlist, each refused with the options or
  operands that make it write (`-i` and `--in-place`, `-w`, `-o`, `--fix`,
  `-u` and `--update`, `--write`, `uniq`'s or `xxd`'s second file, `tree -o`,
  `make` with no checking target, and so on), and with no output redirected
  to a file. Anything else counts as a change, which is the cautious way to be
  wrong. Within one line, output printed before a later command on the same
  line changes something is out of date by that change: `cat f && sed -i … f`
  does not show the file as it is.
- **What the statement says must be in the output.** Its quote, and every
  path, host, version, date and number in it, must appear in the evidence,
  compared without case, terminal colours or extra spaces. A number with its
  counting word is contradicted when the output counts the same thing with
  another number and never with this one: "15 tests passed" against
  `12 tests passed` is ✗, with `12 tests passed` shown beside it.
- **"Tests pass" and "the command succeeded" need an exit code.** Such a
  statement needs a command (of the kind it names: tests, or a build) that
  exited 0 after the last change, with nothing changed after it on its own
  line (`go test && sed -i …` does not count). The kind is read from the program and its
  subcommand (`go test`, `npm test`, `pytest`, `make`, `cargo build`, …), so
  `ls tests` or `test -f go.mod` is not a test run. If the last such command since the last
  change failed, the statement is contradicted.
- **"I changed X" and "I ran `Y`" are checked against the calls.** A statement
  that the agent edited, created or removed a file needs a call in the record
  that changed that file, matched as the same file on whole path segments
  (`a.go` is not `data.go`, and a short name stands for a file in the
  workspace, not outside it). A command counts for the files a known writer
  names (a redirection, `tee`, the destination of `cp`, `mv` and `cp -t`,
  `sed -i` and `sed --expression= -i`, `rm`, `touch`, `gofmt -w`, …), not for
  one it only reads, each resolved against the folder the call ran in: the
  workspace, moved by an earlier call that is only `cd <folder>` (as the bash
  tool follows it) and by a `cd` earlier on the same line. It is contradicted
  only when every change in the session names its files in full this way and
  none is this one. After a change whose files cannot be read from the call
  (a script, `git apply`, `git checkout <branch>`, `yq -i`, `eslint --fix`, a
  folder's contents copied, a tool other than `write`, `edit`, `bash` and
  `ssh`), it is unverified instead. One that it ran a command needs that
  command as one of the simple commands of a call, with any further
  arguments, runners taken off both: `echo 'go test'` is not `go test`, and
  "I ran `npx jest`" is met by `jest --ci`; the same program and subcommand
  with other arguments leaves it unverified. With none, the statement is
  contradicted.
- **A subagent's work counts.** The parent's record names each subagent's
  session, and its own record is read too (up to three levels deep): its
  results are evidence and its calls are changes, placed at the parent's
  entry for its return, which is the `#seq` a verdict cites for them. Where
  that record cannot be read, a claim about its work is unverified, never
  contradicted.
- **`file:line` cites are checked against the file now.** The file must exist
  and have that line, or the statement is contradicted; text it quotes must be
  on that line or the two either side.

A statement with nothing in it to compare ("the design is sound") is
unverified, and so is general knowledge. These are not counted as about the
session's work, so they never trigger a revision; the footer counts them as
general knowledge.

A statement the agent cites nothing for is matched against every current
result in the session, but only one result that shows **all** of it verifies
it, and a bare number found somewhere never does: a `3` in some output is not
the 3 the statement means.

## The answer tool

In `strict` mode (or with `verify.answer_tool: true`) the agent gives its
answer through a tool rather than as text:

```json
{"text": "There are 3 replicas; the last deploy was v1.4.2.",
 "claims": [
   {"text": "There are 3 replicas", "kind": "observed", "evidence": ["call_8f2"], "quote": "replicas: 3"},
   {"text": "the last deploy was v1.4.2", "kind": "observed", "evidence": ["#41"]},
   {"text": "Kubernetes keeps the old ReplicaSet for rollback", "kind": "knowledge"}
 ]}
```

`text` is what the person reads; it is shown as the agent's reply, and the call
itself is not drawn. Each claim names the results that show it, by `call_id` or
`#seq`, and may quote them. `knowledge` claims are labelled unverified and not
checked; a `knowledge` claim about the session's own work (that tests pass,
that a file changed) is checked anyway. A model that ignores the tool and
answers in text is checked as text. The tool adds no model call: it is how the
last turn ends.

## The revision turn

When a statement comes out contradicted, in `label` or `strict` mode, or when
a statement about the session's work comes out unverified in `strict` mode or
under a contract, the agent is sent one note, recorded in the `answer.verdict`
event, listing each one and why: "Check what you can with a tool, then give
your answer again… say plainly what you could not verify." Its next answer is
checked the same way. There is only one revision per run. "I could not verify
X" is an answer the checks accept. In `label` mode the run then ends as it
would have, with the second answer labelled; a statement still contradicted
is flagged as below.

In `strict` mode (or with a contract), a run still not verified ends with the terminal reason `unverified`, and
`abhed -p` exits **8**, so CI fails closed. The answer is still printed, with
its labels. The SDK returns an `*EndedError` whose `Reason` is `unverified`;
`abhed rpc` answers the prompt with `type: "error"`, the answer and the verdict.

## Where you see it

- **A contradiction is impossible to miss.** The reply streams as it is
  written, before it can be checked, so when a statement in it is
  contradicted the first line under it, above everything else, is a bold red
  banner: `✗ NOT FACT: 1 statement in the reply above is contradicted by this
  session's record; struck through below.` (or, when a revision follows, that
  the agent is asked to correct it). The workbench puts the same flag on the
  reply itself, with a red edge, and at the top of its box; Studio gets it as
  the first line of the verdict's text.
- **The terminal**: under the reply, a footer such as
  `? 3 verified · 1 unverified · 0 contradicted · /evidence N` on one line
  (no footer for a reply with nothing to check, such as a greeting; ✓ only when
  something was verified and nothing about the work was not), then
  each contradicted statement struck through with the record against it, and
  the unverified statements about the session's work. `/evidence` lists every
  statement with its verdict and reason; `/evidence N` opens the record behind
  statement N (the command, its exit code and its output); `/evidence #seq`
  shows any entry of the record.
- **`abhed -p`**: the same lines under the answer in text output; in `json`
  and `stream-json`, the `answer.verdict` event and a `verdict` field on the
  result line.
- **The workbench and the console**: a box under the reply with a ✓, ? or ✗ badge on each
  statement, contradicted ones struck through; it opens by itself when
  something needs a look, and each `#seq` opens the tool call behind it.
- **Abhed Studio and other ACP editors**: an `agent_message_chunk` after the
  reply carrying the footer as text and the whole verdict in
  `_meta["zybuu.ai/abhed"].verdict`; `features` lists `answer-verdicts`.
- **HawkEYE**: an Answers section listing every check, and a
  `contradicted-claim` finding for each contradicted statement in the answer
  last shown.
- **The SDK**: `Agent.Verdict()`.

The labels follow the reply: in the terminal and the workbench the reply
streams as it is written, and the checks run once it is complete. The reply's
own text is not rewritten; the banner, the strikethrough in the list under it
and, where one follows, the corrected answer are what mark it.

## The critic

```json
"verify": {
  "mode": "label",
  "critic": {"enabled": true, "model": "reviewer", "timeout_ms": 30000}
}
```

Off by default. With it on, a model reads each verified statement beside the
tool output it was verified on, and nothing else, and says whether that output
supports it. It can only take a verdict down: a statement it does not find
supported becomes unverified, one it finds contradicted becomes contradicted.
It never raises one, so tool output that tries to talk it into agreeing can at
worst fail to lower a verdict, and the plain checks still stand underneath. If
it cannot be reached, times out, or answers in a form that cannot be read, every
statement it was asked about becomes unverified, and the verdict says the critic
was unavailable; the same happens, with no call, when the session's token
budget is spent. It costs one model call per answer with a verified statement,
recorded as a `model.call` with `purpose: critic`. `model` names a configured
provider, ideally a different model family from the agent's; unset, the
session's own model is asked. A workspace file cannot turn the critic on or
choose its model.

## The completion contract

```json
"verify": {
  "contract": {
    "require": ["go build ./...", "go test ./..."],
    "protect": ["*_test.go", ".github/**"],
    "timeout_ms": 600000
  }
}
```

A contract is what must hold before a run may end `completed`. The operator
declares it in configuration, which the agent cannot read or change: the
`.abhed` directory is Abhed's own state, refused to every tool in every mode.

- **`require`** are commands the **harness** runs, in the session's sandbox,
  after the agent's last change. Each must exit 0. They are recorded as calls
  by the system with `via: "completion contract"`, approved `by: operator`,
  and a pass is reused while nothing has changed since. They are not counted
  as changes, so what was read before them stays current. Policy still judges each one: a deny rule, plan
  mode, a destructive command, an ask rule, a screen or a hook, or a managed
  policy that asks, stops it, and the contract fails rather than run it
  unasked. Only an ordinary default-mode ask is taken as answered by the
  operator's declaration, as an allow rule of theirs would be.
  The commands are the operator's, not the agent's: the supervisor, which
  reviews the agent's calls, does not review them, and in a session bound to
  a [runbook](28-runbooks.md) they are not runbook steps. They neither use a
  step nor are recorded as `plan.step`, and the runbook's rule that any other
  change asks binds the agent's calls, not these.
- **`protect`** are globs of files the run must not change: `*` stays in one
  folder, `**` crosses folders, and a pattern without a slash matches a name
  at any depth. The files are hashed (up to 5,000) when the run starts and
  again when it ends; any change, addition or removal marks the run unverified.
  Globs that match more than 5,000 files leave the contract unmet, with
  `protect_partial`, since the rest could not be checked.
- **Claims**: with a contract, every statement about the session's work must
  also be verified.

A run with a contract ends `completed` only when all of this holds. Otherwise
the agent gets its one revision turn (with the failing command's output in the
note), and then the run ends `unverified`, exit 8, whatever the mode. A contract
works with `mode: off` too: then only the contract is checked. It gates an
answer: a run that ends on a plan proposed for approval or a structured result
runs neither the checks nor the contract (`abhed -p -json-schema` says so on
stderr when a contract is configured), and plan mode, which changes nothing,
runs no contract. A reply with nothing in it has no claims to check, but the
contract still runs and still decides how the run ends. `abhed serve` refuses to start, and a
session to open, when its answers cannot be checked as configured (a critic
model that is not there, say).

An untrusted workspace may add `protect` patterns but not `require` commands,
which are processes the harness runs; a trusted one may set both.

## What is recorded

Each check is an `answer.verdict` event: the mode, the source (`answer_tool` or
`text`), each claim with its verdict, reason, the `#seq` of its evidence, what
was compared, and for a contradicted claim the conflicting entry and an
excerpt; the counts; the contract's results; whether the critic checked; the
revision note when one was sent; and the outcome, `verified` only when every
statement about the session's work was verified and any contract was met. A
rebuilt or forked conversation includes the revision note, as the model saw it.

## Measuring it

`go test ./internal/agent -run TestFalseClaimCorpus -v` runs the false-claim
corpus: answers a weak model might give, each against a small recorded
session. 46 plant a wrong claim: a test count or result, a replica count,
port, version, host, path, disk figure, file change, command, `file:line` or
citation; among them a change claimed for a file a quoted `=>` or `->` only
grepped, "tests pass" after only `ls tests` ran or with an edit later on the
same line, and a read relied on after `env sed -i`, `yq -i`, `awk -i
inplace`, `uniq` in out, `tree -o`, `ruff --fix`, `jest -u`, `npm run lint --
--fix`, `sort -o`, `make` or a `sed -i` later on its own line. 32 controls say
only what the session showed, among them files written by a heredoc, `sed -i`
after a `cd` on the line or in an earlier call, `sed --expression= -i`,
`gofmt -w`, `echo x>>f`, `mv x dir/` and `cp -t`, absolute and relative paths
for the same file, and tests run under `timeout`, `npx` or `uv run`. 11 are
neutral: general knowledge, and true change claims after a script, `git
apply`, `git checkout <branch>`, `yq -i`, `eslint --fix`, `golangci-lint
--fix` or a folder copy, whose files cannot be read from the call. Today none
of the 46 planted claims is shown as verified (18 are contradicted, the rest
unverified), all 32 controls are verified, and none of the 43 true or neutral
claims is contradicted. The test fails if a planted claim is shown as
verified, if a true one is contradicted, or if either count falls below its
floor; the floors only go up. The corpus is small and written by the people
who wrote the checks, so it shows what the checks do on these shapes of
mistake, not how often a given model makes them.

## Limits

- The comparison is textual. A statement can be verified by output that says
  the same words for another reason, and a correct statement put in other words
  ("twelve" for `12`) is unverified. Quote the output, or use the answer tool's
  `quote`, where it matters.
- A subagent's work is placed at its return, so a parent's change made while
  a background subagent was still running may not make its output out of date.
- Remote state (a cluster, a host over ssh) is only as current as the last
  command that read it; any later call that may change state outside a file
  makes that output out of date.
- Labels do not stop a reply from streaming; the banner follows it, and
  strict mode's exit code is the gate for a script.
