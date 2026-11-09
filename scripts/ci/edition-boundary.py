#!/usr/bin/env python3
"""Fail on any reference to the company's private deployment tree, deploy/.

Every deploy/ path in the open tree fails unless it is one of the named forms
below, so a new private entry is caught without being named here.
"""
import re
import subprocess
import sys

# The changelog and the scan record are history and may name the old tree;
# this script names the pattern and its probes.
EXCLUDED = [
    ":!CHANGELOG.md",
    ":!docs/trust/security-scans.md",
    ":!scripts/ci/edition-boundary.py",
]
# What follows the name is looked at, not taken, so a path inside it is seen too.
TOKEN = re.compile(r"(?:^|[^A-Za-z0-9_.-])deploy/([A-Za-z0-9_.-]*)(?=(\S*))")
# A project's own deploy/ in tests and the verifier's and monitor's corpora:
# its files, and the cluster resources a kubectl line names.
FIXTURES = {"app.yaml", "app.conf", "app.vars", "compose.vars", "values-prod.yaml", "vars",
            "api", "checkout", "db", "web", "postgres"}
FIXTURE_FILES = re.compile(r"(_test\.go$|(^|/)testdata/|^internal/agent/verify_(corpus|facts)\.go$|^monitor/drift\.go$)")


def allowed(path, line, name, rest):
    if name == "helm":
        return rest.startswith("/") and ".." not in rest  # the open Helm chart
    if path.startswith("deploy/helm/") and name == "" and rest.startswith("{{"):
        return True  # the chart's own kubectl hint: deploy/{{ name }}
    if not FIXTURE_FILES.search(path):
        return False
    bare = name.rstrip(".")  # a sentence's full stop
    if bare in FIXTURES and not rest.startswith("/"):
        return True
    # The corpus's lint run over a project's own deploy/ folder.
    return name == "" and "ruff check --fix deploy/" in line


def offending(path, line):
    return any(not allowed(path, line, m.group(1), m.group(2)) for m in TOKEN.finditer(line))


# Lines the check must refuse (True) or pass (False), in a file of each kind.
PROBES = [
    (True, "docs/a.md", "see deploy/.demo-secret"),
    (True, "docs/a.md", "deploy/.db-admin-password"),
    (True, "docs/a.md", "run deploy/newthing/x"),
    (True, "docs/a.md", "kubectl apply -f deploy/cloud"),
    (True, "docs/a.md", "kubectl apply -k deploy/cloud"),
    (True, "docs/a.md", "kubectl logs deploy/console-edge"),
    (True, "docs/a.md", "kubectl delete deploy/demo"),
    (True, "docs/a.md", "see deploy/sitegen, not kubectl"),
    (True, "docs/a.md", "kubectl scale deploy/api"),
    (True, "docs/a.md", "cat deploy/app.yaml"),
    (True, "docs/a.md", "deploy/helm/../run.sh"),
    (True, "deploy/helm/abhed/templates/x.yaml", "deploy/run.sh"),
    (True, "a_test.go", "kubectl logs deploy/console-edge"),
    (True, "a_test.go", "deploy/app.yaml/x"),
    (True, "a_test.go", "ruff check --fix deploy/ && cat deploy/run.sh"),
    (True, ".github/workflows/ci.yml", "run: deploy/run.sh"),
    (True, "a_test.go", "cat deploy/app.yaml&&deploy/run.sh"),
    (True, "docs/a.md", "deploy/helm/x/deploy/run.sh"),
    (False, "docs/a.md", "helm install abhed deploy/helm/abhed"),
    (False, ".github/workflows/ci.yml", "run: scripts/ci/helm-check.sh deploy/helm/abhed"),
    (False, "deploy/helm/abhed/templates/NOTES.txt", "kubectl logs deploy/{{ include \"abhed.fullname\" . }}"),
    (False, "a_test.go", "kubectl scale deploy/api"),
    (False, "internal/agent/verify_corpus.go", "cat deploy/app.yaml && sed -i x deploy/app.yaml"),
    (False, "internal/agent/verify_corpus.go", "The settings live in deploy/values-prod.yaml."),
    (False, "internal/agent/verify_corpus.go", "ruff check --fix deploy/"),
    (False, "docs/a.md", "redeploy/x and my.deploy/x"),
]


def self_test():
    bad = [(want, path, line) for want, path, line in PROBES if offending(path, line) != want]
    for want, path, line in bad:
        print(f"{'passed' if want else 'refused'}: {path}: {line}")
    return 1 if bad else 0


def main():
    if sys.argv[1:] == ["--self-test"]:
        return self_test()
    out = subprocess.run(
        ["git", "grep", "-n", "-I", "-E", r"(^|[^A-Za-z0-9_.-])deploy/", "--", "."] + EXCLUDED,
        capture_output=True, text=True, check=False,
    )
    if out.returncode not in (0, 1):
        print(out.stderr, file=sys.stderr)
        return 2
    bad = []
    for hit in out.stdout.splitlines():
        path, _, rest = hit.partition(":")
        line = rest.partition(":")[2]
        if offending(path, line):
            bad.append(hit)
    if bad:
        print("references to the private deployment tree:")
        print("\n".join(bad))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
