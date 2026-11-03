#!/usr/bin/env python3
"""Fail on any reference to the company's private deployment tree, deploy/.

Every deploy/ path in the open tree fails unless it is one of the known forms
below, so a new private entry is caught without being named here.
"""
import re
import subprocess
import sys

# The changelog and the scan record are history and may name the old tree;
# the workflow and this script name the pattern; deploy/helm/ is the open chart.
EXCLUDED = [
    ":!CHANGELOG.md",
    ":!docs/trust/security-scans.md",
    ":!.github/workflows/ci.yml",
    ":!scripts/ci/edition-boundary.py",
    ":!deploy/helm/",
]
TOKEN = re.compile(r"(?:^|[^A-Za-z0-9_.-])deploy/([A-Za-z0-9_.-]*)(/?)")
# Fixture paths in tests and the verifier's corpus: a project's own deploy/.
FIXTURES = {"app.yaml", "app.conf", "app.vars", "compose.vars", "values-prod.yaml", "vars", "checkout"}
# A cluster resource, kind/name, on a kubectl line: deploy/api, deploy/db.
RESOURCE = re.compile(r"[a-z0-9][a-z0-9-]*")


def allowed(line, name, slash):
    name = name.rstrip(".")  # a sentence's full stop
    if name == "helm":
        return True  # the open Helm chart
    if name in FIXTURES and not slash:
        return True
    if "kubectl" in line and not slash and RESOURCE.fullmatch(name):
        return True
    # The corpus's lint run over a project's own deploy/ folder.
    return name == "" and "ruff check --fix deploy/" in line


def main():
    out = subprocess.run(
        ["git", "grep", "-n", "-I", "-E", r"(^|[^A-Za-z0-9_.-])deploy/", "--", "."] + EXCLUDED,
        capture_output=True, text=True, check=False,
    )
    if out.returncode not in (0, 1):
        print(out.stderr, file=sys.stderr)
        return 2
    bad = []
    for hit in out.stdout.splitlines():
        line = hit.split(":", 2)[-1]
        if any(not allowed(line, m.group(1), m.group(2)) for m in TOKEN.finditer(line)):
            bad.append(hit)
    if bad:
        print("references to the private deployment tree:")
        print("\n".join(bad))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
