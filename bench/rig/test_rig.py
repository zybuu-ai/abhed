#!/usr/bin/env python3
"""Checks on the parts of the rig that would misreport quietly if wrong."""
import json
import shutil
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import rig  # noqa: E402


class Status(unittest.TestCase):
    def parse(self, text):
        out = {}
        for line in text.splitlines():
            m = rig.STATUS.match(line.strip())
            if m:
                out[m.group(2).strip()] = m.group(1)
        return out

    def test_reads_pytest_short_summary(self):
        got = self.parse("""
PASSED testing/test_a.py::test_one
FAILED testing/test_a.py::test_two - AssertionError: nope
ERROR testing/test_b.py::test_three - ImportError
PASSED testing/test_c.py::test_param[a b-1]
some other line
""")
        self.assertEqual(got["testing/test_a.py::test_one"], "PASSED")
        self.assertEqual(got["testing/test_a.py::test_two"], "FAILED")
        self.assertEqual(got["testing/test_b.py::test_three"], "ERROR")
        # A parametrised id may contain spaces; it must survive whole.
        self.assertEqual(got["testing/test_c.py::test_param[a b-1]"], "PASSED")
        self.assertEqual(len(got), 4)


class Bootstrap(unittest.TestCase):
    def test_identical_harnesses_span_zero(self):
        a = {f"t{i}": (i % 3) / 2 for i in range(30)}
        lo, hi = rig.paired_bootstrap(a, dict(a))
        self.assertEqual((lo, hi), (0.0, 0.0))

    def test_a_consistent_gap_excludes_zero(self):
        a = {f"t{i}": 1.0 for i in range(30)}
        b = {f"t{i}": 0.0 if i % 2 else 1.0 for i in range(30)}
        lo, hi = rig.paired_bootstrap(a, b)
        self.assertGreater(lo, 0)
        self.assertLess(hi, 1)

    def test_two_tasks_out_of_24_is_not_a_difference(self):
        # The earlier suite's headline: 24/24 against 22/24. Paired, over
        # tasks, that interval has to reach zero — which is the whole reason
        # this rig runs more than once and reports intervals.
        a = {f"t{i}": 1.0 for i in range(24)}
        b = {f"t{i}": 0.0 if i < 2 else 1.0 for i in range(24)}
        lo, _ = rig.paired_bootstrap(a, b)
        self.assertEqual(lo, 0.0)

    def test_only_shared_tasks_count(self):
        self.assertIsNone(rig.paired_bootstrap({"x": 1.0}, {"y": 1.0}))

    def test_same_seed_same_interval(self):
        a = {f"t{i}": (i * 7 % 5) / 4 for i in range(40)}
        b = {f"t{i}": (i * 3 % 5) / 4 for i in range(40)}
        self.assertEqual(rig.paired_bootstrap(a, b), rig.paired_bootstrap(a, b))


class Conditions(unittest.TestCase):
    def test_tight_window_respects_openhands_documented_minimum(self):
        self.assertGreaterEqual(rig.CONDITIONS["tight"], 22000)
        self.assertLess(rig.CONDITIONS["tight"], rig.CONDITIONS["full"])

    def test_requests_stays_out_of_the_pool(self):
        self.assertNotIn("psf/requests", rig.POOL)


class Gate(unittest.TestCase):
    def test_target_tests_must_run_and_fail_at_base(self):
        f2p = ["t.py::a", "t.py::b"]
        self.assertTrue(rig.fair_at_base({"t.py::a": "FAILED", "t.py::b": "FAILED"}, f2p))
        # Not collected at all: the test file could not be imported. This is
        # pylint-4604, whose tests import a constant only the reference patch
        # adds, and which took half of the first pilot.
        self.assertFalse(rig.fair_at_base({}, f2p))
        self.assertFalse(rig.fair_at_base({"t.py::a": "FAILED"}, f2p))
        self.assertFalse(rig.fair_at_base({"t.py::a": "ERROR", "t.py::b": "FAILED"}, f2p))
        # Already passing is not a task either.
        self.assertFalse(rig.fair_at_base({"t.py::a": "PASSED", "t.py::b": "FAILED"}, f2p))
        self.assertFalse(rig.fair_at_base({}, []))


class Watch(unittest.TestCase):
    def test_snapshot_counts_progress_and_passes(self):
        import json
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            old, rig.RESULTS = rig.RESULTS, Path(tmp)
            try:
                root = Path(tmp) / "d1" / "rig"
                plan = [{"run": 1, "instance": f"t{i}", "condition": "full", "harness": h}
                        for i in range(2) for h in ("abhed", "pi")]
                root.mkdir(parents=True)
                (root / "plan.json").write_text(json.dumps({"sessions": plan}))
                for h, iid, ok, to in (("abhed", "t0", True, False), ("pi", "t0", False, True)):
                    p = root / h / "full" / "run1" / f"{iid}.json"
                    p.parent.mkdir(parents=True)
                    p.write_text(json.dumps({"harness": h, "condition": "full", "instance": iid, "wall_sec": 90,
                                             "timed_out": to, "finished": "2026-01-01 00:00:0" + ("1" if ok else "2"),
                                             "score": {"resolved": ok, "f2p": "1/1"}}))
                text = rig.snapshot("d1")
            finally:
                rig.RESULTS = old
        self.assertIn("2 of 4 finished", text)
        self.assertIn("1 timed out", text)
        self.assertIn("not running", text)
        # One row per harness, with its own count — not a pooled number.
        self.assertRegex(text, r"abhed\s+full\s+1 / 1")
        self.assertRegex(text, r"pi\s+full\s+0 / 1")
        self.assertIn("not a result", text)




class RecordAndDifficultyTests(unittest.TestCase):
    def test_abhed_usage_comes_from_the_record(self):
        lines = [json.dumps({"seq": 1, "type": "user.message", "payload": {"text": "x"}}),
                 "not json at all",
                 json.dumps({"seq": 2, "type": "session.ended",
                             "payload": {"reason": "completed", "turns": 4, "tokens_in": 1200, "tokens_out": 80}})]
        h = rig.Abhed()
        self.assertEqual(h.usage("\n".join(lines)),
                         {"turns": 4, "tokens_in": 1200, "tokens_out": 80, "reason": "completed"})
        self.assertEqual([e["seq"] for e in h.record("\n".join(lines))], [1, 2])
        # The old text format still parses, for a record that was cut short.
        self.assertEqual(h.usage("… 3 turns · 9,000 in / 200 out tokens")["tokens_in"], 9000)

    def test_difficulty_filter_uses_the_datasets_rating(self):
        real = rig.instances
        rig.instances = lambda repo=None: [
            {"instance_id": "a", "difficulty": "<15 min fix"},
            {"instance_id": "b", "difficulty": "1-4 hours"},
            {"instance_id": "c", "difficulty": "<15 min fix"}]
        try:
            self.assertEqual(rig.by_difficulty(["a", "b", "c"], "easy"), ["a", "c"])
            self.assertEqual(rig.by_difficulty(["a", "b", "c"], "1-4 hours"), ["b"])
            self.assertEqual(rig.by_difficulty(["a", "b"], ">4 hours"), [])
        finally:
            rig.instances = real


class ResultFilesTests(unittest.TestCase):
    def test_sidecars_are_not_results(self):
        import tempfile
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            run = root / "abhed" / "full" / "run1"
            run.mkdir(parents=True)
            for name in ("x.json", "x.events.json", "x.hawkeye.json", "y.slept.json"):
                (run / name).write_text("{}")
            self.assertEqual([p.name for p in rig.result_files(root)], ["x.json"])


class RemoteEndpointTests(unittest.TestCase):
    def test_remote_model_is_named_and_keyed_from_the_environment(self):
        import os
        saved = {k: os.environ.get(k) for k in ("ABHED_BENCH_MODEL", "ABHED_BENCH_API_KEY", "ABHED_BENCH_ENDPOINT")}
        try:
            for k in saved:
                os.environ.pop(k, None)
            self.assertEqual(rig.model_name("full"), "abhed-bench-full")
            self.assertEqual(rig.api_key(), "bench")
            self.assertFalse(rig.remote())
            os.environ["ABHED_BENCH_MODEL"] = "openai/gpt-oss-120b"
            os.environ["ABHED_BENCH_API_KEY"] = "sk-test"
            os.environ["ABHED_BENCH_ENDPOINT"] = "https://openrouter.ai/api/v1"
            self.assertEqual(rig.model_name("tight"), "openai/gpt-oss-120b")
            self.assertEqual(rig.api_key(), "sk-test")
            self.assertTrue(rig.remote())
            self.assertEqual(rig.base_model(), "openai/gpt-oss-120b @ openrouter.ai")
        finally:
            for k, v in saved.items():
                if v is None:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v


class WindowTests(unittest.TestCase):
    def test_every_harness_is_told_one_window(self):
        import os
        saved = {k: os.environ.get(k) for k in ("ABHED_BENCH_MODEL", "ABHED_BENCH_CONTEXT")}
        try:
            for k in saved:
                os.environ.pop(k, None)
            self.assertEqual(rig.window("tight"), rig.CONDITIONS["tight"])
            os.environ["ABHED_BENCH_MODEL"] = "openai/gpt-oss-120b"
            self.assertEqual(rig.window("full"), 131072)
            os.environ["ABHED_BENCH_CONTEXT"] = "65536"
            self.assertEqual(rig.window("full"), 65536)
        finally:
            for k, v in saved.items():
                if v is None:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v


def fake_instance(root, iid="i1"):
    """A prepared instance on disk: a tiny repo, a venv stub and instance.json."""
    import sys as _sys
    env = root / "envs" / iid
    (env / "repo").mkdir(parents=True)
    (env / "repo" / "src.py").write_text("x = 1\n")
    (env / "repo" / "test_x.py").write_text("def test_x(): pass\n")
    (env / "venv" / "bin").mkdir(parents=True)
    (env / "venv" / "bin" / "python").symlink_to(_sys.executable)
    gold = ("diff --git a/test_x.py b/test_x.py\n--- a/test_x.py\n+++ b/test_x.py\n"
            "@@ -1 +1 @@\n-def test_x(): pass\n+def test_x(): assert True\n")
    inst = {"instance_id": iid, "repo": "pytest-dev/pytest", "test_patch": gold,
            "FAIL_TO_PASS": '["test_x.py::test_x"]', "PASS_TO_PASS": "[]", "problem_statement": "p"}
    (env / "instance.json").write_text(json.dumps(inst))
    return inst


class UsingCache:
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)
        self._saved = rig.CACHE
        rig.CACHE = self.root
        (self.root / "suite.json").write_text("{}")

    def tearDown(self):
        rig.CACHE = self._saved
        self._tmp.cleanup()


class AgentChangeTests(UsingCache, unittest.TestCase):
    def agent(self, ws, *cmd):
        import subprocess
        subprocess.run(["git", "-c", "user.name=a", "-c", "user.email=a@a", *cmd], cwd=ws, check=True,
                       capture_output=True)

    def test_a_commit_or_a_new_history_by_the_agent_does_not_hide_its_change(self):
        fake_instance(self.root)
        ws = self.root / "scratch" / "abhed-full-i1-run1"
        rig.workspace("i1", ws)
        (ws / "src.py").write_text("x = 2\n")
        self.agent(ws, "add", "-A")
        self.agent(ws, "commit", "-q", "-m", "agent")
        shutil.rmtree(ws / ".git")  # and then starts over
        self.agent(ws, "init", "-q")
        patch = rig.agent_patch(ws)
        self.assertIn("+x = 2", patch)

    def test_scoring_uses_a_fresh_tree_with_the_gold_tests(self):
        inst = fake_instance(self.root)
        ws = self.root / "scratch" / "abhed-full-i1-run1"
        rig.workspace("i1", ws)
        (ws / "src.py").write_text("x = 2\n")
        (ws / "test_x.py").write_text("def test_x(): assert False\n")  # the agent edits a test
        (ws / "stray.txt").write_text("left behind")
        seen = {}
        real_sh = rig.sh

        def sh(cmd, cwd=None, env=None, timeout=None, check=False):
            if cmd[:3] == ["python", "-m", "pytest"]:
                seen["src"] = (Path(cwd) / "src.py").read_text()
                seen["test"] = (Path(cwd) / "test_x.py").read_text()
                seen["tmp"] = env["TMPDIR"]
                return 0, "PASSED test_x.py::test_x\n"
            return real_sh(cmd, cwd=cwd, env=env, timeout=timeout, check=check)

        rig.sh = sh
        try:
            result = rig.score_patch("i1", inst, rig.agent_patch(ws), "t")
        finally:
            rig.sh = real_sh
        self.assertTrue(result["resolved"], result)
        self.assertEqual(seen["src"], "x = 2\n")
        self.assertEqual(seen["test"], "def test_x(): assert True\n")
        self.assertIn("rig-score-", seen["tmp"])


class SessionIsolationTests(UsingCache, unittest.TestCase):
    def test_the_venv_copy_points_only_at_itself(self):
        import os
        src = self.root / "envs" / "i1" / "venv"
        (src / "bin").mkdir(parents=True)
        (src / "bin" / "pytest").write_text(f"#!{src}/bin/python\nprint('hi')\n")
        (src / "bin" / "python").symlink_to("/usr/bin/python3")
        (src / "lib").mkdir()
        (src / "lib" / "tool").write_text("x")
        (src / "bin" / "tool").symlink_to(src / "lib" / "tool")
        copy = rig.session_venv("i1", self.root / "venv-a")
        self.assertEqual((copy / "bin" / "pytest").read_text().splitlines()[0], f"#!{copy}/bin/python")
        self.assertTrue((copy / "bin" / "python").is_symlink())
        self.assertTrue(Path(os.path.realpath(copy / "bin" / "tool")).is_relative_to(copy.resolve()))
        env = rig.task_env("i1", self.root / "ws", copy, tmp=self.root / "tmp-a")
        self.assertTrue(env["PATH"].startswith(str(copy / "bin")))
        self.assertEqual(env["TMPDIR"], str(self.root / "tmp-a"))

    def test_scratch_names_keep_runs_and_invocations_apart(self):
        a = rig.scratch_tag("abhed", "full", "i1", 1, Path("/r/2026-09-23/x.json"))
        b = rig.scratch_tag("abhed", "full", "i1", 2, Path("/r/2026-09-23/x.json"))
        c = rig.scratch_tag("abhed", "full", "i1", 1, Path("/r/2026-09-24/x.json"))
        self.assertEqual(len({a, b, c}), 3)


class RunBookkeepingTests(unittest.TestCase):
    def test_a_slept_session_is_set_aside_and_redone(self):
        out = Path("/r/abhed/full/run1/i1.json")
        self.assertEqual(rig.result_path(out, 5), out)
        self.assertEqual(rig.result_path(out, rig.SLEPT_LIMIT + 1).name, "i1.slept.json")
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            run1 = root / "abhed" / "full" / "run1"
            run1.mkdir(parents=True)
            (run1 / "i1.slept.json").write_text("{}")
            (run1 / "i2.json").write_text("{}")
            plan = [(1, "i1", "full", "abhed"), (1, "i2", "full", "abhed")]
            self.assertEqual([t[2] for t in rig.pending(plan, root)], ["i1"])

    def test_a_timed_out_session_keeps_its_output(self):
        import subprocess
        self.assertEqual(rig.timeout_output(subprocess.TimeoutExpired("x", 1, output=b"half\xff")), "half\ufffd")
        self.assertEqual(rig.timeout_output(subprocess.TimeoutExpired("x", 1, output="text")), "text")
        self.assertEqual(rig.timeout_output(subprocess.TimeoutExpired("x", 1)), "")

    def test_a_timeout_stops_the_whole_process_group(self):
        import subprocess
        import time as _t
        start = _t.monotonic()
        with self.assertRaises(subprocess.TimeoutExpired):
            rig.sh(["sh", "-c", "sleep 30 & sleep 30"], timeout=1)
        self.assertLess(_t.monotonic() - start, 10)

    def test_the_endpoint_key_is_redacted(self):
        import os
        saved = os.environ.get("ABHED_BENCH_API_KEY")
        os.environ["ABHED_BENCH_API_KEY"] = "sk-secret-123456"
        try:
            self.assertEqual(rig.redact("key=sk-secret-123456"), "key=[redacted]")
        finally:
            if saved is None:
                os.environ.pop("ABHED_BENCH_API_KEY", None)
            else:
                os.environ["ABHED_BENCH_API_KEY"] = saved


class EnvironmentSpecTests(UsingCache, unittest.TestCase):
    def test_an_environment_built_from_another_spec_is_not_current(self):
        d = self.root / "envs" / "i1"
        d.mkdir(parents=True)
        (d / "ready").write_text("")
        self.assertFalse(rig.env_current("i1", "pytest-dev/pytest"))
        (d / "ready").write_text(rig.env_spec("pytest-dev/pytest"))
        self.assertTrue(rig.env_current("i1", "pytest-dev/pytest"))


class ProxyLimitTests(unittest.TestCase):
    def test_every_request_is_held_to_the_output_limit(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location("shim", Path(__file__).parent / "hosted" / "shim.py")
        shim = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(shim)
        self.assertEqual(shim.MAX_OUTPUT, rig.MAX_OUTPUT)
        self.assertEqual(shim.normalise({}, 100)["max_tokens"], 100)
        self.assertEqual(shim.normalise({"max_tokens": 5000}, 100)["max_tokens"], 100)
        d = shim.normalise({"max_completion_tokens": 5000}, 100)
        self.assertEqual(d["max_completion_tokens"], 100)
        self.assertNotIn("max_tokens", d)
        self.assertEqual(shim.normalise({"max_tokens": 50}, 100)["max_tokens"], 50)
        msgs = shim.normalise({"messages": [{"content": []}, {"content": None},
                                            {"content": [{"type": "text", "text": "a"}]}]}, 100)["messages"]
        self.assertEqual([m["content"] for m in msgs], ["", "", "a"])


if __name__ == "__main__":
    unittest.main()
