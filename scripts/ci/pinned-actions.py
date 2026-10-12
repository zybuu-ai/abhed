#!/usr/bin/env python3
"""Fail when a workflow uses an action by a tag or branch instead of a commit.

A tag can be moved to other code, and the release jobs hold the signing
identity, so every `uses:` names a full 40-character commit with the version
in a comment: `uses: owner/repo@<sha> # v1.2.3`. A local action (`./...`) or
a container image pinned by digest (`docker://...@sha256:...`) is accepted.

`--self-test` checks the rule on made-up lines and exits.
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
    print("self-test passed")


def main(paths):
    if paths == ["--self-test"]:
        self_test()
        return 0
    found = []
    for path in paths:
        with open(path, encoding="utf-8") as f:
            for n, line in enumerate(f, 1):
                why = problem(line)
                if why:
                    found.append(f"{path}:{n}: {why}")
    for f in found:
        print(f)
    if not paths:
        print("no workflow files named")
        return 1
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
