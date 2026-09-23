# Contributing to Abhed

Abhed is maintained by a small team at Zybuu. That shapes everything below:
review latency is real, and a PR that makes review easy gets merged faster
than one that is merely correct.

## Building and testing

```bash
go build ./...
go test ./...
```

**Local quirk:** on the maintainer's machine, `GOROOT` is set to something Go
1.26 doesn't like, so commands here are actually run as:

```bash
env -u GOROOT go build ./...
env -u GOROOT go test ./...
```

You almost certainly don't have that environment variable set, so plain
`go test ./...` is what you should use and what CI runs. If `go build`/`go
test` fail in a way that looks like a toolchain/GOROOT problem rather than a
real bug, that's the mismatch — not something to work around in the code.

Some tests need more:

- `go test ./... -short` skips the slower network-exfiltration checks; drop
  `-short` to run them.
- Postgres integration tests need `ABHED_TEST_DSN` pointed at a real database
  (`store`).

## How this repository actually works

These aren't aspirational — they're enforced, in the sense that a PR
violating them gets asked to fix it before merge.

**Comments explain why, not what.** Read `store/schema.sql` or
`store/postgres.go` for the tone: a comment exists to record a
decision or a bug that was found and fixed (see the `FORCE ROW LEVEL
SECURITY` comment in `store/schema.sql`, or the comment in
`store/postgres.go` on why a superuser connection is refused). A
comment that restates the line below it is worse than no comment — it's
something else to go stale.

**A test must fail without the change.** Before you write the fix, write (or
run) the test that fails because the bug exists. If you can't make a test go
red on the old code, the PR needs a different kind of evidence that it does
something — a test that passes on old and new code alike hasn't tested
anything. This project's own history is full of bugs found exactly this way:
row-level security silently inert because a table owner bypasses it without
`FORCE`, an auth middleware ordered so it read identity before establishing
it, a bundle manifest that listed its own digest. Each was caught by a test
written to attack the thing, not confirm it.

**No claim in the docs without something that checks it.** If your PR adds
or changes a number in `README.md` or `docs/` — a test count, a provider
count, an attack count — or a compliance statement, name the test or command
that produces it, or it will be reverted. The project's whole argument is
that its numbers are checkable; that only holds if drift is caught instead of
shipping quietly.

**Secrets never go in the repo.** Not in a commit, not in a config file
checked in, not in a test fixture. If a change needs a credential, it should
come from an environment variable or a file outside the tree — follow the
pattern of `ABHED_DATABASE_URL` (`docs/ops/enabling-auth.md`) and the `*_env`
config keys (`api_key_env`, `password_env`, `headers_env`), which name the
variable holding a secret rather than the secret itself — not a new
hardcoded value.

**Errors are written for whoever reads them next**, which for a policy
rejection or a failed edit is the model, and for a startup failure is the
operator. Match the existing tone: name what was tried, what would fix it
(see the sandbox's refusal message in `internal/sandbox/sandbox.go` — it
names every backend it tried and why each failed).

## Sign-off (DCO)

Every commit must be signed off, certifying you wrote it or otherwise have
the right to submit it under this project's license (Apache 2.0):

```bash
git commit -s -m "your message"
```

That appends a `Signed-off-by: Your Name <you@example.com>` trailer using
your configured `user.name` and `user.email`. PRs with unsigned commits will
be asked to amend and re-push before merge.

## Sending a change

1. Open an issue first for anything that isn't a small, obvious fix — a
   small review queue means a discussion up front saves a rewritten PR
   later.
2. Keep the PR scoped to one thing. A PR that fixes a bug and reformats an
   unrelated file is two PRs' worth of review for the price of one diff
   that's hard to read.
3. Include the test that fails without your change, per above.
4. Expect review latency measured in days, not hours. A small team reads
   every PR against a codebase where the history above is typical of
   the bugs that get through when review is rushed. A ping after a week is
   fine; a ping after a day is not going to make it faster.

## Code of conduct

Participation in this project is governed by `CODE_OF_CONDUCT.md`.
