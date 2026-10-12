#!/usr/bin/env python3
"""Fail when a workflow breaks one of two rules.

Pinned actions: a tag can be moved to other code, and the release jobs hold
the signing identity, so every `uses:` names a full 40-character commit with
the version in a comment: `uses: owner/repo@<sha> # v1.2.3`. A local action
(`./...`) or a container image pinned by digest (`docker://...@sha256:...`)
is accepted.

Self-hosted runners: a pull request can rewrite the workflow it runs, so a
job on a self-hosted runner runs only on a push to this repository: its `if:`
holds `github.event_name == 'push'` and `github.repository == 'zybuu-ai/abhed'`.

`--self-test` checks the rules on made-up input and exits.
"""
import re
import sys

USES = re.compile(r"^\s*(?:-\s*)?uses:\s*(\S+)(.*)$")
PINNED = re.compile(r"^[\w.-]+/[\w./-]+@[0-9a-f]{40}$")
VERSION = re.compile(r"^\s+#\s*v\d")
DIGEST = re.compile(r"^docker://\S+@sha256:[0-9a-f]{64}$")


def problem(line):
    """Returns why a workflow line's uses is not pinned, or None."""
    m = USES.match(line)
    if not m:
        return None
    ref, rest = m.group(1), m.group(2)
    if ref.startswith("./") or DIGEST.match(ref):
        return None
    if not PINNED.match(ref):
        return f"{ref} is not pinned to a full commit"
    if not VERSION.match(rest):
        return f"{ref} does not name its version in a comment"
    return None


JOB = re.compile(r"^  ([\w-]+):\s*$")
PUSH_ONLY = ("github.event_name == 'push'", "github.repository == 'zybuu-ai/abhed'")


def self_hosted_problems(lines):
    """Returns why each job on a self-hosted runner could run for a pull request."""
    jobs, name, in_jobs = {}, None, False
    for line in lines:
        if line.startswith("jobs:"):
            in_jobs = True
            continue
        if in_jobs and line and not line[0].isspace():
            in_jobs = False
        if not in_jobs:
            continue
        m = JOB.match(line)
        if m:
            name = m.group(1)
            jobs[name] = {"runs_on": "", "if": ""}
        elif name and line.startswith("    runs-on:"):
            jobs[name]["runs_on"] = line
        elif name and line.startswith("    if:"):
            jobs[name]["if"] = line
    out = []
    for name, job in jobs.items():
        if "self-hosted" in job["runs_on"] and not all(c in job["if"] for c in PUSH_ONLY):
            out.append(f"job {name} runs on a self-hosted runner but not only on a push to this repository")
    return out


def self_test():
    good = [
        "      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4.4.0",
        "        uses: ./.github/actions/local",
        "    steps:",
    ]
    bad = [
        "      - uses: actions/checkout@v4",
        "      - uses: actions/checkout@main",
        "      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262",
        "      - uses: actions/checkout@11d5960a # v4.4.0",
        "        uses: docker://alpine:3",
    ]
    for line in good:
        assert problem(line) is None, line
    for line in bad:
        assert problem(line) is not None, line
    ok = """jobs:
  a:
    runs-on: ubuntu-latest
  z:
    if: github.event_name == 'push' && github.repository == 'zybuu-ai/abhed' && vars.X == 'true'
    runs-on: [self-hosted, linux, s390x]
""".splitlines()
    assert self_hosted_problems(ok) == [], self_hosted_problems(ok)
    for cond in ["vars.X == 'true'", "github.repository == 'zybuu-ai/abhed'", "github.event_name == 'push'"]:
        bad_job = ["jobs:", "  z:", f"    if: {cond}", "    runs-on: [self-hosted, s390x]"]
        assert len(self_hosted_problems(bad_job)) == 1, cond
    print("self-test passed")


def main(paths):
    if paths == ["--self-test"]:
        self_test()
        return 0
    found = []
    for path in paths:
        with open(path, encoding="utf-8") as f:
            lines = f.read().splitlines()
        for n, line in enumerate(lines, 1):
            why = problem(line)
            if why:
                found.append(f"{path}:{n}: {why}")
        found += [f"{path}: {why}" for why in self_hosted_problems(lines)]
    for f in found:
        print(f)
    if not paths:
        print("no workflow files named")
        return 1
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
