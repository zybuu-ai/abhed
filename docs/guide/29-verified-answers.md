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
| ✗ | **contradicted** | the result it cites says otherwise. It is shown struck through, beside that result, never as fact |

"Verified" is a narrow word here. It means the statement matches what this
session observed, not that the world agrees: a test suite that passes can still
test the wrong thing, and a file the agent read can be wrong. What Abhed
guarantees is the other direction: nothing is shown as verified unless it is
tied to evidence from this session that a check could compare it with, and a
statement the result it cites contradicts is never presented as fact. A
statement that cites nothing is never marked contradicted: the checks say
"unverified" whenever they cannot stand behind either answer. The checks are
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
- **The evidence must still be current.** A file read is out of date once a
  later call may have changed that file; a command's output is out of date
  after any change at all. A shell line is read with the parser policy uses,
  quoting and redirections as the shell reads them, and each simple command
  in it is judged on its own, with runners and launchers (`env`, `time`,
  `sudo`, `nice`, `nohup`, `exec`, `timeout`, `npx`, `uv run`, `poetry run`,
  `pnpm exec` and the like) taken off first unless given an option. A command
  counts as changing nothing only when it is on a short list of readers:
  programs with no option that writes a file or runs another (`cat`, `head`,
  `tail`, `wc`, `ls`, `stat`, `grep`, `diff`, `jq`, `cut`, `tr`, `echo`,
  `pwd` and a few more); `sed -n` whose script only prints (`p`, `=`, `l`);
  `sort` and `uniq` without an output file; `rg` without `--pre` or `-z`;
  `git status`, `diff`, `log`, `show` and `rev-parse`; `find` without
  `-delete`, `-exec` or `-fprint`; and `go test` and `go vet` with flags that
  write nothing. Where a program takes long options, only the ones spelled
  out in full are let through, since GNU and git programs take abbreviations.
  A command with an assignment before it (`UPDATE=1 go test`) is never a
  reader. Everything else, test runners, linters, build tools, `curl` and `xargs`
  included, is a possible change whose files are unknown, and nothing is
  redirected to a file. Within one line, output printed before a later command
  on the same line changes something is out of date by that change: `cat f &&
  sed -i … f` does not show the file as it is.
- **Each clause is judged on its own.** A statement is cut into clauses
  at "and", "then", ",", ";", ":" and "—" wherever each side has a verb
  of its own, and is verified only when every clause that says something
  about the work is. "I read `sum.go` and inserted `continue`" needs the
  read and a change; "Fixed `Sum`, and the tests pass" needs a change and
  a passing run. A look chained to a look ("searched for `API_KEY` and
  found it") stays one clause. A clause can only take a verified
  statement down to unverified, never contradict it.
- **A lead-in never drops a claim.** A neutral lead ("In summary,",
  "Summary:", "Note:", "To recap,", "Results:", "The plan:") is taken off,
  and the rest is checked. A lead that reports work ("Done:", "Update:",
  "Changes:", "Here's what I changed:", "What I did:") stays: it says
  something was done, and needs a change that shows it. Left out are a
  question, a lead line with nothing after it, a courtesy that says
  nothing about the work, a bare sign-off ("Done.", "All done!"; a
  contract, when there is one, is what shows the work is done), and
  advice or an intention by its form ("run `go test ./...` to confirm",
  "you can run …", "I'll update the docs next", "next, …", "consider
  …"), unless it also says something already happened. A sentence that
  opens with a bare change verb ("Set `DEBUG` to `true` in
  `config/app.env`") is a deed in a session that changed something, and
  advice, not counted as work, in one that changed nothing.
- **What the statement says must be in the output.** Its quote, and every
  path, host, version, date and number in it, must appear in the evidence,
  compared without case, terminal colours or extra spaces. What is missing
  leaves it unverified ("`auth.go` is not in the cited #12"), never
  contradicted: output that is silent on a thing does not say otherwise. A
  `write` or `edit` result is no evidence for a path: it reports only the
  file the call was given, and whether that file changed is the "I changed
  X" rule's to say. And a statement about a file this session changed (a
  write, an edit, or a shell line naming it), other than that it was
  changed, is never verified by output naming the file: "`main.go` is
  exactly as before" after an edit of it stays unverified whatever `ls`
  shows. A call whose files the checks cannot tell (`python fix.py`) counts
  as changing every file in the workspace this way.
- **A count is compared by what it says of its unit.** The words beside a
  count that name an outcome (passed, failed, skipped), an action (added,
  changed, fixed, removed and the like) or a state (ready, down, running,
  healthy and the like) come from a fixed list. A count that names one
  ("12 tests passed", "3 replicas are ready", "5 nodes are down") is
  verified only by a count of the same unit that names it too, so
  `replicas: 3` does not show "3 replicas are ready", and `Ran 12 tests`
  does not show "12 tests failed". A plain count ("3 replicas") is verified
  by any count of the unit with its number. A count the output gives in
  other words leaves the statement unverified. Two shapes never carry the
  words after them: the whole of "N of M", a count after a `/` with any
  spacing or with "of" anywhere before it in its clause ("0 of 3 updated
  replicas are available", `0/3 nodes are available`, `1 / 3 replicas are
  ready`, "2 out of 5", "2 of all the 5", "2 out of a total of 5"; also
  `status of the rollout: 3 replicas are ready`, which the checks cannot
  tell from a part), and a count after a negation (not, no, none, never, without,
  isn't, don't and the like) earlier in its clause, the text since the last
  `;` or `,` on its line ("not ready: 3 replicas"; but `no errors; 3
  replicas are ready` shows "3 replicas are ready"). "12 of 12", "12 out of
  12", "12 of the 12" and `12/12` are all of them, not a part. A statement that counts a part of a whole
  ("3 of 5 nodes are down") is not verified by a count of the whole.
- **Any pass said of tests needs a run that passed.** A statement that
  tests passed, in any wording, a count of passes included ("9 tests
  passed"), is checked as "tests pass" as well as a count: it is verified
  only when a whole test run, read as the rule below says, exited 0 after
  the last change. Against a run that failed it is never verified, even
  when the pass count is right: "9 tests passed" with `3 failed, 9 passed`
  from a run that exited 1 stays unverified, and so does "9 tests passed
  and 3 tests failed", which reports the failure too. This holds whatever
  else the statement says: "Fixed the failing test; all 12 tests pass now"
  is held to the run like any other. That costs true statements, and is on
  purpose: a reader takes "9 tests passed" from an agent that just fixed
  something to mean the suite is green. It is contradicted only when it
  cites the failed run and says the run passed: with "all", "every" or
  "each" before the count or within it ("All 12 tests passed", "12 tests
  all passed"), with words after it that refer back to the tests (", all
  of them", "- every one", "(each of the 12)"), as "12/12", "12 of 12", "12
  out of 12" or "12 of the 12", or of the tests themselves ("the tests
  pass"), and does not also say not or never. "9 tests passed in all",
  "after all" or ", each in under a second" are counts, not claims the run
  passed. A bare `9 passed` or `2 failed` counts tests only in the output
  of a line that runs tests (`pytest`, `go test`, `npm test` and the like),
  not a linter's.
- **Contradicted only from what it cites, and only by evidence that
  conflicts.** A statement that cites a result (a `call_id` or `#seq`
  through the answer tool, or `(#12)` in the reply's text) is contradicted
  when that result, still current, counts the same unit with another number
  and the same outcome, action or state word ("15 tests passed" citing
  `12 tests passed` is ✗, with the result shown beside it), unless words
  after the statement's count narrow it ("3 tests passed in the auth
  package" is never contradicted by a count of the whole run); when it
  gives a key another value than the cited result does and its own value
  is nowhere in that result ("port 9090" citing `port: 8080`; but "Redis is
  on port 6379" citing `PORT=8080` beside `redis://cache:6379` is not).
  A statement that gives one unit or key more than one value, as a
  before and an after ("3 tests failed before the fix, 0 after", "from
  port 8080 to port 9090"), is not contradicted when the result shows one
  of them, where the values say the same of the unit: "9 tests passed and
  2 tests failed" citing `3 failed, 9 passed` is still ✗, since 9 passed
  says nothing of how many failed;
  when it says a file was not changed, or that the agent avoided or kept it,
  and cites a write or edit of that file that succeeded; or when it says
  tests pass (or a build succeeds) and cites a failed run of the same kind
  and the same scope, run on its own (no runner around it, exit 1). Nothing
  else is ever contradicted: a statement that cites nothing, however wrong
  it looks against some result, is unverified.
- **A statement that something did not happen is never verified.** One that
  says not, never, no, without, unchanged, untouched, left alone, avoided,
  kept as it was and the like is not shown by a result that shows the
  thing. The cost is that some true statements are never verified either:
  "no test was skipped and all tests pass" says "no", so it stays
  unverified even after a clean run, where "all tests pass" alone is
  verified. "No failures", "zero failures", "no errors" and "without
  errors" are read as part of a pass, not as a negation.
- **A state, an outcome or a judgement needs output that says it.** A
  statement that gives how something stands (healthy, broken, ready, is
  down and the like), what came about (fixed, resolved, merged, shipped,
  live, reviewed, approved, complete and the like) or a judgement (correct,
  fine, safe, harmless, a placeholder, a sandbox key and the like) is
  verified only when a result it matched says that word too: "the service
  is healthy because the config sets `replicas: 3`" and "`API_KEY` is a
  placeholder" stay unverified, whatever the result shows.
- **An event is shown only by the calls.** Output (a read, a run's text)
  can verify only a sentence that describes content as it stands. A
  sentence that reports an event is shown only by the record's calls,
  whatever its wording or person: one with, outside its code spans, a verb
  in the past or a past participle (any word in -ed, or an irregular form
  such as wrote, made, done or found), "now", "new", "no longer", "any
  more", or a change named as a thing (change, edit, diff, patch, fix,
  commit, PR, refactor, hotfix, update, post-change). "Added `continue`",
  "`Sum` now skips negatives", "the agent rewrote `Sum`" and "the diff
  adds `continue`" are events. The calls that can show one are a change
  (the next rule), a command run, a whole test or build run, and a read or
  a search for what the agent says it looked at ("I read `sum.go`", "I
  searched for `API_KEY` and found it in `config/app.env`"). Each kind of
  event needs a call of its own kind: a pass a run ("the tests pass now"
  included; "skipped" or "failed" is a run's only in a sentence about
  tests, a build or a command, so "negatives are skipped" is not), a look a look, any other event a change ("all tests pass now
  that `continue` is in `Sum`" needs both); a look never shows a change in
  the same sentence.
  A look is verified when every path it names has a read, a listing or a
  search of its own (the same path, or one ending in it at a slash, never
  a part of a name), a code span it says was searched for is in a
  search's pattern, what it says was found is in that search's result,
  and a search said to be of the whole repository was of all of it. "I
  read `calc`" names no file, and is not shown by a read that mentions
  it. A check or a release (tested, verified, reviewed, deployed,
  shipped, merged and the like) is never shown. Three things are left out
  before a sentence is read as an event:
  - a count with its own outcome ("3 tests failed", "3 files changed"),
    which the count check compares with the output's;
  - a tool's output said as such ("pytest reported 2 skipped", "the suite
    ran 12 tests");
  - a short allowlist of participles that describe code as it stands: "is
    defined", "declared", "called", "named", "used", "based", "typed",
    "exported", "embedded", "ordered",
    "sorted", "keyed", "scoped", "bounded", "nested", "shared",
    "required", "expected", "allowed", "supported", "meant" or "known",
    and "built-in", "hard-coded", "so-called", "well-known" and
    "zero-based".

  The cost is on purpose: some sentences about code as it stands with
  another participle ("negative numbers are skipped") are events, and stay
  unverified.
- **Explaining what a read showed is shown apart, and only shown.** A
  sentence the checks take as an explanation is drawn in its own group,
  marked "explanation", and left out of the unverified-work count; it
  keeps its place in everything else. The answer's outcome, strict mode's
  revision and the contract treat it exactly as they would without the
  group, so a sentence wrongly taken as an explanation can at worst be
  drawn apart, never make an answer verified. A sentence is taken as one
  only when all of these hold:
  - the session has a current read or search (`read`, `grep`, `glob`,
    `ls`, `web_fetch`, `web_search`), and what the sentence cites is such a
    result, every one of them: a run, a change, an edit's own report or
    `kubectl` output never counts as what it explains;
  - it reads as an explanation: an analogy, a definition, a reason, a walk
    through code ("like", "think of", "means", "because", "this line",
    "here is how", "we need to check if" and the like);
  - it says no deed, count, success, state or outcome, and has no word of
    judgement, secrecy or security (issue, bug, safe, harmless, secret,
    sensitive, exposed, bypass, credential, tests and the like); these are
    lists, so this guard can miss a word, which the group being display
    only makes harmless;
  - any negation in it is on a short allowlist: "not ... yet" (but not
    with a change verb: "`Sum` has not been touched yet" stays work), and
    code that does not run, return, loop, call, block, panic or the like,
    or is never nil; every other negation (of a change, the work or how
    something stands) keeps it work;
  - it reports no event, but for participles that say what code does
    (skipped, called, ignored, stored, returned, passed, given, used and
    the like);
  - no code it names is on a result line that sets or shows a credential;
  - every piece of code, number and other literal it names is in the read
    it cites, or, citing none, in one current read or search (a read's own
    path counts as shown); and a sentence that names no code at all
    qualifies only as a definition or an analogy that shares a word with
    what was read.

  "Think of `total` as a magic notebook" and "before we start, the
  running total is 0 because nothing has been added yet" after a read of
  the function are explanations. "`Sum` stays as it was because `total`
  starts at 0" after an edit, "`STRIPE_KEY` is not sensitive because
  `sk_prod_9f8e7d` is a sandbox key" after a grep, and "login works now
  because the token check runs first" citing a failed run all stay work.
- **"Tests pass" and "the build succeeds" need a whole run.** Such a
  statement is verified by a run of its own kind (read from the program and
  its subcommand: `go test`, `npm test`, `pytest`, `cargo test` for tests,
  `go build`, `cargo build`, `npm run build`, `make` for a build; a test run
  never shows a build, and `go vet` or a linter shows neither) that:
  - is the whole line, or the last command of an `&&` chain at its top with
    only `cd` before it, so its exit is the line's: not in a pipe, and not
    followed by `;`, `||` or `&`;
  - ran in this session's workspace (the folder the record holds for it),
    not over `ssh` and not in a subagent;
  - filters, lists and dry-runs nothing (`-run`, `-skip`, `-short`, `-list`,
    `-c`, `pytest -k` or `--collect-only`, `cargo test --no-run` and anything
    else not read as a plain run leave it out);
  - is given no variable but `NO_COLOR`, `FORCE_COLOR`, `CLICOLOR` or
    `CLICOLOR_FORCE`, which only colour its output: any other
    (`GOFLAGS=-run=TestA`, `PYTEST_ADDOPTS='-k x'`, `CI=true`), before it or
    through `env`, may filter it or change what passes. Runners that load
    variables from a file are not read either: `pipenv run` (the project's
    `.env`), `hatch run` (its environment's variables), and `uv run
    --env-file`, `dotenv` and `direnv exec`, which are not taken off, so the
    run is not one of a known kind;
  - covers, by path, the packages or folders the statement names, or the
    whole workspace (`./...` from the workspace) when it names none;
  - exited 0 after the last call that may have changed anything.
- **"I changed X" is checked against the calls.** It is verified by the last
  call that changed X, as the statement writes it, when that call succeeded:
  a `write` or `edit` of X, or a shell line in this session's workspace read
  in full as known writers that exited 0, whose destination is X (a
  redirection, `tee`, the destination of `cp`, `ln` or `install`, `sed -i`,
  `rm`, `touch`, `mkdir` for the folder itself, or a file beneath a folder
  `gofmt -w` or `goimports -w` rewrote). A file is resolved against the
  folder the bash tool ran the line in, as the record holds it, and a `cd`
  earlier in its own `&&` chain. A statement that names a folder is met by a
  change beneath it. The path must match as written, letter case included. A
  later call naming X that failed or was refused leaves it unverified, and a
  change over `ssh` or in a subagent's worktree never shows a change to the
  workspace. A change that names code ("I added `continue`", "the patch
  means ...", "`Sum` was rewritten", "I updated `sum.go` so that `Sum`
  skips negatives") is verified only by an edit, a write or `sed -i` of a
  file (of the file it names, when it names one), or a command that
  changes a resource it names (`kubectl apply`, `patch`, `scale`, `set`,
  `edit`, `delete`, `create`, `replace`, `annotate`, `label`, `autoscale`,
  `expose` or `rollout restart` or `undo`, the verb read as the first word
  after `kubectl` that is not a flag; `helm upgrade`, `install`,
  `uninstall` or `rollback`; `terraform apply`, `destroy` or `import`).
  It is read the way the sentence says, against the change's own changed
  text: an edit's old and new text, `sed -i`'s expression, or the lines a
  write changed from what the session last read or wrote of the file.
  Comment lines in it are left out: a comment that names code adds none.
  An addition needs the code in the new text and not the old, and a
  removal the reverse; then the other code the sentence names (the
  function it was done in) may be in the file as it stands after the
  change. Any other change (set, changed, updated, renamed, moved) needs
  its code in the new text, every piece of it: code only in the old text
  counts only when the sentence names it as what was replaced ("from
  `X`", "replaced `X` with", "renamed `X` to", "instead of `X`"), and code
  named as the place ("in `Sum`") may be in the file after the change. So
  "I set `DEBUG` to `false`" after `DEBUG=false` became `DEBUG=true`, or
  "I put `/admin` behind `requireAuth`" after `requireAuth` was taken off
  it, stays unverified; "enabled", "disabled", "turned on" and "turned
  off" need the matching value in the new text. The tool a change was
  made with ("with `sed`") is not code it must hold. A write that created
  its file ("Created …") added all of it, but a test file shows an
  addition only to a sentence about tests; any other write of a file the
  session had not read or written says nothing of what it added or
  removed, only what the new file holds. A
  count or a number the sentence gives for the change ("scaled `checkout`
  to 3 replicas") is read from what the change was given
  (`--replicas=3`), never from output after it. `kind/name`
  (`deploy/checkout`) is a resource, not a file. The arguments are decoded
  first, so a tab or a newline before the code still matches. A note to
  memory, a commit message, a formatter, any other shell writer and every
  other tool never count. Otherwise it is unverified, never contradicted.
- **"I ran `Y`" is checked against the commands.** It is verified when Y, as
  written (assignments and the program's path included), perhaps with more
  arguments after it, is a line's first command, or any command of an `&&`
  chain whose exit was 0. A command after `;`, `||` or a failed `&&` may not
  have run, and does not count. Otherwise it is unverified.
- **A subagent's work counts as evidence.** The parent's record names each
  subagent's session, and its own record is read too (up to three levels
  deep): its results are evidence and its calls are changes, placed at the
  parent's entry for its return. Its runs do not show the workspace's tests
  passing, and its folder is not known.
- **`file:line` cites are checked against the file now.** Text a cite quotes
  must be on that line or the two either side; a cite the file does not hold
  now leaves the statement unverified.

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

When a statement comes out contradicted (from what it cites), in `label` or `strict` mode, or when
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
session. 452 plant a wrong claim. Some cite the result that shows them wrong
(a test, outcome or files-changed count with the same word, a runner's
summary, a failure count beside a pass count that is right, a key given
another value, a failed run, a claim in any wording that every test passed
against a run that failed, whatever else it says ("Fixed the failing test;
all 12 tests pass now", "All 12 tests passed with zero failures"), an edited
file said untouched, avoided or kept as it was) and come out contradicted.
The rest cite nothing, or rest on what the checks cannot read, and must come
out unverified: wrong counts and paths, a count said with another outcome, a
state ("3 replicas are ready", "5 nodes are down", "12 tests failed")
against a plain count, the whole of "N of M" (`0/3 nodes are available`, `0
/ 3`, `1/3`, `1 / 3`, "2 of all 5", "2 of all the 5", "2 out of a total of
5") or a count after "not" read as the state after it, a part claimed of a
whole, a count said of every test ("All 12 tests passed", "12/12", "12 out
of 12", "12 of the 12", "12 tests all passed", "12 tests passed, every one",
"All 9 tests are passing") against a run that failed, a linter's bare "9
passed" read as tests, a state, an outcome or a deed given with a reason
("the service is healthy because ...", "we deployed v2.4.1 because ...",
"I've verified `Sum` because ...", "the hotfix is live"), an analogy naming
code the result lacks, a change said the other way round (a flag set to the
value it was changed from, auth said added where it was taken off, a rename
said backwards), a lead that reports work ("Done:", "Here's what I
changed:") over a plain state, a comment said as code added, a search of one
folder said to be of the whole repository, a read that names no file, a look
joined to a change ("I read `sum.go` and inserted `continue`", "I grepped
for `API_KEY` and deleted it"), more files said read than were, a write that
kept what was there or a new test file said to add to the code, a named
file's edit that did not touch the code named, a count said of a change but
read from later output, a change beside a test pass, a security state
("protected by `requireAuth`"), a change said in the present ("`Sum` gets a
`continue`"), an event in any wording or person that no call shows ("Added
`continue`", "I deliberately added", "I’ve added", "the agent rewrote
`Sum`", "`Sum` now skips negatives", "the diff adds `continue`", "Verified
that `Sum` skips ..."), a change matched only by another file's edit, an
edit that kept what is said removed, an argument's key or value, a memory
note, a commit message or a formatter, a change, a deed or a change noun
that names no file and that no call shows ("I added `continue`", "we set
`total` to 0", "`Sum` was rewritten", "my change to `Sum` means ..."), a
negation of a change or a judgement of a secret dressed as an explanation
("`Sum` stays as it was because ...", "nobody touched `Sum`", "`STRIPE_KEY`
is not sensitive because ..."), a judgement the result does not say
("`API_KEY` is a placeholder", "`Sum` is correct"), a false claim about the
work dressed as an explanation (a file said not changed after an edit, a fix
or a recovery explained from a failed run or `kubectl`, a leaked key called
harmless after a grep, an outcome explained in a session with nothing read),
a file this session or a script changed said to be as before, preserved or
fine, runs piped or followed by `;` or `|| true`, runs that filter, list or
only compile, runs given `GOFLAGS`, `PYTEST_ADDOPTS` or `CI`, or run through
`pipenv`, `hatch`, `uv run --env-file`, `dotenv` or `direnv`, a package run
against a claim about the whole suite, a trap before the run, commands after
a failed `&&`, `||` or `exit`, a copy's or a link's source, a write refused
in a folder `mkdir` made, a remote `rm`, a program at another path, a
success of no named kind, a path read from an edit's own report, reads
relied on after changes the checks cannot rule out, and more. 83 controls
say only what the session showed, among them a folder claim met by an edit
in it, `sed -l` or `perl -I` with their values, a run given only `NO_COLOR`,
a files-changed count, failed counts from a run that failed, "12 of 12 tests
passed", "12 tests passed, all of them" and "all 12 tests pass" after a fix
from a run that passed, "all tests pass with no failures" and "with zero
failures", and a state after another clause's "no", what the agent read,
opened, searched for and found, shown by its reads and searches, an addition
read from an edit's escaped text, a scale read from its `--replicas`, `sed
-i` read as an edit with the tool named, a new file's content and a test
added in it, a flag turned the way it was, auth removed and said so, a
rename said the right way, and a true addition named with the function it
was made in ("I added `continue` to `Sum`"). 86 are neutral: general
knowledge, and true claims the checks cannot stand behind, such as a build
claim against a failed test run, a filtered or timed-out run, a run of
another or a wider scope, a count of the same thing in other words ("added 3
tests" citing `42 tests passed`, "fixed 2 errors" citing `Found 5 errors`),
a narrowed count against a whole run, a port the cited result also holds, a
value given before and after a fix, a right count of passes from a run that
failed, with or without its failures, a count "in all", "after all" or "each
in under a second", a pass beside another "no", a state after an "of" in its
clause, a listing after a script, a `cd` after `CDPATH=`, and things a cited
result is silent on (no panic lines in a log tail, a port not listening, a
file not in a response). Today none of the 452 planted claims is shown as
verified (30 are contradicted, all from cited, conflicting evidence; the
rest unverified), all 83 controls are verified, and none of the 169 true or
neutral claims is contradicted. 19 of the neutral ones are a line-by-line
explanation of a function the session read, with an analogy; one of them
counts as unverified work, an analogy that shares no word with the read (5
did before the rule), and no planted claim is drawn as explanation; a second
test runs the corpus with the explanation group off and checks that every
verdict is the same, and a third runs strict mode on an answer whose only
unverified sentence is an explanation: it is still unverified, gets the
revision with that sentence in the note, and ends unverified. The test fails
if a planted claim is shown as verified, if a true one is contradicted, or
if either count falls below its floor; the floors rise, except where a rule
is made stricter on purpose (the controls floor fell from 53 to 50 when a
count of passes from a failed run stopped being verified; "fixed the failing
test; all 12 tests pass now" moved to neutral when each clause came to need
its own call). The corpus is small and written by the people who wrote the
checks, so it shows what the checks do on these shapes of mistake, not how
often a given model makes them.

## Limits

- The comparison is textual. A statement can be verified by output that says
  the same words for another reason, and a correct statement put in other words
  ("twelve" for `12`) is unverified. Quote the output, or use the answer tool's
  `quote`, where it matters. What code does is compared only by its names:
  "`Login` lets every `token` through because `check` returns nil" is
  verified when the read holds `Login`, `token`, `check` and `nil`.
- A subagent's work is placed at its return, so a parent's change made while
  a background subagent was still running may not make its output out of date.
- Remote state (a cluster, a host over ssh) is only as current as the last
  command that read it; any later call that may change state outside a file
  makes that output out of date.
- Labels do not stop a reply from streaming; the banner follows it, and
  strict mode's exit code is the gate for a script.
- The checks lean to "unverified": most shell work (test runners, builds,
  linters, scripts) counts as a possible change with unknown files, so
  what was read before it, and claims about files it may have changed, are
  left open. Use `write` and `edit` for changes the answer will claim, and
  run tests as the last command of their line.
