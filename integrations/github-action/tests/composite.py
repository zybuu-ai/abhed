"""Runs action.yml's composite steps locally, as a runner would, for tests.

Only what this action uses is supported: run steps in bash, the expression
forms its inputs and conditions use, step outputs and outcomes, GITHUB_PATH,
GITHUB_OUTPUT and GITHUB_STEP_SUMMARY. A `uses:` step is not run; it is
recorded with its inputs so a test can check what it was given. It needs
PyYAML (scripts/ci/workflow-rules.requirements.txt pins it).
"""

import json
import os
import re
import subprocess
import tempfile

import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
ACTION = os.path.dirname(HERE)


# --- expressions -----------------------------------------------------------

STATUS = re.compile(r"\b(success|failure|always|cancelled)\(\)")


def evaluate(expr, ctx, failed):
    """Evaluates a GitHub expression in the forms action.yml uses."""
    src = expr.strip()
    if src.startswith("${{") and src.endswith("}}"):
        src = src[3:-2].strip()
    if not STATUS.search(src):
        src = f"success() && ({src})"
    py = []
    i = 0
    tokens = re.findall(r"'(?:[^']|'')*'|&&|\|\||==|!=|!|\(|\)|[A-Za-z_][\w.-]*\(\)|[A-Za-z_][\w.-]*|\S", src)
    for t in tokens:
        if t == "&&":
            py.append(" and ")
        elif t == "||":
            py.append(" or ")
        elif t == "!":
            py.append(" not ")
        elif t in ("==", "!=", "(", ")"):
            py.append(f" {t} ")
        elif t.startswith("'"):
            py.append(repr(t[1:-1].replace("''", "'")))
        elif t == "success()":
            py.append(repr(not failed))
        elif t == "failure()":
            py.append(repr(failed))
        elif t == "always()":
            py.append("True")
        elif t == "cancelled()":
            py.append("False")
        elif t in ("true", "false"):
            py.append(t.capitalize())
        else:
            py.append(repr(lookup(t, ctx)))
        i += 1
    return eval("".join(py), {"__builtins__": {}})  # noqa: S307 - our own action file


def lookup(name, ctx):
    cur = ctx
    for part in name.split("."):
        if isinstance(cur, dict):
            cur = cur.get(part, "")
        else:
            return ""
    return cur if cur is not None else ""


def interpolate(value, ctx, failed=False):
    if not isinstance(value, str):
        return value

    def sub(m):
        v = evaluate("${{" + m.group(1) + "}}", ctx, failed) if STATUS.search(m.group(1)) else _value(m.group(1), ctx)
        return "" if v is None else str(v)

    return re.sub(r"\$\{\{(.*?)\}\}", sub, value)


def _value(expr, ctx):
    # An expression used for its value: a && 'x' || '' and plain lookups.
    src = expr.strip()
    tokens = re.findall(r"'(?:[^']|'')*'|&&|\|\||==|!=|!|\(|\)|[A-Za-z_][\w.-]*|\S", src)
    py = []
    for t in tokens:
        if t == "&&":
            py.append(" and ")
        elif t == "||":
            py.append(" or ")
        elif t == "!":
            py.append(" not ")
        elif t in ("==", "!=", "(", ")"):
            py.append(f" {t} ")
        elif t.startswith("'"):
            py.append(repr(t[1:-1]))
        else:
            py.append(repr(lookup(t, ctx)))
    return eval("".join(py), {"__builtins__": {}})  # noqa: S307


# --- the runner ------------------------------------------------------------


def read_outputs(path):
    out, lines, i = {}, open(path).read().split("\n") if os.path.exists(path) else [], 0
    while i < len(lines):
        l = lines[i]
        if "<<" in l and "=" not in l.split("<<", 1)[0]:
            k, d = l.split("<<", 1)
            j = lines.index(d, i + 1)
            out[k] = "\n".join(lines[i + 1:j])
            i = j + 1
        elif "=" in l:
            k, v = l.split("=", 1)
            out[k] = v
            i += 1
        else:
            i += 1
    return out


def run_action(inputs, *, workspace, temp, github=None, env=None):
    """Runs the action's steps; returns (steps, uses_calls, summary)."""
    with open(os.path.join(ACTION, "action.yml")) as f:
        action = yaml.safe_load(f)
    gh = {"workspace": workspace, "token": "ghs_job_token", "event_name": "workflow_dispatch",
          "repository": "acme/widgets", "api_url": "https://api.github.invalid",
          "server_url": "https://github.invalid", "run_id": "1"}
    gh.update(github or {})
    ctx = {"github": gh, "runner": {"os": "Linux" if os.uname().sysname == "Linux" else "macOS", "temp": temp},
           "steps": {}, "inputs": {}}
    for name, spec in action["inputs"].items():
        default = interpolate(spec.get("default", ""), ctx) if isinstance(spec, dict) else ""
        ctx["inputs"][name] = str(inputs.get(name, default))
    path_file = os.path.join(temp, "_path")
    summary = os.path.join(temp, "_summary.md")
    extra_path = []
    uses = []
    failed = False
    base_env = {"PATH": os.environ["PATH"], "HOME": os.path.join(os.path.dirname(temp), "_home"), "RUNNER_TEMP": temp,
                "GITHUB_ACTION_PATH": ACTION, "GITHUB_STEP_SUMMARY": summary, "GITHUB_PATH": path_file,
                "GITHUB_WORKSPACE": workspace, "GITHUB_EVENT_NAME": gh["event_name"],
                "GITHUB_REPOSITORY": gh["repository"], "GITHUB_API_URL": gh["api_url"],
                "RUNNER_OS": ctx["runner"]["os"], "RUNNER_ARCH": "ARM64" if os.uname().machine in ("arm64", "aarch64") else "X64",
                **(env or {})}
    os.makedirs(base_env["HOME"], exist_ok=True)
    for n, st in enumerate(action["runs"]["steps"]):
        sid = st.get("id", f"_{n}")
        cond = st.get("if", "")
        go = evaluate(cond, ctx, failed) if cond else not failed
        if not go:
            ctx["steps"][sid] = {"outputs": {}, "outcome": "skipped"}
            continue
        if "uses" in st:
            uses.append((st["uses"], {k: interpolate(v, ctx, failed) for k, v in (st.get("with") or {}).items()}))
            ctx["steps"][sid] = {"outputs": {}, "outcome": "success"}
            continue
        out_file = tempfile.mktemp(dir=temp, prefix="_out")
        open(out_file, "w").close()
        step_env = dict(base_env)
        step_env["PATH"] = ":".join(extra_path + [base_env["PATH"]])
        step_env["GITHUB_OUTPUT"] = out_file
        for k, v in (st.get("env") or {}).items():
            step_env[k] = interpolate(v, ctx, failed)
        p = subprocess.run(["bash", "-e", "-c", st["run"]], env=step_env, cwd=workspace, capture_output=True, text=True)
        outcome = "success" if p.returncode == 0 else "failure"
        failed = failed or p.returncode != 0
        ctx["steps"][sid] = {"outputs": read_outputs(out_file), "outcome": outcome,
                             "stdout": p.stdout, "stderr": p.stderr, "name": st.get("name", sid)}
        if os.path.exists(path_file):
            extra_path = [l for l in open(path_file).read().splitlines() if l][::-1]
    outputs = {k: interpolate(v.get("value", ""), ctx) for k, v in (action.get("outputs") or {}).items()}
    return ctx["steps"], uses, outputs, failed, (open(summary).read() if os.path.exists(summary) else "")
