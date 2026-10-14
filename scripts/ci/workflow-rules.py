#!/usr/bin/env python3
"""Fail when a workflow breaks one of two rules.

Pinned actions: a tag can be moved to other code, and the release jobs hold
the signing identity, so every `uses:` names a full 40-character commit with
the version in a comment: `uses: owner/repo@<sha> # v1.2.3`. A local action
(`./...`) or a container image pinned by digest (`docker://...@sha256:...`)
is accepted.

Self-hosted runners: a pull request can rewrite the workflow it runs, so a
job that may run on a self-hosted runner runs only on a push to this
repository: its `if:` has `github.event_name == 'push'` and
`github.repository == 'zybuu-ai/abhed'` as top-level `&&` terms. Only a
GitHub-hosted label (`ubuntu-*`, `macos-*`, `windows-*`), given directly or
as a matrix list, counts as hosted; a runner group or other expression does not.

The workflow is read as YAML (PyYAML, pinned in workflow-rules.requirements.txt), and its
lines are also scanned, since the version comment is not part of the YAML.
`--self-test` checks the rules on made-up input and exits.
"""
import re
import sys

try:
    import yaml
except ImportError:
    sys.exit("workflow-rules.py needs PyYAML: pip install --require-hashes -r scripts/ci/workflow-rules.requirements.txt")

USES = re.compile(r"""(?:^|[\s{,-])uses:\s*["']?([^\s"',}]+)["']?(.*)$""")
PINNED = re.compile(r"^[\w.-]+/[\w./-]+@[0-9a-f]{40}$")
VERSION = re.compile(r"#\s*v\d")
DIGEST = re.compile(r"^docker://\S+@sha256:[0-9a-f]{64}$")
HOSTED = re.compile(r"^(ubuntu|macos|windows)-[\w.-]+$")
MATRIX = re.compile(r"^\$\{\{\s*matrix\.([\w-]+)\s*\}\}$")
PUSH_ONLY = ("github.event_name == 'push'", "github.repository == 'zybuu-ai/abhed'")


def ref_problem(ref):
    """Returns why a uses reference is not pinned, or None."""
    if ref.startswith("./") or DIGEST.match(ref):
        return None
    if not PINNED.match(ref):
        return f"{ref} is not pinned to a full commit"
    return None


def line_problem(line):
    """Returns why a workflow line's uses is not pinned or names no version."""
    m = USES.search(line)
    if not m:
        return None
    ref, rest = m.group(1), m.group(2)
    if ref.startswith("./") or DIGEST.match(ref):
        return None
    why = ref_problem(ref)
    if why:
        return why
    if not VERSION.search(rest):
        return f"{ref} does not name its version in a comment"
    return None


def all_uses(node):
    """Yields every value under a `uses` key anywhere in the parsed workflow."""
    if isinstance(node, dict):
        for k, v in node.items():
            if k == "uses":
                yield v
            else:
                yield from all_uses(v)
    elif isinstance(node, list):
        for v in node:
            yield from all_uses(v)


def runner_values(job):
    """Returns the runs-on values a job may take, or None when they cannot be known."""
    runs_on = job.get("runs-on")
    if runs_on is None:
        return []
    if not isinstance(runs_on, str):
        return [runs_on]
    m = MATRIX.match(runs_on.strip())
    if not m:
        return [runs_on]
    matrix = (job.get("strategy") or {}).get("matrix")
    if not isinstance(matrix, dict):
        return None
    values = matrix.get(m.group(1), [])
    if not isinstance(values, list):
        return None
    for inc in matrix.get("include") or []:
        if not isinstance(inc, dict):
            return None
        if m.group(1) in inc:
            values = values + [inc[m.group(1)]]
    return values


def hosted(value):
    return isinstance(value, str) and HOSTED.match(value) is not None


def top_level(expr, op):
    """Splits expr on op outside parentheses and quotes."""
    parts, depth, quote, start, i = [], 0, None, 0, 0
    while i < len(expr):
        c = expr[i]
        if quote:
            if c == quote:
                quote = None
        elif c in "'\"":
            quote = c
        elif c == "(":
            depth += 1
        elif c == ")":
            depth -= 1
        elif depth == 0 and expr.startswith(op, i):
            parts.append(expr[start:i])
            start = i + len(op)
            i += len(op)
            continue
        i += 1
    parts.append(expr[start:])
    return parts


def push_only(cond):
    """Reports whether an if: holds both push-only terms as top-level && terms."""
    if not isinstance(cond, str):
        return False
    expr = cond.strip()
    if expr.startswith("${{") and expr.endswith("}}"):
        expr = expr[3:-2]
    if len(top_level(expr, "||")) != 1:
        return False
    terms = {" ".join(t.split()) for t in top_level(expr, "&&")}
    return all(c in terms for c in PUSH_ONLY)


def self_hosted_problems(doc):
    """Returns why each job that may run self-hosted could run for a pull request."""
    out = []
    jobs = doc.get("jobs") if isinstance(doc, dict) else None
    for name, job in (jobs or {}).items():
        if not isinstance(job, dict):
            continue
        values = runner_values(job)
        if values is not None and all(hosted(v) for v in values):
            continue
        if not push_only(job.get("if")):
            out.append(f"job {name} may run on a self-hosted runner but not only on a push to this repository")
    return out


def check(text):
    """Returns every problem in one workflow's text."""
    found = []
    for n, line in enumerate(text.splitlines(), 1):
        why = line_problem(line)
        if why:
            found.append(f"{n}: {why}")
    doc = yaml.safe_load(text)
    for ref in all_uses(doc):
        why = ref_problem(str(ref))
        if why:
            found.append(why)
    return found + self_hosted_problems(doc)


def self_test():
    good = """jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4.4.0
      - uses: ./.github/actions/local
  m:
    runs-on: ${{ matrix.runner }}
    strategy:
      matrix:
        runner: [ubuntu-24.04, ubuntu-24.04-arm]
  z:
    if: github.event_name == 'push' && github.repository == 'zybuu-ai/abhed' && vars.X == 'true'
    runs-on: [self-hosted, linux, s390x]
"""
    assert check(good) == [], check(good)
    steps = "jobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n"
    for bad in [
        "      - uses: actions/checkout@v4\n",
        "      - uses: actions/checkout@main\n",
        "      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262\n",
        "      - uses: actions/checkout@11d5960a # v4.4.0\n",
        "      - uses: docker://alpine:3\n",
        "      - {uses: actions/checkout@v4, name: x}\n",
        "      - {name: x, uses: 'actions/checkout@v4'}\n",
        '      - uses: "actions/checkout@v4"\n',
        "      - name: x\n        uses: >-\n          actions/checkout@v4\n",
    ]:
        assert check(steps + bad), bad
    assert check("jobs:\n  r:\n    uses: org/repo/.github/workflows/x.yml@v1\n")
    gate = "github.event_name == 'push' && github.repository == 'zybuu-ai/abhed'"
    matrix = "    strategy:\n      matrix:\n        runner: "
    for runs_on, extra in [
        ("[self-hosted, s390x]", ""),
        ("\n      - self-hosted\n      - s390x", ""),
        ("\n      group: builders", ""),
        ("${{ matrix.runner }}", matrix + "[ubuntu-24.04, self-hosted]\n"),
        ("${{ matrix.runner }}", matrix + "[ubuntu-24.04]\n        include:\n          - runner: self-hosted\n"),
        ("${{ matrix.runner }}", "    strategy:\n      matrix: ${{ fromJSON(vars.M) }}\n"),
        ("${{ vars.RUNNER }}", ""),
    ]:
        for cond, ok in [
            ("vars.X == 'true'", False),
            ("github.repository == 'zybuu-ai/abhed'", False),
            ("github.event_name == 'push'", False),
            ("contains('github.event_name == ''push'' && github.repository == ''zybuu-ai/abhed''', 'x')", False),
            (f"{gate} || true", False),
            (f"always() || {gate}", False),
            ("(github.event_name == 'push' || true) && github.repository == 'zybuu-ai/abhed'", False),
            (gate, True),
            (f"${{{{ {gate} && vars.X == 'true' }}}}", True),
        ]:
            job = f"jobs:\n  z:\n    if: \"{cond}\"\n    runs-on: {runs_on}\n{extra}"
            got = self_hosted_problems(yaml.safe_load(job))
            assert (got == []) == ok, (runs_on, extra, cond, got)
    print("self-test passed")


def main(paths):
    if paths == ["--self-test"]:
        self_test()
        return 0
    if not paths:
        print("no workflow files named")
        return 1
    found = []
    for path in paths:
        with open(path, encoding="utf-8") as f:
            found += [f"{path}:{why}" if why[0].isdigit() else f"{path}: {why}" for why in check(f.read())]
    for f in found:
        print(f)
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
