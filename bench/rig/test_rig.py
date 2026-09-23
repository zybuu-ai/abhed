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


class SessionIsolationTests(unittest.TestCase):
    def test_each_session_gets_its_own_environment_and_temp_dir(self):
        import os
        with tempfile.TemporaryDirectory() as tmp:
            src = Path(tmp) / "envs" / "i1" / "venv"
            (src / "bin").mkdir(parents=True)
            (src / "bin" / "pytest").write_text(f"#!{src}/bin/python\nprint('hi')\n")
            (src / "bin" / "python").symlink_to("/usr/bin/python3")
            saved = rig.CACHE
            rig.CACHE = Path(tmp)
            try:
                copy = rig.session_venv("i1", Path(tmp) / "venv-a")
                self.assertEqual((copy / "bin" / "pytest").read_text().splitlines()[0], f"#!{copy}/bin/python")
                self.assertTrue((copy / "bin" / "python").is_symlink())
                a = rig.task_env("i1", Path(tmp) / "abhed-full-i1-run1", copy)
                b = rig.task_env("i1", Path(tmp) / "abhed-full-i1-run2")
                self.assertTrue(a["PATH"].startswith(str(copy / "bin")))
                self.assertTrue(b["PATH"].startswith(str(src / "bin")))
                self.assertNotEqual(a["TMPDIR"], b["TMPDIR"])
                self.assertTrue(os.path.isdir(a["TMPDIR"]))
            finally:
                rig.CACHE = saved
                for e in (a, b):
                    shutil.rmtree(e["TMPDIR"], ignore_errors=True)


class AgentCommitTests(unittest.TestCase):
    def test_a_commit_by_the_agent_neither_hides_the_patch_nor_keeps_its_test_edit(self):
        import subprocess
        with tempfile.TemporaryDirectory() as d:
            ws = Path(d)
            def git(*a):
                return subprocess.run(["git", "-c", "user.name=t", "-c", "user.email=t@t", *a], cwd=ws, check=True,
                                      capture_output=True, text=True).stdout
            git("init", "-q")
            (ws / "src.py").write_text("x = 1\n")
            (ws / "test_x.py").write_text("def test(): pass\n")
            git("add", "-A"); git("commit", "-q", "-m", "base")
            base = git("rev-parse", "HEAD").strip()
            (ws / "src.py").write_text("x = 2\n")
            (ws / "test_x.py").write_text("def test(): assert False\n")
            git("add", "-A"); git("commit", "-q", "-m", "agent")
            _, diff = rig.sh(["git", "diff", "--cached", base, "--", "."], cwd=ws)
            self.assertIn("x = 2", diff)
            rig.sh(["git", "checkout", "--quiet", base, "--", "test_x.py"], cwd=ws)
            self.assertEqual((ws / "test_x.py").read_text(), "def test(): pass\n")


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


if __name__ == "__main__":
    unittest.main()
