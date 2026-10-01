#!/usr/bin/env python3
"""Fail when an end-to-end CLI test skipped for a reason CI does not accept.

A skipped test reads like a passing one, and the plan counts these tests as
evidence. Reads `go test -json` output on stdin. A skip is accepted only when
the test logged a line of exactly the form

    <file>.go:<line>: clitest: pending (ITEM): <why>

and ITEM is listed in ABHED_CLITEST_PENDING, where the workflow documents
why each item is still open. Any other skip, a pending item that is not
listed, a package that did not build or failed outside a test, a package
in which no test ran, or no test at all fails the step.

`--self-test` checks the rules above on made-up input and exits.
"""
import json
import os
import re
import sys

# One log line of a test, as the testing package writes it: indented, then
# file:line. The marker anywhere else, such as in the binary's output, does not count.
PENDING = re.compile(r"^ {4,}[\w./-]+\.go:\d+: clitest: pending \(([A-Za-z0-9]+)\): \S.*$")


def check(lines, allowed):
    """Returns (ok, message) for the go test -json lines."""
    output, skipped, failed = {}, [], 0
    passed = {}
    pkg_fail, pkg_ran = set(), set()
    for line in lines:
        try:
            ev = json.loads(line)
        except ValueError:
            continue
        action = ev.get("Action")
        if action == "build-fail":
            pkg_fail.add(ev.get("ImportPath", "?"))
            continue
        pkg = ev.get("Package", "")
        test = ev.get("Test")
        if not test:
            if action == "fail":
                pkg_fail.add(pkg)
            elif action in ("pass", "skip"):
                pkg_ran.add(pkg)
            continue
        key = (pkg, test)
        if action == "output":
            output.setdefault(key, []).append(ev.get("Output", ""))
        elif action == "skip":
            skipped.append(key)
        elif action == "pass":
            passed[pkg] = passed.get(pkg, 0) + 1
        elif action == "fail":
            failed += 1

    bad = []
    for pkg in sorted(pkg_fail):
        bad.append(f"{pkg}: the package did not build, or failed outside a test")
    for pkg in sorted(pkg_ran - pkg_fail):
        if not passed.get(pkg):
            bad.append(f"{pkg}: no test ran")
    for key in skipped:
        marks = [m.group(1) for m in (PENDING.match(l.rstrip("\n")) for l in output.get(key, [])) if m]
        if marks and all(m in allowed for m in marks):
            continue
        text = "".join(output.get(key, []))
        rest = [l.strip() for l in text.splitlines() if l.strip() and not l.strip().startswith(("=== ", "--- "))]
        reason = rest[-1] if rest else "(no reason given)"
        bad.append(f"{key[0]} {key[1]}: {reason}")

    total = sum(passed.values())
    if bad:
        return False, "clitest tests skipped or failed for a reason CI does not accept:\n" + "\n".join("  " + b for b in bad)
    if total == 0:
        return False, "no clitest test ran"
    msg = f"{total} clitest tests passed, {failed} failed, {len(skipped)} pending on {sorted(allowed) or 'nothing'}"
    return failed == 0, msg


def self_test():
    def ev(action, test=None, out=None, pkg="p"):
        d = {"Action": action, "Package": pkg}
        if test:
            d["Test"] = test
        if out is not None:
            d["Output"] = out
        return json.dumps(d)

    ok_run = [ev("pass", "TestA"), ev("output", "TestB", "    x_test.go:9: clitest: pending (A1): editor\n"),
              ev("skip", "TestB"), ev("pass")]
    cases = [
        ("a listed pending skip", ok_run, {"A1"}, True),
        ("an unlisted pending skip", ok_run, set(), False),
        ("the marker inside other output", [ev("pass", "TestA"),
            ev("output", "TestB", "        screen: clitest: pending (A1): x\n"), ev("skip", "TestB"), ev("pass")], {"A1"}, False),
        ("the marker mid-line", [ev("pass", "TestA"),
            ev("output", "TestB", "    x.go:1: got clitest: pending (A1): x\n"), ev("skip", "TestB"), ev("pass")], {"A1"}, False),
        ("a package that did not build", [ev("pass", "TestA", pkg="q"), ev("pass", pkg="q"),
            ev("output", out="p/x_test.go:1:1: undefined: y\n"), ev("fail")], {"A1"}, False),
        ("a build-fail event", [ev("pass", "TestA"), ev("pass"),
            json.dumps({"Action": "build-fail", "ImportPath": "q [q.test]"})], {"A1"}, False),
        ("a package where no test ran", [ev("pass", "TestA", pkg="q"), ev("pass", pkg="q"), ev("pass")], set(), False),
        ("nothing at all", [], set(), False),
    ]
    good = True
    for name, lines, allowed, want in cases:
        got, msg = check(lines, allowed)
        if got != want:
            good = False
            print(f"self-test: {name}: got {got}, want {want}\n{msg}")
    print("self-test passed" if good else "self-test FAILED")
    return 0 if good else 1


if __name__ == "__main__":
    if sys.argv[1:] == ["--self-test"]:
        sys.exit(self_test())
    allowed = set(re.split(r"[\s,]+", os.environ.get("ABHED_CLITEST_PENDING", "").strip())) - {""}
    ok, msg = check(sys.stdin, allowed)
    print(msg)
    sys.exit(0 if ok else 1)
