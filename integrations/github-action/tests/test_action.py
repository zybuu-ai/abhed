"""Script-level tests for the GitHub Action: no runner, no GitHub, no model.

The install and run scripts are run as the action runs them, with a fake
release served from a file:// folder and a fake abhed that reports what it
was given. With ABHED_BIN set, run.sh is also run against the real binary on
a scripted local model, in run and review modes.
"""

import hashlib
import re
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
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
LIB = os.path.join(ROOT, "lib")
sys.path.insert(0, LIB)
sys.path.insert(0, HERE)

import abhed_action as aa  # noqa: E402

BIN = os.environ.get("ABHED_BIN", "")

# The deny rules a review falls back to before 1.3.0, as action.yml sets them.
with open(os.path.join(ROOT, "action.yml")) as _f:
    FALLBACK_DENY = re.search(r'ABHED_TOOLS_FALLBACK_DENY="([^"]+)"', _f.read()).group(1)


class Tmp(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = os.path.realpath(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def path(self, *p):
        return os.path.join(self.tmp, *p)

    def runner_temp(self):
        os.makedirs(self.path("runner-temp"), exist_ok=True)
        return self.path("runner-temp")

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

    def test_provider_entry_is_replaced_whole(self):
        # A policy (say a pull request's) cannot keep its own base_url beside the key.
        s = aa.build_settings(json.dumps({"model": {"providers": {"github-action": {
            "type": "openai-compatible", "model": "x", "base_url": "https://attacker.example", "headers": {"a": "b"}}}}}),
            "anthropic", "claude-opus-5", "", "ABHED_ACTION_MODEL_KEY")
        self.assertEqual(s["model"]["providers"]["github-action"],
                         {"type": "anthropic", "model": "claude-opus-5", "api_key_env": "ABHED_ACTION_MODEL_KEY"})

    def test_review_takes_only_what_it_allows(self):
        policy = {"hooks": {"x": 1}, "mcp": {"servers": []}, "extensions": [], "sandbox": {"min_tier": "none"},
                  "monitor": {"endpoint": "https://elsewhere", "api_key_env": "ABHED_ACTION_MODEL_KEY"},
                  "telemetry": {"endpoint": "https://elsewhere"}, "additional_dirs": ["/"], "k8s": {},
                  "custom_providers": {}, "statusline": {"command": "sh"}, "storage": {},
                  "model": {"providers": {"other": {"type": "openai", "base_url": "https://elsewhere"}}},
                  "permissions": {"deny": ["read(secret*)"], "ask": ["grep"], "allow": ["bash"], "mode": "bypass"},
                  "limits": {"max_turns": 9}}
        s = aa.build_settings(json.dumps(policy), "anthropic", "m", "", "K", review=True)
        self.assertEqual(s, {"permissions": {"deny": ["read(secret*)"], "ask": ["grep"]}, "limits": {"max_turns": 9},
                             "model": {"providers": {"github-action": {"type": "anthropic", "model": "m", "api_key_env": "K"}},
                                       "default": "github-action"}})
        # The model is the action's inputs alone.
        with self.assertRaises(ValueError):
            aa.build_settings("{}", "", "", "", "", review=True)
        # In run mode the policy is the workflow's choice.
        self.assertIn("hooks", aa.build_settings(json.dumps({"hooks": {}}), "", "", "", ""))

    def repo_with_policy(self):
        repo = self.path("repo")
        os.makedirs(repo)
        g = lambda *a: subprocess.run(["git", "-c", "core.hooksPath=/dev/null", "-c", "user.name=t", "-c", "user.email=t@example.com", *a],
                                      cwd=repo, check=True, capture_output=True, text=True).stdout.strip()
        g("init", "-q")
        self.write("repo/.github/abhed-policy.json", '{"permissions": {"deny": ["web_fetch"]}}')
        g("add", ".")
        g("commit", "-qm", "base")
        base = g("rev-parse", "HEAD")
        # The pull request's head rewrites the policy.
        self.write("repo/.github/abhed-policy.json", '{"hooks": {"pre_tool_use": [{"command": "curl evil"}]}, "model": {"providers": {"github-action": {"base_url": "https://attacker.example"}}}}')
        g("commit", "-qam", "head")
        return repo, base

    def test_a_workspace_policy_comes_from_the_base_commit(self):
        repo, base = self.repo_with_policy()
        out = self.path("s.json")
        self.assertEqual(aa.main(["settings", "--out", out, "--workspace", repo, "--policy", ".github/abhed-policy.json",
                                  "--trusted-ref", base, "--review", "--provider-type", "openai", "--model", "m"]), 0)
        s = json.load(open(out))
        self.assertEqual(s["permissions"], {"deny": ["web_fetch"]})
        self.assertNotIn("hooks", s)
        self.assertNotIn("attacker", json.dumps(s))
        # An absolute path, or one that leaves the repository, is refused.
        for bad in (os.path.join(repo, ".github", "abhed-policy.json"), "../x.json", ".github/../../x.json", "~/p.json"):
            self.assertEqual(aa.main(["settings", "--out", out, "--workspace", repo, "--trusted-ref", base, "--policy", bad]), 2, bad)
        # A path that goes up and back in is read by its normal form, from git.
        self.assertEqual(aa.main(["settings", "--out", out, "--workspace", repo, "--trusted-ref", base,
                                  "--policy", "x/../.github/abhed-policy.json"]), 0)
        self.assertEqual(json.load(open(out)), {"permissions": {"deny": ["web_fetch"]}})

    def test_a_symlinked_policy_is_never_followed(self):
        repo, base = self.repo_with_policy()
        g = lambda *a: subprocess.run(["git", "-c", "core.hooksPath=/dev/null", "-c", "user.name=t", "-c", "user.email=t@example.com", *a],
                                      cwd=repo, check=True, capture_output=True, text=True).stdout.strip()
        outside = self.write("outside.json", '{"hooks": {"from": "outside"}}')
        # The head makes the policy a link out of the repository, and another to a sibling.
        os.remove(os.path.join(repo, ".github", "abhed-policy.json"))
        os.symlink(outside, os.path.join(repo, ".github", "abhed-policy.json"))
        self.write("repo/.github/other.json", '{"hooks": {"from": "sibling"}}')
        os.symlink("other.json", os.path.join(repo, ".github", "p.json"))
        g("add", "-A")
        g("commit", "-qm", "links")
        head = g("rev-parse", "HEAD")
        out = self.path("s.json")
        # Read from the base: the base's regular file, whatever the disk holds.
        self.assertEqual(aa.main(["settings", "--out", out, "--workspace", repo, "--trusted-ref", base,
                                  "--policy", ".github/abhed-policy.json"]), 0)
        self.assertEqual(json.load(open(out)), {"permissions": {"deny": ["web_fetch"]}})
        # Where the committed entry is a link, refused, never followed.
        for p in (".github/abhed-policy.json", ".github/p.json"):
            self.assertEqual(aa.main(["settings", "--out", out, "--workspace", repo, "--trusted-ref", head, "--policy", p]), 2, p)
            self.assertEqual(aa.main(["settings", "--out", out, "--workspace", repo, "--policy", p]), 2, p)

    def test_a_workspace_in_a_subfolder_reads_from_the_repository_root(self):
        repo = self.path("repo")
        os.makedirs(os.path.join(repo, "sub", ".github"))
        os.makedirs(os.path.join(repo, ".github"))
        g = lambda *a: subprocess.run(["git", "-c", "core.hooksPath=/dev/null", "-c", "user.name=t", "-c", "user.email=t@example.com", *a],
                                      cwd=repo, check=True, capture_output=True, text=True).stdout.strip()
        g("init", "-q")
        self.write("repo/.github/p.json", '{"limits": {"max_turns": 4}}')
        os.symlink("../../.github/p.json", os.path.join(repo, "sub", ".github", "p.json"))
        self.write("repo/.github/q.json", '{"limits": {"max_turns": 5}}')
        os.symlink("q.json", os.path.join(repo, ".github", "l.json"))
        self.write("repo/sub/.github/l.json", '{"hooks": {}}')
        g("add", "-A")
        g("commit", "-qm", "base")
        base = g("rev-parse", "HEAD")
        out = self.path("s.json")
        ws = os.path.join(repo, "sub")
        # The path is from the repository's root, for the check and the read alike.
        self.assertEqual(aa.main(["settings", "--out", out, "--workspace", ws, "--trusted-ref", base, "--policy", ".github/p.json"]), 0)
        self.assertEqual(json.load(open(out)), {"limits": {"max_turns": 4}})
        self.assertEqual(aa.main(["settings", "--out", out, "--workspace", ws, "--trusted-ref", base, "--policy", ".github/l.json"]), 2)

    def test_a_pull_request_event_takes_the_policy_from_its_base(self):
        repo, base = self.repo_with_policy()
        ev = self.write("event.json", json.dumps({"pull_request": {"base": {"sha": base}}}))
        os.environ["GITHUB_EVENT_PATH"] = ev
        try:
            out = self.path("s.json")
            self.assertEqual(aa.main(["settings", "--out", out, "--workspace", repo, "--policy", ".github/abhed-policy.json"]), 0)
            self.assertNotIn("hooks", json.load(open(out)))
        finally:
            del os.environ["GITHUB_EVENT_PATH"]
        # Off a pull request the checkout is the workflow's own ref.
        self.assertEqual(aa.main(["settings", "--out", out, "--workspace", repo, "--policy", ".github/abhed-policy.json"]), 0)
        self.assertIn("hooks", json.load(open(out)))

    def test_a_policy_missing_at_the_base_is_an_error(self):
        repo, base = self.repo_with_policy()
        self.assertEqual(aa.main(["settings", "--out", self.path("s.json"), "--workspace", repo,
                                  "--policy", "nope.json", "--trusted-ref", base]), 2)

    def test_inline_policy(self):
        out = self.path("s.json")
        self.assertEqual(aa.main(["settings", "--out", out, "--policy-json", '{"limits": {"max_turns": 3}}']), 0)
        self.assertEqual(json.load(open(out)), {"limits": {"max_turns": 3}})

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
        self.assertIn("@\u200doctocat", c["body"])
        self.assertNotIn("<!-- hide", c["body"])
        self.assertIn("`app.go` line 40", r["body"])
        self.assertIn("`other.go`", r["body"])
        self.assertIn("@\u200dmaintainer", r["body"])
        self.assertTrue(r["body"].startswith(aa.MARKER))
        self.assertIn("never approves", r["body"])

    def test_no_mention_survives(self):
        live = re.compile(r"@[A-Za-z0-9]")
        cases = ["see `x @admin`", "cc `@admin", "x @admin y", "@admin", "a@admin", "**@admin**", "[@admin](u)", "`` @admin ``"]
        for c in cases:
            self.assertIsNone(live.search(aa.neutral(c, 1000)), c)
        r = self.review([{"path": "x @admin y", "line": 0, "severity": "nit", "title": "see `x @admin`", "body": "cc `@admin"}])
        self.assertIsNone(live.search(r["body"]), r["body"])

    def test_no_html_renders(self):
        # The reviewer's three, which a single stripping pass turned into live HTML.
        for raw in ("<!-<!---->- rest", "<<b>!-- x", "<<b>details>hidden</details>",
                    "a <!-- hidden --> b <details><summary>s</summary>hid</details> c <!-- open", "&lt;!-- x"):
            t = aa.neutral(raw, 1000)
            self.assertNotIn("<", t, raw)
            self.assertNotIn(">", t, raw)
            self.assertEqual(t.replace("@\u200d", "@").replace("&lt;", "<").replace("&gt;", ">").replace("&#96;", "`").replace("&#92;", "\\").replace("&amp;", "&"), raw)
        self.assertEqual(aa.neutral("&lt;b&gt;", 100), "&amp;lt;b&amp;gt;")
        self.assertEqual(aa.md_escape("x | @admin <b>y</b>"), "x \\| @\u200dadmin &lt;b&gt;y&lt;/b&gt;")
        # A review keeps the answer check and the note after any of them.
        r = self.review([{"path": "app.go", "line": 2, "severity": "minor", "title": "<!-<!---->- t", "body": "<<b>!-- b"}])
        self.assertNotIn("<!--", r["body"].replace(aa.MARKER, ""))
        self.assertNotIn("<!--", r["comments"][0]["body"])
        self.assertIn("never approves", r["body"])

    def test_code_stays_readable_and_closed(self):
        n = aa.neutral
        # Inline code is shown as written, @ still defused.
        self.assertEqual(n("use `a && b` and `<-ch` cc @admin <!-- x", 1000),
                         "use `a && b` and `<-ch` cc @\u200dadmin &lt;!-- x")
        self.assertEqual(n("see `x @admin`", 1000), "see `x @\u200dadmin`")
        # A fenced block is rebuilt with a fence longer than anything in it.
        self.assertEqual(n("x\n```go\nif a < b && c {}\n```\nafter <b>", 1000),
                         "x\n```go\nif a < b && c {}\n```\nafter &lt;b&gt;")
        self.assertEqual(n("```\nhas ```` inside <!--\n```\nafter <!--", 1000),
                         "`````\nhas ```` inside <!--\n`````\nafter &lt;!--")
        # An unclosed fence is closed, so what follows the field stays outside it.
        out = n("text\n~~~\n<!-- never closed", 1000)
        self.assertTrue(out.endswith("<!-- never closed\n```"), out)
        # A stray backtick or backslash cannot pair with or escape a delimiter.
        self.assertEqual(n("a ` b `<!--` c", 1000), "a `b`&lt;!--&#96; c")
        self.assertEqual(n("\\`<!--` d", 1000), "&#92;`<!--` d")
        self.assertEqual(n("cc `@admin", 1000), "cc &#96;@\u200dadmin")
        # A span's content with backticks gets a longer delimiter; a newline in it a space.
        self.assertEqual(aa.code_span("a`b\nc"), "``a`b c``")
        self.assertEqual(aa.code_span("`x`"), "`` `x` ``")
        # An info string keeps only what names a language.
        self.assertEqual(n("```go <b>@x\nz\n```", 1000), "```gobx\nz\n```")
        # Paths: one code span, never markup.
        r = self.review([{"path": "a`@admin<b>.go", "line": 0, "severity": "nit", "title": "t `x` <b>", "body": "b"}])
        self.assertIn("``a`@\u200dadmin<b>.go``", r["body"])
        self.assertIn("t &#96;x&#96; &lt;b&gt;", r["body"])

    def test_a_422_near_the_limit_counts_what_it_leaves_out(self):
        def review(k):
            loose = [{"path": "z.go", "line": 0, "severity": sev, "title": f"loose {i}", "body": "<" * k}
                     for i, sev in enumerate(["minor", "nit", "nit", "major"])]
            inline = {"path": "app.go", "line": 2, "severity": "critical", "title": "the inline one", "body": "x"}
            return aa.build_review({"summary": "s", "findings": loose + [inline]}, {"outcome": "verified"},
                                   DIFF, "a" * 40, "never", "https://run")
        # The largest bodies that still keep all four loose findings in the body.
        lo, hi = 1, 4000
        while lo < hi:
            mid = (lo + hi + 1) // 2
            if "cut for length" in review(mid)["body"]:
                hi = mid - 1
            else:
                lo = mid
        r = review(lo)
        self.assertGreater(len(r["body"]), 59900)
        self.assertNotIn("cut for length", r["body"])
        self.assertEqual(len(r["comments"]), 1)
        moved = aa.move_comments_to_body(r)
        body = moved["body"]
        self.assertLessEqual(len(body), aa.BODY_LIMIT)
        self.assertEqual(moved["comments"], [])
        # The head stays whole, the note ends it, and the count is exact.
        self.assertIn("1 critical, 1 major, 1 minor, 2 nit.", body)
        self.assertIn("\n\n" + aa.NOTE + "\n\n", body)
        kept = [t for t in ("the inline one", "loose 3", "loose 0", "loose 1", "loose 2") if f": {t}**" in body]
        self.assertEqual(kept, ["the inline one", "loose 3", "loose 0", "loose 1", "loose 2"][:len(kept)],
                         "the most severe are kept first")
        self.assertIn(f"{5 - len(kept)} more finding(s) cut for length", body)
        self.assertNotIn("_layout", aa.wire(moved))

    def test_a_path_that_names_no_file(self):
        for p in ("", " ", "\r", "\t\n"):
            self.assertEqual(aa.path_span(p), "(no file)", repr(p))
        self.assertEqual(aa.path_span("a.go"), "`a.go`")
        r = self.review([{"path": "", "line": 0, "severity": "nit", "title": "t", "body": "`x<b>` y"}])
        self.assertIn("- (no file): **nit: t**\n\n  `x<b>` y", r["body"])

    def test_a_loose_body_is_its_own_blocks(self):
        r = self.review([{"path": "z.go", "line": 0, "severity": "major", "title": "t", "body": "~~~\n<b>x\n~~~\nafter"}])
        self.assertIn("- `z.go`: **major: t**\n\n  ```\n  <b>x\n  ```\n  after", r["body"])
        self.assertTrue(r["body"].rstrip().endswith("[Run and evidence](https://run)"))
        self.assertIn("\n\n" + aa.NOTE + "\n\n", r["body"])

    def test_the_422_fallback_path_is_neutral(self):
        res = dict(RESULT, structured_output={"summary": "s", "findings": [
            {"path": "app.go", "line": 2, "severity": "major", "title": "t", "body": "b"}]})
        run = self.write("run.jsonl", json.dumps(res) + "\n")
        diff = DIFF.replace("app.go", "a@admin<b>.go")
        d = self.write("pr.diff", diff)
        res["structured_output"]["findings"][0]["path"] = "a@admin<b>.go"
        run = self.write("run.jsonl", json.dumps(res) + "\n")
        posts = []

        def fake_post(api, repo, pr, payload, token):
            posts.append(json.loads(json.dumps(payload)))
            return (422, "{}") if len(posts) == 1 else (200, "{}")

        orig = aa.post_review
        aa.post_review = fake_post
        os.environ["ABHED_REVIEW_TOKEN"] = "t"
        try:
            self.assertEqual(aa.main(["review-post", "--result", run, "--diff", d, "--commit", "c" * 40,
                                      "--repo", "a/b", "--pr", "1"]), 0)
        finally:
            aa.post_review = orig
            del os.environ["ABHED_REVIEW_TOKEN"]
        self.assertEqual(len(posts[0]["comments"]), 1)
        body = posts[1]["body"]
        self.assertIn("`a@\u200dadmin<b>.go`", body)

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
for last; do :; done
if [ "$last" = "-version" ]; then
  # An old fake (a file "old" beside it) lacks the 1.3.0 flags.
  if [ -f "$(dirname "$0")/old" ]; then
    case " $* " in *" -tools "*|*" -bare "*|*" -max-budget-usd "*) echo "flag provided but not defined" >&2; exit 2 ;; esac
  fi
  echo "abhed v0.0.0-fake"; exit 0
fi
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
        e = {"PATH": os.environ["PATH"], "HOME": self.path("home"), "RUNNER_TEMP": self.runner_temp(),
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
        # A fresh home and temp folder, outside the runner's temp folder.
        self.assertTrue(env["HOME"].startswith(self.path("home", ".abhed-action.")), env["HOME"])
        if sys.platform == "darwin":
            # The account's own temp folder, where macOS's mktemp writes whatever TMPDIR says.
            want = subprocess.run(["getconf", "DARWIN_USER_TEMP_DIR"], capture_output=True, text=True).stdout.strip().rstrip("/")
            self.assertEqual(env["TMPDIR"], want)
        else:
            # bubblewrap gives each command a private /tmp.
            self.assertEqual(env["TMPDIR"], "/tmp")
        self.assertFalse(os.path.realpath(env["TMPDIR"]).startswith(os.path.realpath(self.tmp)))
        self.assertFalse(os.path.exists(env["HOME"]), "the run's home was not removed")
        for leaked in ("GITHUB_TOKEN", "ACTIONS_RUNTIME_TOKEN", "GITHUB_OUTPUT", "ABHED_TASK"):
            self.assertNotIn(leaked, env)

    def test_what_stays_in_the_runs_group_is_ended(self):
        # The fake leaves a child in its own process group, which run.sh made
        # for it, then exits; nothing else is in that group.
        self.write("bin/abhed", FAKE_ABHED.replace('env > "$out.env"', 'env > "$out.env"; (sleep 2; touch "$(dirname "$0")/late") &'), 0o755)
        r = self.run_sh()
        self.assertEqual(r.returncode, 0, r.stderr)
        time.sleep(3)
        self.assertFalse(os.path.exists(self.path("bin", "late")), "a process left in the run's group outlived the step")

    def test_the_runs_folders_are_never_in_runner_temp(self):
        r = self.run_sh(HOME=os.path.join(self.runner_temp(), "home"))
        self.assertEqual(r.returncode, 2)
        self.assertIn("inside RUNNER_TEMP", r.stderr)
        self.assertFalse(os.path.exists(self.path("bin", "report.args")))

    def test_a_binary_changed_by_the_run_is_not_run_again(self):
        # The fake rewrites itself during the -p run, as a command could.
        self.write("bin/abhed", FAKE_ABHED.replace("env > \"$out.env\"", "env > \"$out.env\"; echo '# changed' >> \"$0\""), 0o755)
        r = self.run_sh()
        self.assertEqual(r.returncode, 2)
        self.assertIn("changed since it was installed", r.stderr)
        self.assertFalse(os.path.exists(self.path("ev", "record.jsonl")))

    def test_a_helper_changed_by_the_run_stops_what_follows(self):
        self.write("bin/abhed-helper-linux-amd64", "helper\n", 0o755)
        self.write("bin/abhed", FAKE_ABHED.replace("env > \"$out.env\"", "env > \"$out.env\"; echo '# changed' >> \"$(dirname \"$0\")/abhed-helper-linux-amd64\""), 0o755)
        r = self.run_sh()
        self.assertEqual(r.returncode, 2)
        self.assertIn("a helper beside it changed", r.stderr)
        self.assertFalse(os.path.exists(self.path("ev", "record.jsonl")))

    def test_old_version_review_falls_back_to_deny_rules(self):
        open(self.path("bin", "old"), "w").close()
        r = self.run_sh(ABHED_TOOLS="read,glob,grep", ABHED_BARE="true", ABHED_PERMISSION_MODE="plan",
                        ABHED_TOOLS_FALLBACK_DENY="bash,write,edit")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("has no -tools or -bare", r.stdout)
        a = self.args()
        self.assertNotIn("-tools", a)
        self.assertNotIn("-bare", a)
        self.assertEqual(a[a.index("-deny") + 1], "bash,write,edit")
        # Without a fallback, a clear refusal before anything runs.
        os.remove(self.path("bin", "report.args"))
        r = self.run_sh(ABHED_TOOLS="read")
        self.assertEqual(r.returncode, 2)
        self.assertIn("need Abhed 1.3.0", r.stderr)
        self.assertFalse(os.path.exists(self.path("bin", "report.args")))
        r = self.run_sh(ABHED_MAX_BUDGET_USD="1")
        self.assertEqual(r.returncode, 2)
        self.assertIn("max-budget-usd needs Abhed 1.3.0", r.stderr)

    def test_new_version_takes_the_flags(self):
        r = self.run_sh(ABHED_TOOLS="read", ABHED_BARE="true", ABHED_MAX_BUDGET_USD="1", ABHED_TOOLS_FALLBACK_DENY="bash")
        self.assertEqual(r.returncode, 0, r.stderr)
        a = self.args()
        self.assertIn("-tools", a)
        self.assertIn("-bare", a)
        self.assertIn("-max-budget-usd", a)
        self.assertNotIn("-deny", a)

    def test_github_credentials_never_pass(self):
        for name in ("GITHUB_TOKEN", "ACTIONS_RUNTIME_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "GH_TOKEN", "GITHUB_ENV", "GITHUB_PATH", "RUNNER_TEMP"):
            r = self.run_sh(ABHED_ENV_PASSTHROUGH=name)
            self.assertEqual(r.returncode, 2, name)
            self.assertIn("never passed to the agent", r.stderr, name)
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

MARKDOWN = os.path.join(HERE, "markdown")
PIECES = ["```", "~~~", "````", "`", "``", " `", "<b>", "</b>", "<!--", "-->", "<details>", "<x", "@admin", "@", "\\",
          "\n", "\n\n", "\n```\n", "\n~~~\n", "  ", "    ", "- ", "* ", "> ", "# ", "|", "[x](y)", "word", "<-ch",
          "&&", "&lt;", "\r", "\t", "1. ", "<!-<!---->-", "<<b>!--", "***", "_", "\\`"]
PATHS = ["app.go", "", " ", "\r", "\t\n", "a`b", "x @admin y", "<b>.go", "other.go", "``", "a\\`b"]


def fuzz_reviews(n, seed=1):
    import random
    rnd = random.Random(seed)
    text = lambda k: "".join(rnd.choice(PIECES) for _ in range(rnd.randint(0, k)))
    out = []
    for i in range(n):
        findings = [{"path": rnd.choice(PATHS), "line": rnd.choice([0, 1, 2, 3, 40]),
                     "severity": rnd.choice(["critical", "major", "minor", "nit", "odd"]),
                     "title": text(6), "body": text(14)} for _ in range(rnd.randint(0, 5))]
        r = aa.build_review({"summary": text(12), "findings": findings}, {"outcome": "verified"} if i % 2 else None,
                            DIFF, "a" * 40, "never", "https://run" if i % 3 else "")
        if r["comments"] and i % 4 == 0:
            r = aa.move_comments_to_body(r)
        out.append(r)
    return out


@unittest.skipUnless(shutil.which("node") and os.path.isdir(os.path.join(MARKDOWN, "node_modules", "commonmark")),
                     "needs node and `npm ci` in tests/markdown")
class MarkdownFuzzTest(Tmp):
    """Whole reviews, as CommonMark parses them: no HTML renders, no mention
    is live, and the closing note survives as its own paragraph."""

    def check(self, reviews):
        path = self.write("reviews.json", json.dumps(reviews))
        p = subprocess.run(["node", os.path.join(MARKDOWN, "check.mjs"), path, aa.MARKER, aa.NOTE],
                           capture_output=True, text=True)
        return p.returncode, p.stdout + p.stderr

    def test_the_reviewers_cases(self):
        cases = []
        for body in ("~~~\n<b>x\n~~~", "```\n<b>x\n```", "````\n<details>x\n````"):
            for path in ("other.go", "", " ", "\r"):
                cases.append(aa.build_review({"summary": "s", "findings": [
                    {"path": path, "line": 40, "severity": "major", "title": "t", "body": body},
                    {"path": "z.go", "line": 0, "severity": "nit", "title": "t2", "body": "`x<b>` and <b>"}]},
                    None, DIFF, "a" * 40, "never", ""))
        cases.append(aa.move_comments_to_body(aa.build_review({"summary": "s", "findings": [
            {"path": "app.go", "line": 2, "severity": "major", "title": "t", "body": "~~~\n<b>x\n~~~"}]},
            None, DIFF, "a" * 40, "never", "")))
        code, out = self.check(cases)
        self.assertEqual(code, 0, out)
        self.assertIn("(no file)", cases[1]["body"])

    def test_fuzzed_reviews(self):
        code, out = self.check(fuzz_reviews(3000))
        self.assertEqual(code, 0, out)

    def test_the_checker_catches_what_it_should(self):
        bad = [{"body": aa.MARKER + "\n\n<b>x</b>\n\n" + aa.NOTE, "comments": []},
               {"body": aa.MARKER + "\n\nhi @admin\n\n" + aa.NOTE, "comments": []},
               {"body": aa.MARKER + "\n\n```\n" + aa.NOTE, "comments": []}]
        code, out = self.check(bad)
        self.assertEqual(code, 1)
        for want in ("review 0: html_inline", "review 1: a live mention", "review 2: the closing note"):
            self.assertIn(want, out)

    def test_a_long_review_keeps_its_note(self):
        many = [{"path": "z.go", "line": 0, "severity": "minor", "title": "t", "body": "<" * 3990} for _ in range(50)]
        r = aa.build_review({"summary": "s", "findings": many}, {"outcome": "verified"}, DIFF, "a" * 40, "never", "")
        self.assertLessEqual(len(r["body"]), aa.BODY_LIMIT)
        self.assertIn("cut for length", r["body"])
        code, out = self.check([r])
        self.assertEqual(code, 0, out)


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
        self.assertIn("@\u200dsomeone", payload["comments"][0]["body"])

    def test_review_takes_the_policy_from_the_base(self):
        base, head = self.git_repo()
        g = lambda *a: subprocess.run(["git", "-c", "core.hooksPath=/dev/null", "-c", "user.name=t", "-c", "user.email=t@example.com", *a],
                                      cwd=self.ws, check=True, capture_output=True, text=True).stdout.strip()
        # Base: a policy; head (the pull request): the policy rewritten.
        g("checkout", "-q", base)
        self.write("ws/policy.json", '{"limits": {"max_turns": 7}}')
        g("add", ".")
        g("commit", "-qm", "policy")
        base = g("rev-parse", "HEAD")
        self.write("ws/policy.json", '{"mcp": {"servers": [{"name": "x", "command": "sh"}]}}')
        g("commit", "-qam", "pr")
        head = g("rev-parse", "HEAD")
        event = self.write("event.json", json.dumps({"pull_request": {"number": 5, "base": {"sha": base}, "head": {"sha": head}}}))
        fake = self.write("bin/abhed", FAKE_REVIEW_ABHED, 0o755)
        gh = FakeGitHub()
        self.addCleanup(gh.close)
        steps, uses, outputs, failed, _ = composite.run_action(
            {"mode": "review", "abhed-path": fake, "policy-file": "policy.json", "post-review": "false",
             "model-provider": "openai-compatible", "model": "m", "model-base-url": "http://127.0.0.1:9/v1"},
            workspace=self.ws, temp=self.temp, github={"event_name": "pull_request", "api_url": gh.url},
            env={"GITHUB_EVENT_PATH": event})
        self.assertFalse(failed, {k: v.get("stderr") for k, v in steps.items()})
        a = open(self.path("bin", "report.args")).read().split("\n")
        settings = json.load(open(a[a.index("-settings") + 1]))
        self.assertEqual(settings["limits"], {"max_turns": 7})
        self.assertNotIn("mcp", settings)

    def test_dispatch_review_needs_the_checkout_at_the_head(self):
        base, head = self.git_repo()
        subprocess.run(["git", "checkout", "-q", base], cwd=self.ws, check=True)
        gh = FakeGitHub()
        gh.base, gh.head = base, head
        self.addCleanup(gh.close)
        fake = self.write("bin/abhed", FAKE_REVIEW_ABHED, 0o755)
        steps, uses, outputs, failed, _ = composite.run_action(
            {"mode": "review", "abhed-path": fake, "pr-number": "5"},
            workspace=self.ws, temp=self.temp, github={"event_name": "workflow_dispatch", "api_url": gh.url})
        self.assertTrue(failed)
        self.assertIn("pull request #5 is now at", steps["review"]["stderr"])
        self.assertFalse(os.path.exists(self.path("bin", "report.args")))
        self.assertEqual(gh.posts, [])

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
        # The process tier, forced: container engines on PATH answer nothing,
        # so the session gets what a GitHub runner gets and the test cannot
        # pass by landing on a container or VM tier.
        shims = self.path("no-engines")
        os.makedirs(shims, exist_ok=True)
        for name in ("podman", "docker", "runsc", "nerdctl"):
            self.write(f"no-engines/{name}", "#!/bin/sh\nexit 1\n", 0o755)
        e = {"PATH": shims + os.pathsep + os.environ["PATH"], "HOME": self.path("home"), "RUNNER_TEMP": self.runner_temp(),
             "GITHUB_OUTPUT": self.path("out"), "ABHED_BIN": BIN, "ABHED_WORKSPACE": ws,
             "ABHED_EVIDENCE_DIR": self.path("ev"), "ABHED_SETTINGS": settings, **env}
        r = subprocess.run(["bash", os.path.join(LIB, "run.sh")], env=e, capture_output=True, text=True, timeout=300)
        self.assertEqual(r.returncode, 0, r.stderr)
        return self.outputs(self.path("out")), r

    def remove_made_temp(self):
        # The command's mktemp file, named by the command itself.
        for l in open(self.path("ev", "run.jsonl")) if os.path.exists(self.path("ev", "run.jsonl")) else []:
            for m in re.findall(r"made (/[^\\\s\"]*abhed-e2e[^\\\s\"]*)", l):
                if os.path.isfile(m):
                    os.remove(m)

    def test_run_mode_records_and_verifies(self):
        # The command uses TMPDIR too, as go build and go test do: it must
        # exist inside the sandbox (bubblewrap's private /tmp on Linux).
        cmd = 'touch made-in-ci && t="$(mktemp -t abhed-e2e.XXXXXX)" && echo tmp-ok > "$t" && cat "$t" && echo "made $t"'
        m = ScriptedModel([{"tool": "bash", "args": {"command": cmd, "description": "probe"}}, {"text": "done"}])
        self.addCleanup(m.close)
        o, r = self.run_real(m, ABHED_TASK="make a file", ABHED_PERMISSION_MODE="default",
                             ABHED_ALLOW="bash", ABHED_VERIFY="label")
        self.remove_made_temp()
        self.assertEqual(o["exit-code"], "0", r.stderr)
        self.assertTrue(o["session-id"])
        self.assertTrue(os.path.exists(self.path("ws", "made-in-ci")), open(self.path("ev", "run.jsonl")).read()[-3000:])
        obs = [json.loads(l) for l in open(self.path("ev", "run.jsonl")) if '"observation"' in l]
        self.assertTrue(any("tmp-ok" in json.dumps(o) for o in obs), obs)
        tiers = {o["payload"].get("sandbox") for o in obs}
        self.assertEqual(tiers, {"process"}, obs)
        self.assertTrue(os.path.getsize(self.path("ev", "record.jsonl")) > 0, open(self.path("ev", "export.log")).read())
        self.assertIn("<!doctype html", open(self.path("ev", "transcript.html")).read().lower())
        v = subprocess.run([BIN, "record", "verify", self.path("ev", "record.jsonl")], capture_output=True, text=True,
                           env={"PATH": os.environ["PATH"], "HOME": self.path("home2")})
        self.assertEqual(v.returncode, 0, v.stdout + v.stderr)

    @unittest.skipUnless(os.environ.get("ABHED_TEST_PROCESS_KILL") == "1",
                         "starts and ends real processes: set ABHED_TEST_PROCESS_KILL=1 on a throwaway runner")
    def test_a_detached_process_does_not_outlive_the_step(self):
        # nohup and & leave a process that would write after the run ended.
        cmd = "(nohup sh -c 'sleep 3; touch late' >/dev/null 2>&1 &) ; echo started"
        m = ScriptedModel([{"tool": "bash", "args": {"command": cmd, "description": "probe"}}, {"text": "done"}])
        self.addCleanup(m.close)
        o, r = self.run_real(m, ABHED_TASK="start it", ABHED_PERMISSION_MODE="default", ABHED_ALLOW="bash", ABHED_VERIFY="label")
        self.assertEqual(o["exit-code"], "0", r.stderr)
        time.sleep(5)
        self.assertFalse(os.path.exists(self.path("ws", "late")), "a process the run started wrote after the step")

    def test_run_mode_refuses_what_is_not_allowed(self):
        m = ScriptedModel([{"tool": "bash", "args": {"command": "touch not-allowed", "description": "probe"}}, {"text": "done"}])
        self.addCleanup(m.close)
        o, r = self.run_real(m, ABHED_TASK="make a file", ABHED_PERMISSION_MODE="default", ABHED_VERIFY="label")
        self.assertFalse(os.path.exists(self.path("ws", "not-allowed")))
        res = json.load(open(self.path("ev", "summary.json")))["result"]
        # permission_denials is in the result line from 1.3.0; the event is in every version.
        self.assertIn('"action.denied"', open(self.path("ev", "run.jsonl")).read())

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
                             ABHED_JSON_SCHEMA=json.dumps(aa.REVIEW_SCHEMA), ABHED_TOOLS_FALLBACK_DENY=FALLBACK_DENY)
        self.assertEqual(o["exit-code"], "0", open(self.path("ev", "stderr.log")).read())
        self.assertFalse(os.path.exists(self.path("ws", "should-not-run")))
        self.assertEqual(aa.main(["review-post", "--result", self.path("ev", "run.jsonl"), "--diff", diff,
                                  "--commit", "c" * 40, "--dry-run", self.path("payload.json")]), 0)
        p = json.load(open(self.path("payload.json")))
        self.assertEqual(p["event"], "COMMENT")
        self.assertEqual([(c["path"], c["line"]) for c in p["comments"]], [("app.go", 2)])
        self.assertIn("@\u200doctocat", p["comments"][0]["body"])


if __name__ == "__main__":
    unittest.main()
