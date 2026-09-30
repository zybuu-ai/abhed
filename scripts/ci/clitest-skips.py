#!/usr/bin/env python3
"""Fail when an end-to-end CLI test skipped for a reason CI does not accept.

A skipped test reads like a passing one, and the plan counts these tests as
evidence. Reads `go test -json` output on stdin. A skip is accepted only when
its reason is "clitest: pending (ITEM): ..." and ITEM is listed in
ABHED_CLITEST_PENDING, where the workflow documents why each item is still
open. Any other skip, a pending item that is not listed, or no test at all
fails the step.
"""
import json
import os
import re
import sys

allowed = set(re.split(r"[\s,]+", os.environ.get("ABHED_CLITEST_PENDING", "").strip())) - {""}
pending = re.compile(r"clitest: pending \(([^)]+)\):")

output, skipped, passed, failed = {}, [], 0, 0
for line in sys.stdin:
    try:
        ev = json.loads(line)
    except ValueError:
        continue
    test = ev.get("Test")
    if not test:
        continue
    key = (ev.get("Package", ""), test)
    if ev.get("Action") == "output":
        output.setdefault(key, []).append(ev.get("Output", ""))
    elif ev.get("Action") == "skip":
        skipped.append(key)
    elif ev.get("Action") == "pass":
        passed += 1
    elif ev.get("Action") == "fail":
        failed += 1

bad = []
for key in skipped:
    text = "".join(output.get(key, []))
    m = pending.search(text)
    if m and m.group(1) in allowed:
        continue
    lines = [l.strip() for l in text.splitlines() if l.strip() and not l.strip().startswith(("=== ", "--- "))]
    reason = lines[-1] if lines else "(no reason given)"
    bad.append(f"{key[0]} {key[1]}: {reason.strip()}")

if bad:
    print("clitest tests skipped for a reason CI does not accept:")
    for b in bad:
        print("  " + b)
    sys.exit(1)
if passed == 0:
    print("no clitest test ran")
    sys.exit(1)
print(f"{passed} clitest tests passed, {failed} failed, {len(skipped)} pending on {sorted(allowed) or 'nothing'}")
sys.exit(1 if failed else 0)
