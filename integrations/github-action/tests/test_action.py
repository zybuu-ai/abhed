"""Script-level tests for the GitHub Action: no runner, no GitHub, no model.

The install and run scripts are run as the action runs them, with a fake
release served from a file:// folder and a fake abhed that reports what it
was given. With ABHED_BIN set, run.sh is also run against the real binary on
a scripted local model, in run and review modes.
"""

import hashlib
import io
import json
import os
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
LIB = os.path.join(ROOT, "lib")
sys.path.insert(0, LIB)
sys.path.insert(0, HERE)

import abhed_action as aa  # noqa: E402

BIN = os.environ.get("ABHED_BIN", "")


class Tmp(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = os.path.realpath(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def path(self, *p):
        return os.path.join(self.tmp, *p)

    def write(self, rel, text, mode=0o644):
        p = self.path(rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, "w") as f:
            f.write(text)
        os.chmod(p, mode)
        return p

    def outputs(self, path):
        """Reads a GITHUB_OUTPUT file, heredoc values included."""
        out, lines, i = {}, open(path).read().split("\n"), 0
        while i < len(lines):
            l = lines[i]
            if "<<" in l:
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


# ---------------------------------------------------------------------------


class SettingsTest(Tmp):
    def test_provider_laid_over_policy_and_key_never_written(self):
        s = aa.build_settings('{"permissions": {"deny": ["web_fetch"]}, "model": {"default": "x"}}',
                              "anthropic", "claude-opus-5", "", "ABHED_ACTION_MODEL_KEY")
        self.assertEqual(s["permissions"], {"deny": ["web_fetch"]})
        self.assertEqual(s["model"]["default"], "github-action")
        self.assertEqual(s["model"]["providers"]["github-action"],
                         {"type": "anthropic", "model": "claude-opus-5", "api_key_env": "ABHED_ACTION_MODEL_KEY"})

    def test_policy_alone(self):
        self.assertEqual(aa.build_settings('{"limits": {"max_turns": 5}}', "", "", "", ""), {"limits": {"max_turns": 5}})

    def test_refusals(self):
        with self.assertRaises(ValueError):
            aa.build_settings("[1]", "", "", "", "")
        with self.assertRaises(ValueError):
            aa.build_settings("", "anthropic", "", "", "")

    def test_file_is_private(self):
        out = self.path("s.json")
        self.assertEqual(aa.main(["settings", "--out", out, "--provider-type", "openai", "--model", "m"]), 0)
        self.assertEqual(stat.S_IMODE(os.stat(out).st_mode), 0o600)


RESULT = {"type": "result", "subtype": "completed", "is_error": False, "result": "done\nexit-code=0",
          "session_id": "s-1", "num_turns": 3, "exit_code": 0,
          "usage": {"input_tokens": 10, "output_tokens": 2, "cost_usd": 0.01},
          "verdict": {"outcome": "unverified", "verified": 1, "unverified": 1, "contradicted": 0,
                      "claims": [{"text": "tests | pass", "verdict": "unverified", "reason": "no run"}]},
          "permission_denials": [{"tool": "bash", "reason": "no approver", "flag": "-allow 'bash(make)'"}]}


class ResultTest(Tmp):
    def test_outputs_summary_and_injection(self):
        run = self.write("run.jsonl", '{"type":"session.started"}\nnot json\n' + json.dumps(RESULT) + "\n")
        os.environ["GITHUB_OUTPUT"] = self.path("out")
        os.environ["GITHUB_STEP_SUMMARY"] = self.path("summary.md")
        try:
            self.assertEqual(aa.main(["result", "--run", run, "--evidence", self.tmp, "--exit-code", "8"]), 0)
        finally:
            del os.environ["GITHUB_OUTPUT"], os.environ["GITHUB_STEP_SUMMARY"]
        o = self.outputs(self.path("out"))
        self.assertEqual(o["exit-code"], "8")
        self.assertEqual(o["session-id"], "s-1")
        self.assertEqual(o["verdict"], "unverified")
        # A newline in the answer cannot set another output.
        self.assertEqual(o["result"], "done\nexit-code=0")
        md = open(self.path("summary.md")).read()
        self.assertIn("| Exit code | `8` |", md)
        self.assertIn("-allow 'bash(make)'", md)
        self.assertIn("tests \\| pass", md)
        self.assertEqual(json.load(open(self.path("summary.json")))["exit_code"], 8)

    def test_no_result_line(self):
        self.assertIn("no result line", aa.summary_markdown(None, 1))


DIFF = """diff --git a/app.go b/app.go
index 1111111..2222222 100644
--- a/app.go
+++ b/app.go
@@ -1,4 +1,5 @@
 package app
-func A() {}
+func A() error { return nil }
+func B() {}

 // end
diff --git a/new.txt b/new.txt
new file mode 100644
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+one
+two
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
--- a/gone.txt
+++ /dev/null
@@ -1 +0,0 @@
-bye
"""


class ReviewTest(Tmp):
    def test_commentable_lines(self):
        got = aa.commentable_lines(DIFF)
        self.assertEqual(got["app.go"], {1, 2, 3, 4, 5})
        self.assertEqual(got["new.txt"], {1, 2})
        self.assertNotIn("gone.txt", got)

    def review(self, findings, threshold="never"):
        return aa.build_review({"summary": "ok @maintainer", "findings": findings},
                               {"outcome": "verified", "verified": 2}, DIFF, "a" * 40, threshold, "https://run")

    def test_inline_and_loose_findings(self):
        r = self.review([
            {"path": "app.go", "line": 2, "severity": "major", "title": "error dropped", "body": "cc @octocat <!-- hide -->"},
            {"path": "app.go", "line": 40, "severity": "minor", "title": "far away", "body": "x"},
            {"path": "other.go", "line": 0, "severity": "nit", "title": "naming", "body": "y"},
        ])
        self.assertEqual(r["event"], "COMMENT")
        self.assertEqual(r["commit_id"], "a" * 40)
        self.assertEqual(len(r["comments"]), 1)
        c = r["comments"][0]
        self.assertEqual((c["path"], c["line"], c["side"]), ("app.go", 2, "RIGHT"))
        self.assertIn("`@octocat`", c["body"])
        self.assertNotIn("<!-- hide", c["body"])
        self.assertIn("`app.go` line 40", r["body"])
        self.assertIn("`other.go`", r["body"])
        self.assertIn("`@maintainer`", r["body"])
        self.assertTrue(r["body"].startswith(aa.MARKER))
        self.assertIn("never approves", r["body"])

    def test_event_never_approves(self):
        crit = [{"path": "app.go", "line": 3, "severity": "critical", "title": "t", "body": "b"}]
        self.assertEqual(self.review(crit)["event"], "COMMENT")
        self.assertEqual(self.review(crit, "major")["event"], "REQUEST_CHANGES")
        nit = [{"path": "app.go", "line": 3, "severity": "nit", "title": "t", "body": "b"}]
        self.assertEqual(self.review(nit, "major")["event"], "COMMENT")
        for t in ("never", "critical", "major", "minor", "nit"):
            self.assertNotEqual(self.review(crit + nit, t)["event"], "APPROVE")

    def test_bounds(self):
        many = [{"path": "app.go", "line": 1, "severity": "minor", "title": "t", "body": "b" * 9000}] * 80
        r = self.review(many)
        self.assertEqual(len(r["comments"]), 50)
        self.assertLessEqual(len(r["body"]), 60000)
        self.assertLessEqual(max(len(c["body"]) for c in r["comments"]), 4400)

    def test_no_review_delivered(self):
        with self.assertRaises(ValueError):
            aa.build_review(None, None, DIFF, "a" * 40, "never", "")

    def test_post_dry_run(self):
        res = dict(RESULT, structured_output={"summary": "s", "findings": []})
        run = self.write("run.jsonl", json.dumps(res) + "\n")
        diff = self.write("pr.diff", DIFF)
        self.assertEqual(aa.main(["review-post", "--result", run, "--diff", diff, "--commit", "b" * 40,
                                  "--dry-run", self.path("payload.json")]), 0)
        p = json.load(open(self.path("payload.json")))
        self.assertEqual((p["event"], p["comments"]), ("COMMENT", []))
        self.assertIn("No findings.", p["body"])
        # No review in the result: an error, nothing posted.
        run2 = self.write("run2.jsonl", json.dumps(RESULT) + "\n")
        self.assertEqual(aa.main(["review-post", "--result", run2, "--diff", diff, "--commit", "b" * 40,
                                  "--dry-run", self.path("p2.json")]), 1)

    def test_context(self):
        ev = {"pull_request": {"number": 7, "base": {"sha": "1" * 40}, "head": {"sha": "2" * 40}}}
        self.assertEqual(aa.review_context("pull_request", ev, ""), ("1" * 40, "2" * 40, 7))
        with self.assertRaises(ValueError):
            aa.review_context("pull_request_target", ev, "")
        got = aa.review_context("workflow_dispatch", {}, "9",
                                lambda p: {"number": 9, "base": {"sha": "3" * 40}, "head": {"sha": "4" * 40}} if p == "/pulls/9" else None)
        self.assertEqual(got, ("3" * 40, "4" * 40, 9))
        for bad in ("9; rm -rf /", ""):
            with self.assertRaises(ValueError):
                aa.review_context("workflow_dispatch", {}, bad, lambda p: {})
        with self.assertRaises(ValueError):
            aa.review_context("pull_request", {"pull_request": {"number": 1, "base": {"sha": "main"}, "head": {"sha": "2" * 40}}}, "")

    def test_prep_takes_the_diff_without_running_repository_config(self):
        repo = self.path("repo")
        os.makedirs(repo)
        g = lambda *a: subprocess.run(["git", "-c", "core.hooksPath=/dev/null", "-c", "user.name=t", "-c", "user.email=t@example.com", *a], cwd=repo, check=True, capture_output=True)
        g("init", "-q")
        self.write("repo/a.txt", "one\n")
        g("add", ".")
        g("commit", "-qm", "base")
        base = subprocess.run(["git", "rev-parse", "HEAD"], cwd=repo, capture_output=True, text=True).stdout.strip()
        # A textconv driver the repository asks for must not run.
        self.write("repo/.gitattributes", "*.txt diff=evil\n")
        subprocess.run(["git", "config", "diff.evil.textconv", f"touch {self.path('ran')};cat"], cwd=repo, check=True)
        self.write("repo/a.txt", "one\ntwo\n")
        g("add", ".")
        g("commit", "-qm", "head")
        head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=repo, capture_output=True, text=True).stdout.strip()
        if os.path.exists(self.path("ran")):
            os.remove(self.path("ran"))  # a global commit hook of the machine's may have diffed
        self.assertEqual(aa.main(["review-prep", "--workspace", repo, "--base", base, "--head", head,
                                  "--out", self.path("review"), "--instructions", "check errors"]), 0)
        diff = open(self.path("review", "pr.diff")).read()
        self.assertIn("+two", diff)
        self.assertFalse(os.path.exists(self.path("ran")))
        self.assertIn("check errors", open(self.path("review", "task.txt")).read())
        self.assertEqual(json.load(open(self.path("review", "schema.json")))["required"], ["summary", "findings"])


# ---------------------------------------------------------------------------


FAKE_ABHED = r"""#!/bin/sh
# Reports the arguments and environment of a -p run, then acts as one would.
out="$(dirname "$0")/report"
if [ "$1" = "-version" ]; then echo "abhed v0.0.0-fake"; exit 0; fi
case " $* " in
  *" record export "*) for a in "$@"; do last="$a"; done; echo '{"seq":1}' > "$last"; exit 0 ;;
  *" record verify "*) echo "verified"; exit 0 ;;
esac
for a in "$@"; do printf '%s\n' "$a"; done > "$out.args"
env > "$out.env"
echo '{"type":"session.started"}'
echo '{"type":"result","subtype":"unverified","session_id":"s-fake","num_turns":1,"exit_code":8,"verdict":{"outcome":"unverified"}}'
echo "abhed: a note on stderr" >&2
exit 8
"""


class InstallTest(Tmp):
    def release(self, version="v9.9.9", body=FAKE_ABHED):
        os.makedirs(self.path("rel", version))
        name = f"abhed-{version}-linux-amd64"
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w:gz") as tf:
            data = body.encode()
            ti = tarfile.TarInfo(f"{name}/abhed")
            ti.size, ti.mode = len(data), 0o755
            tf.addfile(ti, io.BytesIO(data))
        raw = buf.getvalue()
        with open(self.path("rel", version, name + ".tar.gz"), "wb") as f:
            f.write(raw)
        return hashlib.sha256(raw).hexdigest()

    def install(self, **env):
        e = {"PATH": os.environ["PATH"], "RUNNER_OS": "Linux", "RUNNER_ARCH": "X64", "RUNNER_TEMP": self.tmp,
             "ABHED_DOWNLOAD_BASE": "file://" + self.path("rel"), "GITHUB_OUTPUT": self.path("out"),
             "GITHUB_PATH": self.path("ghpath"), **env}
        return subprocess.run(["bash", os.path.join(LIB, "install.sh")], env=e, capture_output=True, text=True)

    def test_verified_archive_is_installed(self):
        sha = self.release()
        r = self.install(ABHED_VERSION="v9.9.9", ABHED_SHA256=sha.upper())
        self.assertEqual(r.returncode, 0, r.stderr)
        o = self.outputs(self.path("out"))
        self.assertTrue(o["path"].endswith("abhed-v9.9.9-linux-amd64/abhed"))
        self.assertTrue(os.access(o["path"], os.X_OK))
        self.assertIn("abhed-v9.9.9-linux-amd64", open(self.path("ghpath")).read())

    def test_wrong_hash_runs_nothing(self):
        self.release()
        r = self.install(ABHED_VERSION="v9.9.9", ABHED_SHA256="0" * 64)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("not the pinned", r.stderr)
        self.assertFalse(os.path.exists(self.path("abhed-install", "abhed-v9.9.9-linux-amd64")))

    def test_unpinned_version_needs_a_hash(self):
        self.release()
        r = self.install(ABHED_VERSION="v9.9.9")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("give its archive's SHA-256", r.stderr)

    def test_pinned_version_uses_the_table(self):
        with open(os.path.join(ROOT, "release.txt")) as f:
            pinned = dict(l.split() for l in f if l.strip() and not l.startswith("#"))
        self.assertRegex(pinned["version"], r"^v\d+\.\d+\.\d+$")
        for k in ("linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"):
            self.assertRegex(pinned[k], r"^[0-9a-f]{64}$")
        # The pinned version's archive here is not the real one: refused by hash.
        os.makedirs(self.path("rel", pinned["version"]))
        with open(self.path("rel", pinned["version"], f"abhed-{pinned['version']}-linux-amd64.tar.gz"), "wb") as f:
            f.write(b"not the release")
        r = self.install()
        self.assertNotEqual(r.returncode, 0)
        self.assertIn(pinned["linux-amd64"], r.stderr)

    def test_bad_version_and_platform(self):
        self.assertIn("is not a version", self.install(ABHED_VERSION="v1;rm", ABHED_SHA256="0" * 64).stderr)
        self.assertIn("Linux and macOS", self.install(RUNNER_OS="Windows").stderr)

    def test_abhed_path(self):
        fake = self.write("bin/abhed", FAKE_ABHED, 0o755)
        r = self.install(ABHED_PATH=fake)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.outputs(self.path("out"))["path"], fake)


class RunTest(Tmp):
    def setUp(self):
        super().setUp()
        self.fake = self.write("bin/abhed", FAKE_ABHED, 0o755)
        os.makedirs(self.path("ws"))

    def run_sh(self, **env):
        e = {"PATH": os.environ["PATH"], "HOME": self.path("home"), "RUNNER_TEMP": self.tmp,
             "GITHUB_OUTPUT": self.path("out"), "GITHUB_TOKEN": "ghs_secret_token", "ACTIONS_RUNTIME_TOKEN": "rt",
             "ABHED_BIN": self.fake, "ABHED_WORKSPACE": self.path("ws"), "ABHED_EVIDENCE_DIR": self.path("ev"),
             "ABHED_TASK": "-fix the tests", "ABHED_VERIFY": "strict", "ABHED_PERMISSION_MODE": "default",
             **env}
        return subprocess.run(["bash", os.path.join(LIB, "run.sh")], env=e, capture_output=True, text=True)

    def args(self):
        return open(self.path("bin", "report.args")).read().split("\n")[:-1]

    def env(self):
        return dict(l.split("=", 1) for l in open(self.path("bin", "report.env")).read().splitlines() if "=" in l)

    def test_flags_env_and_evidence(self):
        r = self.run_sh(ABHED_ALLOW="bash(go test*)\n  bash(go vet*), read \n", ABHED_DENY="web_fetch",
                        ABHED_KEY_ENV="ABHED_ACTION_MODEL_KEY", ABHED_ACTION_MODEL_KEY="sk-model",
                        ABHED_ENV_PASSTHROUGH="GOFLAGS", GOFLAGS="-mod=mod", ABHED_MAX_TURNS="12",
                        ABHED_SETTINGS=self.path("settings.json"))
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("a note on stderr", r.stderr)
        o = self.outputs(self.path("out"))
        self.assertEqual((o["exit-code"], o["session-id"], o["verdict"]), ("8", "s-fake", "unverified"))
        for f in ("run.jsonl", "stderr.log", "summary.json", "record.jsonl", "record-verify.txt", "abhed-version.txt"):
            self.assertTrue(os.path.exists(self.path("ev", f)), f)

    def test_the_p_run(self):
        # A fake that reports only the -p run.
        self.run_sh(ABHED_ALLOW="bash(go test*)\n  bash(go vet*), read \n\n", ABHED_DENY="web_fetch",
                    ABHED_KEY_ENV="ABHED_ACTION_MODEL_KEY", ABHED_ACTION_MODEL_KEY="sk-model",
                    ABHED_ENV_PASSTHROUGH="GOFLAGS", GOFLAGS="-mod=mod", ABHED_MAX_TURNS="12")
        a = self.args()
        self.assertEqual(a[:5], ["-C", self.path("ws"), "-p", "-no-stdin", "-output-format"])
        self.assertIn("strict", a[a.index("-verify") + 1])
        self.assertEqual([a[i + 1] for i, x in enumerate(a) if x == "-allow"], ["bash(go test*)", "bash(go vet*), read"])
        self.assertEqual([a[i + 1] for i, x in enumerate(a) if x == "-deny"], ["web_fetch"])
        # The task comes after --, so one starting with - is not read as a flag.
        self.assertEqual(a[-2:], ["--", "-fix the tests"])
        env = self.env()
        self.assertEqual(env["ABHED_ACTION_MODEL_KEY"], "sk-model")
        self.assertEqual(env["GOFLAGS"], "-mod=mod")
        self.assertEqual(env["HOME"], self.path("abhed-home"))
        for leaked in ("GITHUB_TOKEN", "ACTIONS_RUNTIME_TOKEN", "GITHUB_OUTPUT", "ABHED_TASK"):
            self.assertNotIn(leaked, env)

    def test_github_credentials_never_pass(self):
        for name in ("GITHUB_TOKEN", "ACTIONS_RUNTIME_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "GH_TOKEN"):
            r = self.run_sh(ABHED_ENV_PASSTHROUGH=name)
            self.assertEqual(r.returncode, 2, name)
            self.assertIn("never passed to the agent", r.stderr)
        self.assertIn("not a variable name", self.run_sh(ABHED_ENV_PASSTHROUGH="A;B").stderr)

    def test_refuses_a_persisted_checkout_token(self):
        subprocess.run(["git", "init", "-q"], cwd=self.path("ws"), check=True)
        subprocess.run(["git", "config", "http.https://github.com/.extraheader", "AUTHORIZATION: basic xxx"], cwd=self.path("ws"), check=True)
        r = self.run_sh()
        self.assertEqual(r.returncode, 2)
        self.assertIn("persist-credentials: false", r.stderr)
        self.assertFalse(os.path.exists(self.path("bin", "report.args")))
        self.assertEqual(self.run_sh(ABHED_ALLOW_PERSISTED_CREDENTIALS="true").returncode, 0)

    def test_review_reads_stdin(self):
        diff = self.write("pr.diff", DIFF)
        self.run_sh(ABHED_STDIN_FILE=diff, ABHED_TOOLS="read,glob,grep", ABHED_BARE="true",
                    ABHED_PERMISSION_MODE="plan", ABHED_JSON_SCHEMA='{"type":"object"}')
        a = self.args()
        self.assertNotIn("-no-stdin", a)
        self.assertEqual(a[a.index("-tools") + 1], "read,glob,grep")
        self.assertIn("-bare", a)
        self.assertEqual(a[a.index("-mode") + 1], "plan")


# ---------------------------------------------------------------------------

try:
    import composite
except ImportError:  # PyYAML is not installed
    composite = None

FAKE_REVIEW_ABHED = FAKE_ABHED.replace(
    """echo '{"type":"result","subtype":"unverified","session_id":"s-fake","num_turns":1,"exit_code":8,"verdict":{"outcome":"unverified"}}'
echo "abhed: a note on stderr" >&2
exit 8""",
    """cat > /dev/null
echo '{"type":"result","subtype":"completed","session_id":"s-rev","num_turns":2,"exit_code":0,"verdict":{"outcome":"verified"},"structured_output":{"summary":"ok","findings":[{"path":"a.txt","line":2,"severity":"major","title":"t","body":"b @someone"}]}}'
exit 0""")


class FakeGitHub:
    """The GitHub API endpoints the review uses, on 127.0.0.1."""

    def __init__(self, status=200):
        self.posts, self.status = [], status
        gh = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                body = json.dumps({"number": 5, "base": {"sha": gh.base}, "head": {"sha": gh.head}}).encode()
                self.send_response(200)
                self.end_headers()
                self.wfile.write(body)

            def do_POST(self):
                data = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                gh.posts.append((self.path, self.headers.get("Authorization"), json.loads(data)))
                self.send_response(gh.status)
                self.end_headers()
                self.wfile.write(b"{}")

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.url = "http://127.0.0.1:%d" % self.srv.server_address[1]
        self.base = self.head = ""

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


@unittest.skipIf(composite is None, "the composite runner needs PyYAML")
class CompositeTest(Tmp):
    """The whole action.yml, step by step, with abhed-path set."""

    def setUp(self):
        super().setUp()
        self.ws = self.path("ws")
        os.makedirs(self.ws)
        self.temp = self.path("runner-temp")
        os.makedirs(self.temp)

    def git_repo(self):
        g = lambda *a: subprocess.run(["git", "-c", "core.hooksPath=/dev/null", "-c", "user.name=t", "-c", "user.email=t@example.com", *a],
                                      cwd=self.ws, check=True, capture_output=True, text=True).stdout.strip()
        g("init", "-q")
        self.write("ws/a.txt", "one\n")
        g("add", ".")
        g("commit", "-qm", "base")
        base = g("rev-parse", "HEAD")
        self.write("ws/a.txt", "one\ntwo\n")
        g("commit", "-qam", "head")
        return base, g("rev-parse", "HEAD")

    def test_run_mode(self):
        fake = self.write("bin/abhed", FAKE_ABHED, 0o755)
        steps, uses, outputs, failed, summary = composite.run_action(
            {"task": "do it", "abhed-path": fake, "allow": "bash(make)", "model-provider": "openai",
             "model": "m", "model-api-key": "sk-xyz"}, workspace=self.ws, temp=self.temp)
        self.assertEqual(steps["run"]["outcome"], "success", steps["run"].get("stderr"))
        a = open(self.path("bin", "report.args")).read().split("\n")
        self.assertEqual(a[a.index("-verify") + 1], "strict")
        self.assertEqual(a[a.index("-mode") + 1], "default")
        settings = json.load(open(a[a.index("-settings") + 1]))
        self.assertEqual(settings["model"]["providers"]["github-action"]["api_key_env"], "ABHED_ACTION_MODEL_KEY")
        self.assertNotIn("sk-xyz", json.dumps(settings))
        self.assertIn("::add-mask::sk-xyz", steps["prep"]["stdout"])
        env = open(self.path("bin", "report.env")).read()
        self.assertIn("ABHED_ACTION_MODEL_KEY=sk-xyz", env)
        self.assertNotIn("ghs_job_token", env)
        self.assertEqual((outputs["exit-code"], outputs["verdict"], outputs["session-id"]), ("8", "unverified", "s-fake"))
        # abhed-path skips cosign; the upload is the only action used.
        self.assertEqual([u.split("@")[0] for u, _ in uses], ["actions/upload-artifact"])
        upload = [w for u, w in uses if u.startswith("actions/upload-artifact@")]
        self.assertEqual(upload[0]["path"], outputs["evidence-dir"])
        self.assertTrue(os.path.exists(os.path.join(outputs["evidence-dir"], "run.jsonl")))
        # The job fails on exit 8, after the upload.
        self.assertTrue(failed)
        self.assertEqual(steps["fail"]["outcome"], "failure")
        self.assertIn("Exit code | `8`", summary)

    def test_run_mode_passes_on_zero(self):
        fake = self.write("bin/abhed", FAKE_REVIEW_ABHED, 0o755)
        steps, uses, outputs, failed, _ = composite.run_action({"task": "x", "abhed-path": fake}, workspace=self.ws, temp=self.temp)
        self.assertFalse(failed, {k: v.get("stderr") for k, v in steps.items()})
        self.assertEqual(outputs["exit-code"], "0")

    def test_pull_request_target_is_refused(self):
        fake = self.write("bin/abhed", FAKE_ABHED, 0o755)
        steps, uses, outputs, failed, _ = composite.run_action({"task": "x", "abhed-path": fake}, workspace=self.ws,
                                                               temp=self.temp, github={"event_name": "pull_request_target"})
        self.assertTrue(failed)
        self.assertIn("refusing to run on pull_request_target", steps["prep"]["stdout"] + steps["prep"]["stderr"])
        self.assertEqual(uses, [])
        self.assertFalse(os.path.exists(self.path("bin", "report.args")))

    def test_fork_review_without_a_key_is_skipped(self):
        steps, uses, outputs, failed, _ = composite.run_action(
            {"mode": "review", "model-provider": "anthropic", "model": "m", "model-api-key": ""},
            workspace=self.ws, temp=self.temp, github={"event_name": "pull_request"})
        self.assertFalse(failed)
        self.assertIn("review skipped", steps["prep"]["stdout"])
        self.assertEqual(uses, [])
        self.assertEqual(steps["install"]["outcome"], "skipped")

    def review(self, fake_body, status=200):
        base, head = self.git_repo()
        gh = FakeGitHub(status)
        gh.base, gh.head = base, head
        self.addCleanup(gh.close)
        event = self.write("event.json", json.dumps({"pull_request": {"number": 5, "base": {"sha": base}, "head": {"sha": head}}}))
        fake = self.write("bin/abhed", fake_body, 0o755)
        res = composite.run_action(
            {"mode": "review", "abhed-path": fake, "model-provider": "openai", "model": "m", "model-api-key": "sk-r"},
            workspace=self.ws, temp=self.temp, github={"event_name": "pull_request", "api_url": gh.url},
            env={"GITHUB_EVENT_PATH": event})
        return gh, head, res

    def test_review_posts_one_review(self):
        gh, head, (steps, uses, outputs, failed, _) = self.review(FAKE_REVIEW_ABHED)
        self.assertFalse(failed, {k: v.get("stderr") for k, v in steps.items()})
        a = open(self.path("bin", "report.args")).read().split("\n")
        self.assertEqual(a[a.index("-mode") + 1], "plan")
        self.assertEqual(a[a.index("-tools") + 1], "read,glob,grep")
        self.assertEqual(a[a.index("-verify") + 1], "label")
        self.assertIn("-bare", a)
        self.assertIn("-json-schema", a)
        self.assertNotIn("ghs_job_token", open(self.path("bin", "report.env")).read())
        self.assertEqual(len(gh.posts), 1)
        path, auth, payload = gh.posts[0]
        self.assertEqual(path, "/repos/acme/widgets/pulls/5/reviews")
        self.assertEqual(auth, "Bearer ghs_job_token")
        self.assertEqual((payload["event"], payload["commit_id"]), ("COMMENT", head))
        self.assertEqual([(c["path"], c["line"]) for c in payload["comments"]], [("a.txt", 2)])
        self.assertIn("`@someone`", payload["comments"][0]["body"])

    def test_review_post_refused_fails_the_job(self):
        gh, head, (steps, uses, outputs, failed, _) = self.review(FAKE_REVIEW_ABHED, status=403)
        self.assertTrue(failed)
        self.assertIn("read-only token", steps["post"]["stderr"])
        # The evidence is still uploaded.
        self.assertTrue(any(u.startswith("actions/upload-artifact@") for u, _ in uses))


# ---------------------------------------------------------------------------


class ScriptedModel:
    """An OpenAI-compatible stub answering each request with the next reply."""

    def __init__(self, replies):
        self.replies = list(replies)
        model = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", "0") or 0))
                reply = model.replies.pop(0) if model.replies else {"text": "done"}
                if "tool" in reply:
                    chunks = [{"choices": [{"delta": {"tool_calls": [{"index": 0, "id": reply.get("id", "c1"), "function": {
                        "name": reply["tool"], "arguments": json.dumps(reply["args"])}}]}}]},
                              {"choices": [{"delta": {}, "finish_reason": "tool_calls"}]}]
                else:
                    chunks = [{"choices": [{"delta": {"content": reply["text"]}, "finish_reason": "stop"}]}]
                body = ("".join("data: %s\n\n" % json.dumps(c) for c in chunks) + "data: [DONE]\n\n").encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.url = "http://127.0.0.1:%d/v1" % self.srv.server_address[1]

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


@unittest.skipUnless(BIN, "set ABHED_BIN to an abhed binary to run against the real engine")
class EndToEnd(Tmp):
    def run_real(self, model, **env):
        ws = self.path("ws")
        os.makedirs(ws, exist_ok=True)
        settings = self.path("settings.json")
        with open(settings, "w") as f:
            json.dump(aa.build_settings("", "openai-compatible", "m", model.url, ""), f)
        e = {"PATH": os.environ["PATH"], "HOME": self.path("home"), "RUNNER_TEMP": self.tmp,
             "GITHUB_OUTPUT": self.path("out"), "ABHED_BIN": BIN, "ABHED_WORKSPACE": ws,
             "ABHED_EVIDENCE_DIR": self.path("ev"), "ABHED_SETTINGS": settings, **env}
        r = subprocess.run(["bash", os.path.join(LIB, "run.sh")], env=e, capture_output=True, text=True, timeout=300)
        self.assertEqual(r.returncode, 0, r.stderr)
        return self.outputs(self.path("out")), r

    def test_run_mode_records_and_verifies(self):
        m = ScriptedModel([{"tool": "bash", "args": {"command": "touch made-in-ci", "description": "probe"}}, {"text": "done"}])
        self.addCleanup(m.close)
        o, r = self.run_real(m, ABHED_TASK="make a file", ABHED_PERMISSION_MODE="default",
                             ABHED_ALLOW="bash(touch made-in-ci)", ABHED_VERIFY="label")
        self.assertEqual(o["exit-code"], "0", r.stderr)
        self.assertTrue(o["session-id"])
        self.assertTrue(os.path.exists(self.path("ws", "made-in-ci")))
        self.assertTrue(os.path.getsize(self.path("ev", "record.jsonl")) > 0, open(self.path("ev", "export.log")).read())
        self.assertIn("<!doctype html", open(self.path("ev", "transcript.html")).read().lower())
        v = subprocess.run([BIN, "record", "verify", self.path("ev", "record.jsonl")], capture_output=True, text=True,
                           env={"PATH": os.environ["PATH"], "HOME": self.path("home2")})
        self.assertEqual(v.returncode, 0, v.stdout + v.stderr)

    def test_run_mode_refuses_what_is_not_allowed(self):
        m = ScriptedModel([{"tool": "bash", "args": {"command": "touch not-allowed", "description": "probe"}}, {"text": "done"}])
        self.addCleanup(m.close)
        o, r = self.run_real(m, ABHED_TASK="make a file", ABHED_PERMISSION_MODE="default", ABHED_VERIFY="label")
        self.assertFalse(os.path.exists(self.path("ws", "not-allowed")))
        res = json.load(open(self.path("ev", "summary.json")))["result"]
        self.assertTrue(res.get("permission_denials"), res)

    def test_review_mode_end_to_end(self):
        os.makedirs(self.path("ws"))
        with open(self.path("ws", "app.go"), "w") as f:
            f.write("package app\nfunc A() error { return nil }\nfunc B() {}\n\n// end\n")
        review = {"summary": "One problem.", "findings": [
            {"path": "app.go", "line": 2, "severity": "major", "title": "error never set", "body": "A always returns nil; cc @octocat"}]}
        m = ScriptedModel([{"tool": "read", "args": {"path": "app.go"}},
                           {"tool": "bash", "id": "c2", "args": {"command": "touch should-not-run", "description": "x"}},
                           {"tool": "result", "id": "c3", "args": review}])
        self.addCleanup(m.close)
        diff = self.write("pr.diff", DIFF)
        o, r = self.run_real(m, ABHED_TASK=aa.REVIEW_TASK, ABHED_STDIN_FILE=diff, ABHED_TOOLS="read,glob,grep",
                             ABHED_BARE="true", ABHED_PERMISSION_MODE="plan", ABHED_VERIFY="label",
                             ABHED_JSON_SCHEMA=json.dumps(aa.REVIEW_SCHEMA))
        self.assertEqual(o["exit-code"], "0", open(self.path("ev", "stderr.log")).read())
        self.assertFalse(os.path.exists(self.path("ws", "should-not-run")))
        self.assertEqual(aa.main(["review-post", "--result", self.path("ev", "run.jsonl"), "--diff", diff,
                                  "--commit", "c" * 40, "--dry-run", self.path("payload.json")]), 0)
        p = json.load(open(self.path("payload.json")))
        self.assertEqual(p["event"], "COMMENT")
        self.assertEqual([(c["path"], c["line"]) for c in p["comments"]], [("app.go", 2)])
        self.assertIn("`@octocat`", p["comments"][0]["body"])


if __name__ == "__main__":
    unittest.main()
