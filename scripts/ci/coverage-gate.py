#!/usr/bin/env python3
"""Fail when a change lowers statement coverage below the recorded floor.

A ratchet, not a target. The floor is what the tree already achieves, so a PR
cannot quietly remove tests, and raising it is a deliberate commit rather than
a number somebody has to argue for.

Usage: coverage-gate.py <coverage.out> [--update]
"""
import pathlib
import re
import subprocess
import sys

FLOOR = pathlib.Path(__file__).with_name("coverage-floor.txt")
# Coverage varies slightly between runs when a test races or a build tag
# changes what compiles; a small margin stops that flapping the build.
TOLERANCE = 0.5


def total(profile: str) -> float:
    out = subprocess.run(
        ["go", "tool", "cover", f"-func={profile}"],
        capture_output=True, text=True, check=True).stdout
    m = re.search(r"total:.*?([0-9.]+)%", out)
    if not m:
        sys.exit("could not read a total from the coverage profile")
    return float(m.group(1))


def main() -> int:
    if len(sys.argv) < 2:
        sys.exit("usage: coverage-gate.py <coverage.out> [--update]")
    now = total(sys.argv[1])

    if "--update" in sys.argv:
        FLOOR.write_text(f"{now:.1f}\n")
        print(f"coverage floor set to {now:.1f}%")
        return 0

    if not FLOOR.exists():
        sys.exit(f"no coverage floor recorded; run with --update to set it to {now:.1f}%")
    floor = float(FLOOR.read_text().strip())

    if now + TOLERANCE < floor:
        print(f"FAIL: coverage {now:.1f}% is below the {floor:.1f}% floor")
        print("Add tests for what this change touches, or lower the floor in")
        print("scripts/ci/coverage-floor.txt as a deliberate, reviewable commit.")
        return 1

    print(f"ok: coverage {now:.1f}% (floor {floor:.1f}%)")
    if now > floor + 1.0:
        print(f"note: the floor could be raised to {now:.1f}%")
    return 0


if __name__ == "__main__":
    sys.exit(main())
