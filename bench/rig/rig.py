#!/usr/bin/env python3
"""Benchmark rig: several agent harnesses, one local model, multi-file tasks.

Tasks are SWE-bench Verified instances from repositories that install into a
plain virtualenv. Each harness works in a fresh checkout at the instance's base
commit and sees only the problem statement. Its diff is then scored the way
SWE-bench scores: the gold test files are restored over whatever the agent did
to them, and the instance is resolved only if every FAIL_TO_PASS and every
PASS_TO_PASS test passes.

Nothing is scored on trust. An instance enters the suite only if, in this
environment, its tests fail at the base commit and pass with the gold patch
(`validate`). The rig itself is checked the same way: a harness that does
nothing must score 0 and one that applies the gold patch must score 100
(`selftest`). Neither needs a model.

    rig.py fetch                      download the dataset rows
    rig.py prepare [--limit N]        clone, build a venv per instance
    rig.py validate                   keep only instances that behave
    rig.py selftest                   null must score 0, gold must score 100
    rig.py models --base gemma4:26b   write the Ollama variants per condition
    rig.py doctor --harness abhed     can this harness use a tool on this model?
    rig.py run --date 2026-10-01 --runs 3
    rig.py watch --date 2026-10-01    live progress, from another terminal
    rig.py summarize --date 2026-10-01

Standard library only, so it runs wherever Python does.
"""
import argparse
import hashlib
import json
import os
import random
import re
import shutil
import signal
import statistics
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
CACHE = Path(os.environ.get("ABHED_BENCH_CACHE", HERE / ".cache"))
RESULTS = HERE.parent / "results"
DATASET = "princeton-nlp/SWE-bench_Verified"

# Repositories whose tests are pytest node ids and whose dependencies install
# from wheels. The rest of Verified needs compiled extensions or a bespoke
# test runner, which is a container's job; leaving them out is a stated limit
# of this rig, not a judgement about those tasks.
#
# psf/requests is left out on purpose: its suite calls the live httpbin.org, so
# a run takes two minutes, depends on somebody else's server, and cannot be
# repeated offline. Several of its instances are also Python 2 bugs that do not
# reproduce on a current interpreter.

# Each project gets its own test requirements, as in SWE-bench; without them
# agents meet import errors and write stand-ins for the missing packages.
POOL = {
    "pytest-dev/pytest": {"python": "3.9", "install": ["-e", ".[testing]"]},
    "pylint-dev/pylint": {"python": "3.9", "install": ["-e", ".", "-r", "requirements_test_min.txt", "py"]},
    "pallets/flask": {"python": "3.11", "install": ["-e", ".", "-r", "requirements/tests.txt"]},
}

# Two windows, enforced at the endpoint so every harness meets the same limit.
# The tight one is 24k and not lower because OpenHands documents 22,000 as its
# minimum; a condition a harness says it cannot work in measures nothing.
CONDITIONS = {"full": 32768, "tight": 24576}

# Harnesses installed for the benchmark live under the cache, so the operator's
# own tools and global package directories are left alone.
os.environ["PATH"] = f"{CACHE / 'tools' / 'bin'}:{os.environ['PATH']}"

RUN_TIMEOUT = int(os.environ.get("ABHED_BENCH_TIMEOUT", "1800"))
TEST_TIMEOUT = int(os.environ.get("ABHED_BENCH_TEST_TIMEOUT", "300"))
SLEPT_LIMIT = 120

PROMPT = """You are working in a checkout of {repo}. Resolve the issue below by
changing the source code. Do not edit or add tests: hidden tests decide whether
the issue is resolved. The project's Python environment is already on PATH.

<issue>
{problem}
</issue>"""


# Every harness process group alive now, so a stop signal can end all of them.
_LIVE, _LIVE_LOCK = set(), threading.Lock()
_STOPPING = threading.Event()


class Stopped(Exception):
    """The rig is stopping; a session in flight is abandoned, not scored."""


def _kill_group(p):
    try:
        os.killpg(p.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        pass


def stop_all(signum=None, _frame=None):
    """Kill every live harness group; from a signal, then exit."""
    _STOPPING.set()
    with _LIVE_LOCK:
        pids = list(_LIVE)
    for pid in pids:
        try:
            os.killpg(pid, signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            pass
    if signum is not None:
        raise SystemExit(128 + signum)


def install_stop_handlers():
    for s_ in (signal.SIGINT, signal.SIGTERM):
        signal.signal(s_, stop_all)


def sh(cmd, cwd=None, env=None, timeout=None, check=False):
    # stdin is closed on purpose. pi merges piped stdin into its prompt, so a
    # harness that inherits an open stdin waits on it for ever and never calls
    # the model — which is how the first doctor run on pi spent ten minutes.
    #
    # Its own process group, so a timeout stops the agent's tools with it.
    if _STOPPING.is_set():
        raise Stopped()
    p = subprocess.Popen(cmd, cwd=cwd, env=env, text=True, stdin=subprocess.DEVNULL,
                         stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
    with _LIVE_LOCK:
        _LIVE.add(p.pid)
    try:
        out, _ = p.communicate(timeout=timeout)
    except subprocess.TimeoutExpired as e:
        _kill_group(p)
        try:
            out, _ = p.communicate(timeout=10)
        except subprocess.TimeoutExpired:  # a daemonised grandchild still holds the pipe
            p.stdout.close()
            out = ""
        raise subprocess.TimeoutExpired(cmd, timeout, output=out) from e
    except BaseException:
        _kill_group(p)  # Ctrl-C or any error: the harness must not outlive the rig
        raise
    finally:
        with _LIVE_LOCK:
            _LIVE.discard(p.pid)
    _kill_group(p)  # anything the agent left running in the background
    if _STOPPING.is_set():
        raise Stopped()  # killed by a stop signal: not a result
    if check and p.returncode != 0:
        raise RuntimeError(f"{' '.join(map(str, cmd))} failed:\n{out[-2000:]}")
    return p.returncode, out


# ---------------------------------------------------------------- tasks

def fetch(_):
    CACHE.mkdir(parents=True, exist_ok=True)
    out = CACHE / "verified.jsonl"
    rows, offset = [], 0
    while True:
        q = urllib.parse.urlencode({"dataset": DATASET, "config": "default", "split": "test",
                                    "offset": offset, "length": 100})
        with urllib.request.urlopen(f"https://datasets-server.huggingface.co/rows?{q}", timeout=60) as r:
            page = json.load(r)
        rows += [x["row"] for x in page["rows"]]
        offset += 100
        if offset >= page["num_rows_total"]:
            break
    with out.open("w") as f:
        for row in rows:
            f.write(json.dumps(row) + "\n")
    pool = [r for r in rows if r["repo"] in POOL]
    print(f"{len(rows)} instances; {len(pool)} in the venv-installable pool → {out}")


def instances(repo=None):
    path = CACHE / "verified.jsonl"
    if not path.exists():
        sys.exit("no dataset: run `rig.py fetch` first")
    rows = [json.loads(line) for line in path.open()]
    return sorted((r for r in rows if r["repo"] in POOL and (not repo or repo in r["repo"])),
                  key=lambda r: r["instance_id"])


def env_dir(iid):
    return CACHE / "envs" / iid


def env_spec(repo):
    """What an environment is built from; stored in its `ready` marker."""
    return json.dumps(POOL[repo], sort_keys=True)


def env_current(iid, repo):
    ready = env_dir(iid) / "ready"
    return ready.exists() and ready.read_text() == env_spec(repo)


def stale_envs(ids):
    """Instances whose environment was built from another spec, or whose
    repository is no longer in the pool."""
    repo = {i["instance_id"]: i["repo"] for i in instances()}
    return [t for t in ids if t not in repo or not env_current(t, repo[t])]


def prepare(args):
    todo = instances(args.repo)
    todo = todo[: args.limit] if args.limit else todo
    for inst in todo:
        iid, spec = inst["instance_id"], POOL[inst["repo"]]
        d = env_dir(iid)
        if env_current(iid, inst["repo"]):
            continue
        print(f"prepare {iid}", flush=True)
        shutil.rmtree(d, ignore_errors=True)
        d.mkdir(parents=True)
        mirror = CACHE / "mirrors" / inst["repo"].replace("/", "__")
        try:
            if not mirror.exists():
                sh(git(mirror) + ["clone", "--quiet", "--mirror", f"https://github.com/{inst['repo']}.git", str(mirror)],
                   check=True)
            sh(git(d) + ["clone", "--quiet", str(mirror), str(d / "repo")], check=True)
            sh(git(d / "repo") + ["checkout", "--quiet", inst["base_commit"]], cwd=d / "repo", check=True)
            sh(["uv", "venv", "--quiet", "--python", spec["python"], str(d / "venv")], check=True)
            py = str(d / "venv" / "bin" / "python")
            sh(["uv", "pip", "install", "--quiet", "--python", py, *spec["install"]],
               cwd=d / "repo", timeout=900, check=True)
            (d / "instance.json").write_text(json.dumps(inst))
            (d / "ready").write_text(env_spec(inst["repo"]))
            (CACHE / "suite.json").unlink(missing_ok=True)  # validated against the old environment
        except Exception as e:  # noqa: BLE001 - an instance that will not build is excluded, not fatal
            (d / "failed").write_text(str(e)[-3000:])
            print(f"  could not build: {str(e).splitlines()[0][:120]}")


def session_venv(iid, dest):
    """The agent's own copy of the prepared environment. Whatever it installs
    or upgrades stays with the session; the tests are scored in the original,
    and the next session starts clean."""
    src = env_dir(iid) / "venv"
    shutil.rmtree(dest, ignore_errors=True)
    shutil.copytree(src, dest, symlinks=True)
    for f in (dest / "bin").iterdir():  # console scripts name the interpreter by absolute path
        if f.is_file() and not f.is_symlink():
            try:
                text = f.read_text()
            except UnicodeDecodeError:
                continue
            if str(src) in text:
                f.write_text(text.replace(str(src), str(dest)))
    for f in (dest / "bin").iterdir():  # a link back into the prepared env would let the agent write to it
        if f.is_symlink() and Path(os.path.realpath(f)).is_relative_to(src.resolve()):
            target = Path(os.path.realpath(f)).relative_to(src.resolve())
            f.unlink()
            f.symlink_to(dest / target)
    return dest


def workspace(iid, dest):
    """A fresh checkout at the base commit, with no history to mine."""
    inst = json.loads((env_dir(iid) / "instance.json").read_text())
    shutil.rmtree(dest, ignore_errors=True)
    shutil.rmtree(rig_git(dest), ignore_errors=True)
    # A copy of the prepared tree, not a clone: installing a package can write
    # files git does not track (pytest's _version.py comes from setuptools_scm)
    # and a clone would leave them behind and the package unimportable.
    #
    # .git is left out. The fix is in the repository's future, and an agent
    # must not be able to find it with `git log`.
    shutil.copytree(env_dir(iid) / "repo", dest, symlinks=True, ignore=shutil.ignore_patterns(".git"))
    for g in (git(dest), git(dest, rig_git(dest))):
        sh(g + ["init", "--quiet"], cwd=dest, check=True)
        sh(g + ["add", "-A"], cwd=dest, check=True)
        sh(g + ["commit", "--quiet", "-m", "base"], cwd=dest, check=True)
    return inst


def rig_git(ws):
    """The rig's own record of the base tree, outside the workspace, where the
    agent's commits, resets and hooks cannot reach it."""
    return ws.parent / f"{ws.name}.rig-git"


def git(ws, git_dir=None):
    """git with hooks off and a fixed identity; `git_dir` selects the rig's record."""
    g = ["env", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "git", "-c", "core.hooksPath=/dev/null",
         "-c", "user.name=bench", "-c", "user.email=bench@localhost"]
    return g + ([f"--git-dir={git_dir}", f"--work-tree={ws}"] if git_dir else [])


class NoPatch(Exception):
    """The agent's change could not be read."""


def agent_patch(ws):
    """The agent's change against the base tree, from the rig's own record."""
    g = git(ws, rig_git(ws))
    keep_out = [":(exclude).abhed", ":(exclude).pi", ":(exclude).openhands"]
    rc, out = sh(g + ["add", "-A", "--", ".", *keep_out], cwd=ws)
    if rc != 0:
        raise NoPatch(out[-500:])
    rc, out = sh(g + ["diff", "--cached", "--binary", "HEAD", "--", ".", *keep_out], cwd=ws)
    if rc != 0:
        raise NoPatch(out[-500:])
    return out


# Abhed applies an organisation's managed config last, over the rig's.
MANAGED_CONFIG = "/etc/abhed/config.json"

# What a harness inherits from the operator's shell. Nothing else: no provider
# credentials, and no ABHED_MODEL-style overrides of the model the rig set.
PASS_ENV = {"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "USER", "LOGNAME", "SHELL", "TZ",
            "SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"}


def harness_env():
    return {k: v for k, v in os.environ.items() if k in PASS_ENV}


def task_env(iid, ws, venv=None, tmp=None):
    env = harness_env()
    env["PATH"] = f"{(venv or env_dir(iid) / 'venv') / 'bin'}:{env['PATH']}"
    # pytest's scratch lives under the temp dir, one tree per user; sharing it
    # makes sessions race on its cleanup.
    if tmp:
        env["TMPDIR"] = str(tmp)
    # The venv's editable install points at the prepared checkout, not this
    # copy. PYTHONPATH comes first, so the workspace's code is what runs —
    # and `validate` would exclude any instance where that did not hold.
    env["PYTHONPATH"] = f"{ws / 'src'}:{ws}"
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    return env


# ---------------------------------------------------------------- scoring

STATUS = re.compile(r"^(PASSED|FAILED|ERROR|XFAIL|XPASS|SKIPPED)\s+(\S.*?)(?:\s+-\s.*)?$")


def apply_patch(ws, patch):
    p = subprocess.run(git(ws) + ["apply", "--whitespace=nowarn", "-"], cwd=ws, input=patch, text=True,
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    return p.returncode == 0, p.stdout


# An instance is kept if the gold patch passes at least this share of its
# PASS_TO_PASS tests here. The ones it fails are dependency drift — they fail
# with the reference fix applied, so they cannot tell a regression from the
# environment — and are excluded for that instance and listed in suite.json.
P2P_FLOOR = 0.90


def fair_at_base(status, f2p):
    """Do the target tests run and fail at the base commit, or fail to load?

    A test file that imports a name the reference patch introduces cannot be
    collected without it. Such an instance scores zero for any fix that does
    not happen to invent the same name in the same place — it measures
    guessing, not fixing — so it has no business in a comparison. The target
    tests must be collected and FAIL; being absent or erroring is not enough.
    """
    return bool(f2p) and all(status.get(t) == "FAILED" for t in f2p)


def score(iid, ws, inst, skip=()):
    """Restore the gold tests over the agent's, run them, apply the SWE-bench rule."""
    files = re.findall(r"^diff --git a/(\S+) b/", inst["test_patch"], flags=re.M)
    for f in files:  # undo agent edits to test files
        rc, _ = sh(git(ws) + ["cat-file", "-e", f"HEAD:{f}"], cwd=ws)
        if rc == 0:
            sh(git(ws) + ["checkout", "--quiet", "HEAD", "--", f], cwd=ws, check=True)
        elif (ws / f).exists():
            (ws / f).unlink()  # a file the gold patch creates
    ok, out = apply_patch(ws, inst["test_patch"])
    if not ok:
        return {"resolved": False, "reason": "gold tests did not apply", "detail": out[-800:]}

    f2p = json.loads(inst["FAIL_TO_PASS"])
    p2p = [t for t in json.loads(inst["PASS_TO_PASS"]) if t not in set(skip)]
    test_files = sorted({t.split("::")[0] for t in f2p + p2p})
    # A temp dir of its own: nothing the agent left may decide a tmp_path test.
    env = task_env(iid, ws, tmp=tempfile.mkdtemp(prefix="rig-score-"))
    try:
        _, out = sh(["python", "-m", "pytest", "-rA", "-p", "no:cacheprovider", "-q", *test_files],
                    cwd=ws, env=env, timeout=TEST_TIMEOUT)
    except subprocess.TimeoutExpired:
        return {"resolved": False, "reason": "tests timed out"}
    finally:
        shutil.rmtree(env["TMPDIR"], ignore_errors=True)

    status = {}
    for line in out.splitlines():
        m = STATUS.match(line.strip())
        if m:
            status[m.group(2).strip()] = m.group(1)
    f2p_ok = [t for t in f2p if status.get(t) == "PASSED"]
    p2p_ok = [t for t in p2p if status.get(t) in ("PASSED", "XFAIL")]
    return {
        "resolved": len(f2p_ok) == len(f2p) and len(p2p_ok) == len(p2p),
        "f2p": f"{len(f2p_ok)}/{len(f2p)}", "p2p": f"{len(p2p_ok)}/{len(p2p)}",
        "f2p_fraction": len(f2p_ok) / max(len(f2p), 1),
        "p2p_failed": [t for t in p2p if t not in p2p_ok],
        "f2p_ran_and_failed": fair_at_base(status, f2p),
        "tail": out[-1500:],
    }


def validate(_):
    """An instance is usable only if base fails and gold passes, here."""
    valid = {}
    pool = {i["instance_id"] for i in instances()}
    built = [p.parent.name for p in sorted((CACHE / "envs").glob("*/ready")) if p.parent.name in pool]
    stale = set(stale_envs(built))
    ready = [i for i in built if i not in stale]
    for iid in ready:
        ws = CACHE / "scratch" / "validate"
        inst = workspace(iid, ws)
        base = score(iid, ws, inst)
        inst = workspace(iid, ws)
        applied, _ = apply_patch(ws, inst["patch"])
        gold = score(iid, ws, inst) if applied else {"resolved": False, "reason": "gold patch did not apply"}

        n_p2p = len(json.loads(inst["PASS_TO_PASS"]))
        drift = gold.get("p2p_failed", [])
        good = (not base["resolved"] and base.get("f2p_ran_and_failed", False)
                and gold.get("f2p_fraction") == 1 and "reason" not in gold
                and len(drift) <= (1 - P2P_FLOOR) * n_p2p)
        note = f"  ({len(drift)} drifted test(s) excluded)" if good and drift else ""
        print(f"{'ok  ' if good else 'DROP'} {iid}  base f2p {base.get('f2p', '-')}  "
              f"gold f2p {gold.get('f2p', '-')} p2p {gold.get('p2p', '-')}{note}", flush=True)
        if good:
            valid[iid] = drift
        else:
            (env_dir(iid) / "invalid.json").write_text(json.dumps({"base": base, "gold": gold}, indent=1))
    for d in (CACHE / "scratch" / "validate", rig_git(CACHE / "scratch" / "validate")):
        shutil.rmtree(d, ignore_errors=True)
    (CACHE / "suite.json").write_text(json.dumps(valid, indent=1))
    print(f"\n{len(valid)} of {len(ready)} prepared instances are valid → {CACHE / 'suite.json'}")


def suite():
    path = CACHE / "suite.json"
    if not path.exists():
        sys.exit("no suite: run `rig.py validate` first")
    return json.loads(path.read_text())  # instance id → tests excluded as drift


# ---------------------------------------------------------------- harnesses

def model_name(cond):
    """The model id the harnesses ask for. Local runs use the Ollama variants
    `models` writes, one per window condition; a remote endpoint names its
    model directly and the window is what the harnesses are told, since the
    rig cannot set num_ctx on a server it does not run."""
    remote = os.environ.get("ABHED_BENCH_MODEL")
    if remote:
        return remote
    return f"abhed-bench-{cond}"


def window(cond):
    """The window the harnesses are told. Local runs enforce it at the endpoint;
    on a hosted model it is the model's own, and OpenHands is not told it."""
    if remote():
        return int(os.environ.get("ABHED_BENCH_CONTEXT", "131072"))
    return CONDITIONS[cond]


# The per-turn output limit for every harness, read by the proxy hook too.
# Abhed and pi are told it; the endpoint enforces it for OpenHands.
MAX_OUTPUT = int(os.environ.get("ABHED_BENCH_MAX_OUTPUT", "8192"))


def endpoint():
    return os.environ.get("ABHED_BENCH_ENDPOINT", "http://127.0.0.1:11434/v1")


def api_key():
    """The key the harnesses send. Ollama ignores it; a hosted endpoint needs
    it. It comes from the environment only and is written into no result."""
    return os.environ.get("ABHED_BENCH_API_KEY", "bench")


def remote():
    return bool(os.environ.get("ABHED_BENCH_MODEL"))


class Harness:
    name = ""
    # Where the invocation below is documented, and when it was read. A harness
    # run in a way its authors do not recommend measures the mistake.
    source = ""

    version_cmd = None

    def available(self):
        return True, ""

    def version(self):
        """Harnesses ship weekly; a result without a version cannot be repeated."""
        if not self.version_cmd:
            return ""
        try:
            return sh(self.version_cmd, timeout=30)[1].strip().splitlines()[-1][:120]
        except Exception:  # noqa: BLE001
            return "unknown"

    def run(self, ws, prompt, cond, env, home):
        raise NotImplementedError

    def usage(self, output):
        return {}


class Null(Harness):
    """Does nothing. The rig must score it 0."""
    name = "null"

    def run(self, ws, prompt, cond, env, home):
        return 0, ""


class Gold(Harness):
    """Applies the reference patch. The rig must score it 100."""
    name = "gold"

    def run(self, ws, prompt, cond, env, home):
        ok, out = apply_patch(ws, self.inst["patch"])
        return (0 if ok else 1), out


class Abhed(Harness):
    name = "abhed"
    source = "docs/guide/10-automation.md in this repository"

    def binary(self):
        return os.environ.get("ABHED_BIN", str(HERE.parent.parent / "abhed"))

    def version(self):
        return sh([self.binary(), "-version"], timeout=30)[1].strip()[:120]

    def available(self):
        if Path(MANAGED_CONFIG).exists():
            return False, f"{MANAGED_CONFIG} would override the rig's settings; move it aside for the run"
        return Path(self.binary()).exists(), f"build it: go build -o abhed ./cmd/abhed (looked for {self.binary()})"

    @staticmethod
    def configure(cfg, cond):
        """The shipped defaults with only the model changed, told the same
        window and output limit as the other harnesses."""
        cfg["model"] = {"default": "bench", "providers": {"bench": {
            "type": "openai-compatible", "base_url": endpoint(), "model": model_name(cond),
            "api_key": api_key(), "context_window": window(cond), "params": {}}}}
        cfg.setdefault("limits", {})["max_tokens"] = MAX_OUTPUT  # what Abhed sends as max_tokens
        return cfg

    def run(self, ws, prompt, cond, env, home):
        # A throwaway HOME, as the others get: no operator skills or ABHED.md.
        env = dict(env, HOME=str(home))
        sh([self.binary(), "-C", str(ws), "init"], cwd=ws, env=env, check=True)
        path = ws / ".abhed" / "config.json"
        path.write_text(json.dumps(self.configure(json.loads(path.read_text()), cond), indent=1))

        # Unattended like the others; bypass keeps the sandbox and deny rules.
        # JSON output is the event record HawkEYE reads; turns stay at the default.
        return sh([self.binary(), "-C", str(ws), "-mode", "bypass",
                   "-output-format", "json", "-p", prompt],
                  cwd=ws, env=env, timeout=RUN_TIMEOUT)

    def record(self, output):
        events = []
        for line in output.splitlines():
            line = line.strip()
            if not line.startswith("{"):
                continue
            try:
                ev = json.loads(line)
            except json.JSONDecodeError:
                continue
            if isinstance(ev, dict) and "type" in ev and "seq" in ev:
                events.append(ev)
        return events

    def usage(self, output):
        for ev in reversed(self.record(output)):
            if ev["type"] == "session.ended":
                p = ev.get("payload", {})
                return {"turns": p.get("turns", 0), "tokens_in": p.get("tokens_in", 0),
                        "tokens_out": p.get("tokens_out", 0), "reason": p.get("reason", "")}
        m = re.search(r"(\d+)\s*turns?\s*·\s*([\d,]+)\s*in\s*/\s*([\d,]+)\s*out\s*tokens", output)
        if not m:
            return {}
        return {"turns": int(m.group(1)), "tokens_in": int(m.group(2).replace(",", "")),
                "tokens_out": int(m.group(3).replace(",", ""))}


def hawkeye(events, out_path):
    """Write the session's record beside its result and let HawkEYE read it.

    Returns the finding codes, so a summary can say not only that a session
    failed but what the record shows it doing: a call denied, a repeated
    failure, a session that hit the context ceiling."""
    rec = out_path.with_name(out_path.stem + ".events.json")
    rec.write_text(redact(json.dumps(redact_obj(events))))
    binary = Abhed().binary()
    if not Path(binary).exists():
        return {"findings": [], "note": "abhed binary not built; record saved, not analysed"}
    report = out_path.with_name(out_path.stem + ".hawkeye.json")
    rc, out = sh([binary, "hawkeye", "-o", str(report), str(rec)], timeout=120)
    if rc != 0 or not report.exists():
        return {"findings": [], "note": "hawkeye failed: " + out[-300:]}
    try:
        data = json.loads(report.read_text())
    except json.JSONDecodeError:
        return {"findings": [], "note": "hawkeye wrote no JSON"}
    return {"findings": [{"code": f.get("code"), "severity": f.get("severity")} for f in data.get("findings", [])]}


class Pi(Harness):
    name = "pi"
    version_cmd = ["pi", "--version"]
    source = "https://github.com/badlogic/pi-mono packages/coding-agent README and docs/models.md, read 2026-09-21"

    def available(self):
        return shutil.which("pi") is not None, "npm install -g @mariozechner/pi-coding-agent"

    def run(self, ws, prompt, cond, env, home):
        # pi reads ~/.pi/agent/models.json. A per-run HOME keeps the benchmark
        # out of the operator's own configuration, and one run out of the next.
        agent = home / ".pi" / "agent"
        agent.mkdir(parents=True, exist_ok=True)
        (agent / "models.json").write_text(json.dumps({"providers": {"bench": {
            "baseUrl": endpoint(), "api": "openai-completions", "apiKey": api_key(),
            "models": [{"id": model_name(cond), "name": model_name(cond), "reasoning": False, "input": ["text"],
                        "contextWindow": window(cond), "maxTokens": MAX_OUTPUT,
                        "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}}]}}}, indent=1))
        env = dict(env, HOME=str(home))
        return sh(["pi", "--provider", "bench", "--model", model_name(cond), "--mode", "json", "-p", prompt],
                  cwd=ws, env=env, timeout=RUN_TIMEOUT)


    def usage(self, output):
        """Sum the usage pi reports on each finished assistant message."""
        tin = tout = turns = 0
        for line in output.splitlines():
            if '"message_end"' not in line or '"assistant"' not in line:
                continue
            try:
                u = json.loads(line)["message"].get("usage") or {}
            except (ValueError, KeyError):
                continue
            tin += u.get("input", 0) + u.get("cacheRead", 0)
            tout += u.get("output", 0)
            turns += 1
        return {"turns": turns, "tokens_in": tin, "tokens_out": tout} if turns else {}


class OpenHands(Harness):
    name = "openhands"
    version_cmd = ["openhands", "--version"]
    source = ("https://docs.openhands.dev/openhands/usage/cli/headless, .../cli/command-reference and "
              ".../llms/local-llms, read 2026-09-21")

    def available(self):
        return shutil.which("openhands") is not None, "install the OpenHands CLI (see its docs)"

    def run(self, ws, prompt, cond, env, home):
        # Headless always approves, per its docs. LLM settings come from the
        # environment only with --override-with-envs, and are not persisted.
        env = dict(env, HOME=str(home), LLM_MODEL=f"openai/{model_name(cond)}",
                   LLM_BASE_URL=endpoint(), LLM_API_KEY=api_key())
        return sh(["openhands", "--headless", "--json", "--override-with-envs", "-t", prompt],
                  cwd=ws, env=env, timeout=RUN_TIMEOUT)


HARNESSES = {h.name: h for h in (Null, Gold, Abhed, Pi, OpenHands)}


def timeout_output(e):
    """What a timed-out command printed; it comes back as bytes even in text mode."""
    out = e.output or b""
    return out.decode("utf-8", "replace") if isinstance(out, bytes) else out


def result_path(out_path, slept, readable=True):
    """A session the machine slept through, or whose change the rig could not
    read, is kept aside rather than scored, so a resume redoes it."""
    if not readable:
        return out_path.with_suffix(".unread.json")
    return out_path.with_suffix(".slept.json") if slept > SLEPT_LIMIT else out_path


SECRET_NAME = re.compile(r"KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL", re.I)


def _secrets():
    found = {api_key()} | {v for k, v in os.environ.items()
                           if SECRET_NAME.search(k) or k.startswith("ABHED_BENCH_WATSONX")}
    forms = found | {json.dumps(v)[1:-1] for v in found}  # as a JSON string would carry it
    return sorted((v for v in forms if len(v) >= 8), key=len, reverse=True)


def redact(text):
    """No key from the rig's environment goes into a result, whatever a harness printed."""
    for v in _secrets():
        text = text.replace(v, "[redacted]")
    return text


def redact_obj(obj):
    """redact() over every string in a result, before it is serialised."""
    if isinstance(obj, str):
        return redact(obj)
    if isinstance(obj, dict):
        return {k: redact_obj(v) for k, v in obj.items()}
    if isinstance(obj, list):
        return [redact_obj(v) for v in obj]
    return obj


def scratch_tag(hname, cond, iid, run, out_path):
    # The result path in the name keeps two rig invocations apart.
    return f"{hname}-{cond}-{iid}-run{run}-{hashlib.sha1(str(out_path).encode()).hexdigest()[:6]}"


def score_patch(iid, inst, patch, tag):
    """Score the agent's change on a fresh copy of the base tree, so nothing
    else it left behind (git state, caches, files outside the diff) counts."""
    fresh = CACHE / "scratch" / f"score-{tag}"
    try:
        workspace(iid, fresh)
        if patch.strip():
            ok, out = apply_patch(fresh, patch)
            if not ok:
                return {"resolved": False, "reason": "agent patch did not apply to the base", "detail": out[-800:]}
        return score(iid, fresh, inst, skip=suite().get(iid, []))
    finally:
        shutil.rmtree(fresh, ignore_errors=True)
        shutil.rmtree(rig_git(fresh), ignore_errors=True)


def one_run(hname, iid, cond, out_path, run=1):
    tag = scratch_tag(hname, cond, iid, run, out_path)
    ws = CACHE / "scratch" / tag
    home = CACHE / "scratch" / f"home-{tag}"
    venv = CACHE / "scratch" / f"venv-{tag}"
    tmp = CACHE / "scratch" / f"tmp-{tag}"
    try:
        for d in (home, tmp):
            shutil.rmtree(d, ignore_errors=True)
            d.mkdir(parents=True)
        inst = workspace(iid, ws)
        session_venv(iid, venv)
        h = HARNESSES[hname]()
        h.inst = inst
        prompt = PROMPT.format(repo=inst["repo"], problem=inst["problem_statement"])
        started, awake, timed_out, output, rc = time.time(), time.monotonic(), False, "", None
        try:
            rc, output = h.run(ws, prompt, cond, task_env(iid, ws, venv, tmp), home)
        except subprocess.TimeoutExpired as e:
            timed_out, output = True, timeout_output(e)
        if _STOPPING.is_set():
            raise Stopped()
        elapsed = time.time() - started
        # The monotonic clock, and so the session timeout, stops while the machine sleeps.
        slept = elapsed - (time.monotonic() - awake)

        readable = True
        try:
            diff = agent_patch(ws)
            scored = score_patch(iid, inst, diff, tag)
        except NoPatch as e:
            readable, diff, scored = False, "", {"resolved": False, "reason": "unread", "detail": str(e)}
        result = {"harness": hname, "base_model": base_model(), "finished": time.strftime("%Y-%m-%d %H:%M:%S"),
                  "version": h.version(), "instance": iid, "condition": cond, "exit_code": rc,
                  "timed_out": timed_out, "wall_sec": round(elapsed, 1), "patch_bytes": len(diff),
                  "patch": redact(diff[-20000:]), "usage": h.usage(output), "output_tail": redact(output[-3000:]),
                  "difficulty": inst.get("difficulty", ""), "score": scored}
        out_path.parent.mkdir(parents=True, exist_ok=True)
        events = h.record(output) if hasattr(h, "record") else None
        if events:
            result["hawkeye"] = hawkeye(events, out_path)
        final = result_path(out_path, slept, readable)
        if slept > SLEPT_LIMIT:
            result["slept_sec"] = round(slept)
        final.write_text(redact(json.dumps(redact_obj(result), indent=1)))
        return result
    finally:
        # The workspace and home hold the endpoint key in harness config.
        for d in (ws, rig_git(ws), home, venv, tmp):
            shutil.rmtree(d, ignore_errors=True)


def pending(plan, root, force=False):
    """Sessions still to run: those without a result. A slept session has
    only a `.slept.json` beside where its result goes, so it is pending."""
    todo = []
    for i, (r, iid, cond, n) in enumerate(plan, 1):
        out = root / n / cond / f"run{r}" / f"{iid}.json"
        if out.exists() and not force:
            continue
        todo.append((i, r, iid, cond, n, out))
    return todo


def selftest(_):
    failures = 0
    for iid in sorted(suite()):
        for hname, want in (("null", False), ("gold", True)):
            r = one_run(hname, iid, "full", CACHE / "selftest" / hname / f"{iid}.json")
            good = r["score"]["resolved"] is want
            failures += not good
            print(f"{'ok  ' if good else 'FAIL'} {hname:5} {iid}  resolved={r['score']['resolved']}", flush=True)
    if failures:
        sys.exit(f"\n{failures} self-test failure(s): the rig's scores cannot be trusted until these are explained")
    print("\nself-test passed: doing nothing scores 0, the reference patch scores 100")


DIFFICULTY_ALIASES = {"easy": "<15 min fix", "medium": "15 min - 1 hour", "hard": "1-4 hours", "hardest": ">4 hours"}


def by_difficulty(tasks, label):
    """Keep the tasks the dataset rates at one difficulty. The rating is the
    dataset's own — the time a human annotator judged the fix to take — and
    it is written into the plan and every result, so a run on the easy band
    can never be mistaken for a run on the whole suite."""
    label = DIFFICULTY_ALIASES.get(label, label)
    rated = {i["instance_id"]: i.get("difficulty", "") for i in instances()}
    return [t for t in tasks if rated.get(t) == label]


def result_files(root):
    """One file per session: the result, not the record or the HawkEYE
    report the rig writes beside it."""
    return [p for p in sorted(root.glob("*/*/run*/*.json"))
            if not p.name.endswith((".events.json", ".hawkeye.json", ".slept.json", ".unread.json"))]


def base_model():
    """The model behind the benchmark variants, as `models` last set it — or
    the remote model named in the environment, with its endpoint's host."""
    if remote():
        host = urllib.parse.urlparse(endpoint()).netloc or endpoint()
        return f"{os.environ['ABHED_BENCH_MODEL']} @ {host}"
    p = CACHE / "base-model.txt"
    return p.read_text().strip() if p.exists() else "unknown"


def models(args):
    d = CACHE / "modelfiles"
    d.mkdir(parents=True, exist_ok=True)
    # The variants keep one name whatever they are built from, so the base is
    # written down: a result that does not say which model produced it is not
    # a result, and a doctor pass on one model says nothing about another.
    (CACHE / "base-model.txt").write_text(args.base + "\n")
    for p in CACHE.glob("doctor-*.ok"):
        p.unlink()
    for cond, ctx in CONDITIONS.items():
        (d / f"{cond}.Modelfile").write_text(f"FROM {args.base}\nPARAMETER num_ctx {ctx}\nPARAMETER num_predict {MAX_OUTPUT}\n")
        print(f"ollama create {model_name(cond)} -f {d / f'{cond}.Modelfile'}")
    print("\nThe window is set at the endpoint, so every harness meets the same limit.")


def setup_fingerprint(hname):
    """What a doctor pass vouches for: the model, the endpoint, the limits and
    the environment a harness gets. A change to any of them needs a new pass."""
    setup = {"harness": hname, "model": base_model(), "max_output": MAX_OUTPUT,
             "window": {c: window(c) for c in CONDITIONS}, "pass_env": sorted(PASS_ENV)}
    return hashlib.sha256(json.dumps(setup, sort_keys=True).encode()).hexdigest() + "\n"


def doctor(args):
    """Before hours are spent: can this harness drive a tool on this model at all?"""
    h = HARNESSES[args.harness]()
    ok, how = h.available()
    if not ok:
        sys.exit(f"{args.harness}: not installed — {how}")
    ws, home = CACHE / "scratch" / "doctor", CACHE / "scratch" / "doctor-home"
    for p in (ws, home):
        shutil.rmtree(p, ignore_errors=True)
        p.mkdir(parents=True)
    sh(git(ws) + ["init", "--quiet"], cwd=ws)
    try:
        rc, out = h.run(ws, "Create a file named hello.txt containing exactly the word ready. Then stop.",
                        "full", harness_env(), home)
        made = (ws / "hello.txt").exists() and "ready" in (ws / "hello.txt").read_text()
    finally:
        for p in (ws, home):  # both hold the endpoint key in harness config
            shutil.rmtree(p, ignore_errors=True)
    print(out[-1200:])
    if not made:
        sys.exit(f"\n{args.harness}: exit {rc}, and hello.txt was not written. Fix the setup before benchmarking.")
    (CACHE / f"doctor-{args.harness}.ok").write_text(setup_fingerprint(args.harness))
    print(f"\n{args.harness}: ok — it used a tool on this model")


def run(args):
    names = args.harness or ["abhed", "pi", "openhands"]
    for n in names:
        ok = CACHE / f"doctor-{n}.ok"
        if n not in ("null", "gold") and (not ok.exists() or ok.read_text() != setup_fingerprint(n)):
            sys.exit(f"{n} has not passed `rig.py doctor --harness {n}`; an unchecked setup is not a measurement")
    if remote() and (args.condition or list(CONDITIONS)) != ["full"]:
        sys.exit("a remote model serves one window, its own: run with --condition full. "
                 "The tight condition needs num_ctx set at the endpoint, which only a local Ollama allows.")
    tasks = sorted(suite())
    stale = stale_envs(tasks)
    if stale:
        sys.exit(f"{len(stale)} environment(s) were built from another spec: run prepare and validate")
    if getattr(args, "difficulty", None):
        tasks = by_difficulty(tasks, args.difficulty)
        if not tasks:
            sys.exit(f"no valid instance is rated {args.difficulty!r}")
    if args.limit:
        # A sample, not the first N: sorted order puts one repository first,
        # and a pilot drawn from a single corner of the suite says little.
        tasks = sorted(random.Random(args.seed).sample(tasks, min(args.limit, len(tasks))))
    # Interleave so a slow afternoon or a thermal throttle lands on every
    # harness rather than on whichever ran last.
    plan = [(r, iid, cond, n) for r in range(1, args.runs + 1) for iid in tasks
            for cond in (args.condition or list(CONDITIONS)) for n in names]
    random.Random(args.seed).shuffle(plan)
    # The suite is whatever validated on this machine, so it travels with the
    # results: which instances, and which drifted tests were set aside.
    root = RESULTS / args.date / "rig"
    sessions = [{"run": r, "instance": iid, "condition": cond, "harness": n} for r, iid, cond, n in plan]
    old = root / "plan.json"
    if old.exists() and not args.force and json.loads(old.read_text())["sessions"] != sessions:
        sys.exit("a run with this date has a different plan; resume it with the same options or use a new --date")
    (RESULTS / args.date / "rig").mkdir(parents=True, exist_ok=True)
    (RESULTS / args.date / "rig" / "suite.json").write_text(json.dumps(suite(), indent=1))
    (RESULTS / args.date / "rig" / "plan.json").write_text(json.dumps(
        {"started": time.strftime("%Y-%m-%d %H:%M:%S"), "timeout_sec": RUN_TIMEOUT, "base_model": base_model(),
         "context_window": {c: window(c) for c in (args.condition or list(CONDITIONS))}, "max_output": MAX_OUTPUT,
         "difficulty": getattr(args, "difficulty", None) or "any",
         "sessions": [{"run": r, "instance": iid, "condition": cond, "harness": n} for r, iid, cond, n in plan]}))
    todo = pending(plan, RESULTS / args.date / "rig", args.force)

    def one(item):
        i, r, iid, cond, n, out = item
        res = one_run(n, iid, cond, out, run=r)
        print(f"[{i}/{len(plan)}] {n:9} {cond:5} run{r} {iid}  resolved={res['score']['resolved']}  {res['wall_sec']}s", flush=True)

    # Sessions are independent: each has its own workspace, home and result
    # file. On a local model they run one at a time, since the model is the
    # bottleneck; on a hosted one several may run, and the machine's own CPU
    # and sandbox are what bound the number.
    execute(todo, max(1, getattr(args, "parallel", 1) or 1), one)


def execute(todo, workers, one):
    """Run sessions, several at once if asked. A stop signal kills every live
    harness at once; sessions in flight are abandoned and queued ones dropped."""
    def guarded(item):
        try:
            one(item)
        except Stopped:
            pass

    if workers == 1:
        for item in todo:
            guarded(item)
        return
    import concurrent.futures
    pool = concurrent.futures.ThreadPoolExecutor(max_workers=workers)
    try:
        for _ in pool.map(guarded, todo):
            pass
    finally:
        pool.shutdown(wait=True, cancel_futures=True)


# ---------------------------------------------------------------- watch

def _hms(sec):
    sec = int(sec)
    return f"{sec // 3600}h{sec % 3600 // 60:02d}m" if sec >= 3600 else f"{sec // 60}m{sec % 60:02d}s"


def _current():
    """The session in flight, read from the scratch directory it works in."""
    scratch = CACHE / "scratch"
    names = "|".join(sorted(HARNESSES, key=len, reverse=True))
    conds = "|".join(CONDITIONS)
    for d in sorted(scratch.glob("*"), key=lambda p: p.stat().st_mtime, reverse=True):
        m = re.fullmatch(rf"({names})-({conds})-(.+?)(?:-run\d+)?(?:-[0-9a-f]{{6}})?", d.name)
        if m and d.is_dir() and not d.name.endswith(".rig-git"):
            return d, m.group(1), m.group(2), m.group(3)
    return None


def editing(d):
    """Files a session in flight has changed, read without locks so the
    session's own `add` never meets a lock of ours."""
    g = git(d, rig_git(d)) + ["--no-optional-locks"]
    try:
        _, diff = sh(g + ["diff", "--name-only", "HEAD"], cwd=d, timeout=20)
        _, new = sh(g + ["ls-files", "-o", "--exclude-standard"], cwd=d, timeout=20)
    except (OSError, subprocess.TimeoutExpired):
        return []  # the session ended, or git was slow, while we looked
    return [ln.strip() for ln in (diff + new).splitlines() if ln.strip() and not ln.startswith(".")]


def snapshot(date):
    root = RESULTS / date / "rig"
    done = [json.loads(p.read_text()) for p in result_files(root)]
    plan = json.loads((root / "plan.json").read_text()) if (root / "plan.json").exists() else None
    total = len(plan["sessions"]) if plan else None
    alive = subprocess.run(["pgrep", "-f", f"rig.py run --date {date}"], stdout=subprocess.PIPE).returncode == 0

    out = [f"rig · {date} · {(plan or {}).get('base_model', base_model())} · {'running' if alive else 'not running'}", ""]
    bar = ""
    if total:
        filled = 30 * len(done) // total
        bar = f"  [{'█' * filled}{'·' * (30 - filled)}]"
    out.append(f"  sessions  {len(done)}{f' of {total}' if total else ''} finished{bar}")
    if done:
        walls = [r["wall_sec"] for r in done]
        out.append(f"  timing    mean {_hms(statistics.mean(walls))} · longest {_hms(max(walls))} · "
                   f"{sum(r['timed_out'] for r in done)} timed out")
        if total and alive:
            out.append(f"  remaining about {_hms(statistics.mean(walls) * (total - len(done)))} at this pace")

    cur = _current() if alive else None
    if cur:
        d, h, cond, iid = cur
        out += ["", f"  now       {h} · {cond} · {iid}   ({_hms(time.time() - d.stat().st_ctime)} of {_hms(RUN_TIMEOUT)} allowed)"]
        out.append(f"  editing   {', '.join(editing(d)[:4]) or 'nothing changed yet'}")

    if done:
        cells = {}
        for r in done:
            c = cells.setdefault((r["harness"], r["condition"]), [0, 0, 0.0])
            c[0] += r["score"]["resolved"]
            c[1] += 1
            c[2] += r["wall_sec"]
        out += ["", "  harness    window  resolved   mean time"]
        for (h, cond), (ok, n, wall) in sorted(cells.items()):
            out.append(f"  {h:10} {cond:6}  {ok:>3} / {n:<3}   {_hms(wall / n)}")
        out += ["", "  last finished"]
        for r in sorted(done, key=lambda r: r.get("finished", ""))[-5:]:
            mark = "✓" if r["score"]["resolved"] else ("⏱" if r["timed_out"] else "✗")
            out.append(f"  {mark} {r['harness']:10} {r['condition']:6} {r['instance']:32} {_hms(r['wall_sec'])}  "
                       f"f2p {r['score'].get('f2p', '-')}")
        out += ["", "  Counts from a few sessions are not a result. `summarize` reports intervals."]
    return "\n".join(out)


def watch(args):
    if args.once:
        print(snapshot(args.date))
        return
    try:
        while True:
            text = snapshot(args.date)
            sys.stdout.write("\033[2J\033[H" + text + f"\n\n  refreshing every {args.every}s · ctrl-c to leave (the run carries on)\n")
            sys.stdout.flush()
            time.sleep(args.every)
    except KeyboardInterrupt:
        print()


# ---------------------------------------------------------------- statistics

def load(date):
    cells = {}
    for p in result_files(RESULTS / date / "rig"):
        r = json.loads(p.read_text())
        run_no = int(p.parent.name[3:])
        cells.setdefault((r["harness"], r["condition"]), {}).setdefault(run_no, {})[r["instance"]] = r
    return cells


def coverage(date):
    """Planned, scored and set-aside sessions per harness, so a session that was
    never scored cannot vanish from the summary."""
    root = RESULTS / date / "rig"
    plan = json.loads((root / "plan.json").read_text())["sessions"] if (root / "plan.json").exists() else []
    lines = ["Sessions planned, scored and set aside (slept through, or change unreadable):", "",
             "| Harness | Planned | Scored | Set aside | Missing |", "|---|---|---|---|---|"]
    for h in sorted({s["harness"] for s in plan}):
        mine = [s for s in plan if s["harness"] == h]
        base = [root / h / s["condition"] / f"run{s['run']}" / s["instance"] for s in mine]
        scored = sum((b.parent / f"{b.name}.json").exists() for b in base)
        aside = sum(not (b.parent / f"{b.name}.json").exists() and any(
            (b.parent / f"{b.name}{x}").exists() for x in (".slept.json", ".unread.json")) for b in base)
        lines.append(f"| {h} | {len(mine)} | {scored} | {aside} | {len(mine) - scored - aside} |")
    return lines


def paired_bootstrap(a, b, n=10000, seed=7):
    """95% interval for mean(a) - mean(b) over tasks, resampling tasks.

    a and b map a task to its resolve rate across runs. Tasks are resampled
    together because the two harnesses faced the same ones: what varies between
    samples is which tasks were drawn, which is the uncertainty that matters.
    """
    tasks = sorted(set(a) & set(b))
    if not tasks:
        return None
    rng, diffs = random.Random(seed), []
    for _ in range(n):
        pick = [rng.choice(tasks) for _ in tasks]
        diffs.append(sum(a[t] - b[t] for t in pick) / len(pick))
    diffs.sort()
    return diffs[int(0.025 * n)], diffs[int(0.975 * n)]


def summarize(args):
    cells = load(args.date)
    if not cells:
        sys.exit("no results for that date")
    per_task, lines = {}, []
    lines.append("| Harness | Window | Runs | Resolved per run | Mean | SD | Tasks solved every run | Mean wall (s) |")
    lines.append("|---|---|---|---|---|---|---|---|")
    for (h, cond), runs in sorted(cells.items()):
        rates, walls = [], []
        tasks = sorted({t for r in runs.values() for t in r})
        for _, res in sorted(runs.items()):
            rates.append(100 * sum(x["score"]["resolved"] for x in res.values()) / len(res))
            walls += [x["wall_sec"] for x in res.values()]
        rate = {t: statistics.mean(int(runs[r][t]["score"]["resolved"]) for r in runs if t in runs[r]) for t in tasks}
        per_task[(h, cond)] = rate
        sd = statistics.stdev(rates) if len(rates) > 1 else float("nan")
        lines.append(f"| {h} | {cond} | {len(runs)} | {' / '.join(f'{x:.0f}%' for x in rates)} | "
                     f"{statistics.mean(rates):.1f}% | {sd:.1f} | {sum(v == 1 for v in rate.values())}/{len(tasks)} | "
                     f"{statistics.mean(walls):.0f} |")

    lines += ["", "Difference in mean resolve rate, paired by task, 95% bootstrap interval. "
                  "An interval that spans zero is not a difference.", "",
              "| Comparison | Window | Difference | 95% interval |", "|---|---|---|---|"]
    for cond in CONDITIONS:
        ref = per_task.get(("abhed", cond))
        for (h, c), rate in sorted(per_task.items()):
            if c != cond or h == "abhed" or not ref:
                continue
            ci = paired_bootstrap(ref, rate)
            common = sorted(set(ref) & set(rate))
            d = 100 * statistics.mean(ref[t] - rate[t] for t in common)
            verdict = "" if ci[0] > 0 or ci[1] < 0 else " (spans zero)"
            lines.append(f"| abhed − {h} | {cond} | {d:+.1f} pts | {100 * ci[0]:+.1f} to {100 * ci[1]:+.1f}{verdict} |")
    # By the dataset's difficulty band, so a run over the whole suite still
    # says how each harness did on the easy, medium and hard tasks separately.
    bands = {}
    for (h, cond), runs in cells.items():
        for res in runs.values():
            for t, x in res.items():
                band = x.get("difficulty") or "unrated"
                cell = bands.setdefault((band, h, cond), [0, 0])
                cell[0] += int(x["score"]["resolved"])
                cell[1] += 1
    if bands:
        lines += ["", "By difficulty band (sessions resolved / sessions run, all runs together):", "",
                  "| Band | Harness | Window | Resolved |", "|---|---|---|---|"]
        order = {"<15 min fix": 0, "15 min - 1 hour": 1, "1-4 hours": 2, ">4 hours": 3}
        for (band, h, cond), (won, n) in sorted(bands.items(), key=lambda kv: (order.get(kv[0][0], 9), kv[0][1], kv[0][2])):
            lines.append(f"| {band} | {h} | {cond} | {won}/{n} ({100 * won / n:.0f}%) |")
    lines += ["", *coverage(args.date)]
    text = "\n".join(lines)
    (RESULTS / args.date / "rig" / "SUMMARY.md").write_text(text + "\n")
    print(text)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("fetch").set_defaults(fn=fetch)
    p = sub.add_parser("prepare"); p.add_argument("--limit", type=int)
    p.add_argument("--repo", help="only instances whose repository contains this"); p.set_defaults(fn=prepare)
    sub.add_parser("validate").set_defaults(fn=validate)
    sub.add_parser("selftest").set_defaults(fn=selftest)
    p = sub.add_parser("models"); p.add_argument("--base", required=True); p.set_defaults(fn=models)
    p = sub.add_parser("doctor"); p.add_argument("--harness", required=True, choices=sorted(HARNESSES)); p.set_defaults(fn=doctor)
    p = sub.add_parser("run")
    p.add_argument("--date", required=True); p.add_argument("--runs", type=int, default=3)
    p.add_argument("--harness", action="append", choices=sorted(HARNESSES))
    p.add_argument("--condition", action="append", choices=sorted(CONDITIONS))
    p.add_argument("--limit", type=int); p.add_argument("--seed", type=int, default=1); p.add_argument("--force", action="store_true")
    p.add_argument("--difficulty", help="only tasks the dataset rates so: easy (<15 min fix), medium, hard, or the label itself")
    p.add_argument("--parallel", type=int, default=1, help="sessions at once; more than 1 only makes sense on a hosted model")
    p.set_defaults(fn=run)
    p = sub.add_parser("summarize"); p.add_argument("--date", required=True); p.set_defaults(fn=summarize)
    p = sub.add_parser("watch", help="live progress of a run")
    p.add_argument("--date", required=True); p.add_argument("--every", type=int, default=10)
    p.add_argument("--once", action="store_true", help="print one snapshot and exit"); p.set_defaults(fn=watch)
    args = ap.parse_args()
    install_stop_handlers()
    args.fn(args)


if __name__ == "__main__":
    main()
