#!/usr/bin/env python3
"""Checks on the parts of the rig that would misreport quietly if wrong."""
import json
import os
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
                seen["cwd"] = Path(cwd)
                seen["stray"] = (Path(cwd) / "stray.txt").exists()
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
        self.assertNotEqual(seen["cwd"], ws)
        self.assertTrue(seen["stray"], "stray.txt is part of the diff, so it is in the fresh tree")

    def test_harness_state_stays_out_of_the_patch(self):
        fake_instance(self.root)
        ws = self.root / "scratch" / "abhed-full-i1-run1"
        rig.workspace("i1", ws)
        (ws / ".abhed").mkdir()
        (ws / ".abhed" / "config.json").write_text('{"api_key": "k"}')
        (ws / "src.py").write_text("x = 3\n")
        patch = rig.agent_patch(ws)
        self.assertIn("+x = 3", patch)
        self.assertNotIn(".abhed", patch)

    def test_a_file_the_gold_tests_create_is_replaced_not_merged(self):
        inst = fake_instance(self.root)
        inst["test_patch"] = ("diff --git a/test_new.py b/test_new.py\nnew file mode 100644\n--- /dev/null\n"
                              "+++ b/test_new.py\n@@ -0,0 +1 @@\n+def test_x(): pass\n")
        ws = self.root / "scratch" / "abhed-full-i1-run1"
        rig.workspace("i1", ws)
        (ws / "test_new.py").write_text("def test_x(): assert False\n")  # the agent wrote it first
        real_sh = rig.sh
        seen = {}

        def sh(cmd, cwd=None, env=None, timeout=None, check=False):
            if cmd[:3] == ["python", "-m", "pytest"]:
                seen["test"] = (Path(cwd) / "test_new.py").read_text()
                return 0, "PASSED test_x.py::test_x\n"
            return real_sh(cmd, cwd=cwd, env=env, timeout=timeout, check=check)

        rig.sh = sh
        try:
            result = rig.score_patch("i1", inst, rig.agent_patch(ws), "t")
        finally:
            rig.sh = real_sh
        self.assertTrue(result["resolved"], result)
        self.assertEqual(seen["test"], "def test_x(): pass\n")


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

    def test_the_agent_cannot_read_its_task_id_from_its_path(self):
        tag = rig.scratch_tag("abhed", "full", "pytest-dev__pytest-5809", 1, Path("/r/x.json"))
        self.assertNotIn("pytest", tag)
        self.assertNotIn("abhed", tag)


class RunBookkeepingTests(unittest.TestCase):
    def test_a_slept_session_is_set_aside_and_redone(self):
        out = Path("/r/abhed/full/run1/i1.json")
        self.assertEqual(rig.result_path(out, 5), out)
        self.assertEqual(rig.result_path(out, rig.SLEPT_LIMIT + 1).name, "i1.slept.json")
        self.assertEqual(rig.result_path(out, 0, readable=False).name, "i1.unread.json")
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            run1 = root / "abhed" / "full" / "run1"
            run1.mkdir(parents=True)
            (run1 / "i1.slept.json").write_text("{}")
            (run1 / "i2.json").write_text("{}")
            (run1 / "i3.unread.json").write_text("{}")
            self.assertEqual([p.name for p in rig.result_files(root)], ["i2.json"])
            plan = [(1, "i1", "full", "abhed"), (1, "i2", "full", "abhed"), (1, "i3", "full", "abhed")]
            self.assertEqual([t[2] for t in rig.pending(plan, root)], ["i1", "i3"])

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


class HarnessParityTests(UsingCache, unittest.TestCase):
    def test_abhed_is_told_the_window_and_the_output_limit(self):
        cfg = rig.Abhed.configure({"permissions": {}}, "full")
        bench = cfg["model"]["providers"]["bench"]
        self.assertEqual(cfg["limits"]["max_tokens"], rig.MAX_OUTPUT)
        self.assertEqual(bench["context_window"], rig.window("full"))
        self.assertIn("permissions", cfg)

    def test_every_harness_runs_with_the_session_home(self):
        seen = []
        real_sh = rig.sh

        def sh(cmd, cwd=None, env=None, timeout=None, check=False):
            seen.append((cmd[0], (env or {}).get("HOME")))
            if cmd[-1] == "init":
                (Path(cwd) / ".abhed").mkdir(exist_ok=True)
                (Path(cwd) / ".abhed" / "config.json").write_text("{}")
            return 0, ""

        rig.sh = sh
        try:
            for h in (rig.Abhed(), rig.Pi(), rig.OpenHands()):
                ws, home = self.root / f"ws-{h.name}", self.root / f"home-{h.name}"
                ws.mkdir()
                home.mkdir()
                seen.clear()
                h.run(ws, "p", "full", {"PATH": "/usr/bin", "HOME": "/Users/operator"}, home)
                self.assertTrue(seen and all(hm == str(home) for _, hm in seen), (h.name, seen))
        finally:
            rig.sh = real_sh

    def test_the_record_beside_a_result_is_redacted(self):
        import os
        saved = {k: os.environ.get(k) for k in ("ABHED_BENCH_API_KEY", "ABHED_BIN")}
        os.environ["ABHED_BENCH_API_KEY"] = "sk-secret-123456"
        os.environ["ABHED_BIN"] = str(self.root / "no-such-binary")
        try:
            out = self.root / "r" / "i1.json"
            out.parent.mkdir()
            rig.hawkeye([{"type": "observation", "seq": 1, "payload": {"content": "api_key sk-secret-123456"}}], out)
            self.assertNotIn("sk-secret-123456", (out.parent / "i1.events.json").read_text())
        finally:
            for k, v in saved.items():
                if v is None:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v

    def test_local_variants_carry_the_window_and_the_output_limit(self):
        import argparse
        import contextlib
        import io
        with contextlib.redirect_stdout(io.StringIO()):
            rig.models(argparse.Namespace(base="some-model"))
        text = (self.root / "modelfiles" / "full.Modelfile").read_text()
        self.assertIn(f"PARAMETER num_ctx {rig.CONDITIONS['full']}", text)
        self.assertIn(f"PARAMETER num_predict {rig.MAX_OUTPUT}", text)

    def test_the_environment_sets_no_temp_dir_unless_given_one(self):
        self.assertNotIn("TMPDIR", rig.task_env("i1", self.root / "ws"))
        self.assertEqual(rig.task_env("i1", self.root / "ws", tmp=self.root / "t")["TMPDIR"], str(self.root / "t"))


class RigBoundaryTests(UsingCache, unittest.TestCase):
    def test_an_interrupted_rig_takes_the_harness_with_it(self):
        import os
        import signal as sig
        import subprocess
        import time as _t
        pidfile = self.root / "child.pid"
        code = (f"import sys; sys.path.insert(0, {str(Path(__file__).parent)!r}); import rig; "
                f"rig.sh(['sh', '-c', 'echo $$ > {pidfile}; exec sleep 60'])")
        proc = subprocess.Popen([sys.executable, "-c", code], stderr=subprocess.DEVNULL)
        for _ in range(100):
            if pidfile.exists() and pidfile.read_text().strip():
                break
            _t.sleep(0.1)
        child = int(pidfile.read_text())
        proc.send_signal(sig.SIGINT)
        proc.wait(timeout=20)
        _t.sleep(0.3)
        with self.assertRaises(ProcessLookupError):
            os.kill(child, 0)

    def test_harnesses_never_see_the_operators_credentials_or_overrides(self):
        import os
        planted = {"ABHED_BENCH_WATSONX_APIKEY": "wx-very-secret-key", "ABHED_MODEL": "other-model",
                   "ABHED_BASE_URL": "http://elsewhere", "OPENAI_API_KEY": "sk-operator-key-1"}
        saved = {k: os.environ.get(k) for k in planted}
        os.environ.update(planted)
        try:
            env = rig.task_env("i1", self.root / "ws")
            for k in planted:
                self.assertNotIn(k, env)
            self.assertIn("PATH", env)
            text = rig.redact("key wx-very-secret-key and sk-operator-key-1")
            self.assertNotIn("wx-very-secret-key", text)
            self.assertNotIn("sk-operator-key-1", text)
        finally:
            for k, v in saved.items():
                if v is None:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v

    def test_a_change_the_rig_cannot_read_is_set_aside_not_scored(self):
        fake_instance(self.root)

        class Quiet(rig.Harness):
            name = "quiet"

            def run(self, ws, prompt, cond, env, home):
                return 0, "done"

            def version(self):
                return "0"

        real = (rig.agent_patch, rig.HARNESSES.get("quiet"))
        rig.HARNESSES["quiet"] = Quiet

        def broken(ws):
            raise rig.NoPatch("index.lock exists")

        rig.agent_patch = broken
        try:
            out = self.root / "results" / "quiet" / "full" / "run1" / "i1.json"
            rig.one_run("quiet", "i1", "full", out)
        finally:
            rig.agent_patch = real[0]
            rig.HARNESSES.pop("quiet")
        self.assertFalse(out.exists())
        self.assertTrue(out.with_suffix(".unread.json").exists())
        self.assertEqual(list((self.root / "scratch").glob("quiet-*")), [])

    def test_the_rigs_git_ignores_the_operators_git_config(self):
        g = rig.git(self.root)
        self.assertIn("GIT_CONFIG_GLOBAL=/dev/null", g)
        self.assertIn("GIT_CONFIG_NOSYSTEM=1", g)
        self.assertIn("core.hooksPath=/dev/null", g)

    def test_the_summary_counts_sessions_set_aside(self):
        saved = rig.RESULTS
        rig.RESULTS = self.root / "results"
        try:
            root = rig.RESULTS / "t" / "rig"
            run1 = root / "abhed" / "full" / "run1"
            run1.mkdir(parents=True)
            (root / "plan.json").write_text(json.dumps({"sessions": [
                {"run": 1, "instance": i, "condition": "full", "harness": "abhed"} for i in ("a", "b", "c")]}))
            (run1 / "a.json").write_text("{}")
            (run1 / "b.slept.json").write_text("{}")
            self.assertIn("| abhed | 3 | 1 | 1 | 1 |", rig.coverage("t"))
        finally:
            rig.RESULTS = saved


def _pids_alive(pids):
    import os
    alive = []
    for pid in pids:
        try:
            os.kill(pid, 0)
            alive.append(pid)
        except ProcessLookupError:
            pass
    return alive


class StopSignalTests(UsingCache, unittest.TestCase):
    def start(self, body, pids):
        import subprocess
        code = (f"import sys; sys.path.insert(0, {str(Path(__file__).parent)!r}); import rig; "
                f"rig.install_stop_handlers(); {body}")
        proc = subprocess.Popen([sys.executable, "-c", code], stderr=subprocess.DEVNULL)
        import time as _t
        for _ in range(150):
            if all(p.exists() and p.read_text().strip() for p in pids):
                break
            _t.sleep(0.1)
        return proc, [int(p.read_text()) for p in pids]

    def check(self, sig_):
        import signal as s_
        import time as _t
        pids = [self.root / f"c{i}.pid" for i in range(2)]
        body = ("rig.execute([0, 1], 2, lambda i: rig.sh(['sh', '-c', "
                f"'echo $$ > {self.root}/c' + str(i) + '.pid; exec sleep 60']))")
        proc, children = self.start(body, pids)
        start = _t.monotonic()
        proc.send_signal(sig_)
        proc.wait(timeout=20)
        self.assertLess(_t.monotonic() - start, 10, "the rig waited for its harnesses instead of stopping them")
        _t.sleep(0.3)
        self.assertEqual(_pids_alive(children), [])

    def test_sigterm_stops_every_harness_in_a_parallel_run(self):
        import signal as s_
        self.check(s_.SIGTERM)

    def test_ctrl_c_stops_every_harness_in_a_parallel_run(self):
        import signal as s_
        self.check(s_.SIGINT)

    def test_a_closed_terminal_stops_every_harness(self):
        import signal as s_
        self.check(s_.SIGHUP)

    def test_sigterm_stops_a_serial_run(self):
        import signal as s_
        import time as _t
        pids = [self.root / "c0.pid"]
        body = f"rig.execute([0], 1, lambda i: rig.sh(['sh', '-c', 'echo $$ > {self.root}/c0.pid; exec sleep 60']))"
        proc, children = self.start(body, pids)
        proc.send_signal(s_.SIGTERM)
        self.assertEqual(proc.wait(timeout=10), 128 + s_.SIGTERM)
        _t.sleep(0.3)
        self.assertEqual(_pids_alive(children), [])

    def injected(self, prelude, workers, delay):
        """Start a run whose Popen or registration is instrumented, signal it
        after `delay`, and return (exit code, seconds taken, children alive)."""
        import os
        import signal as s_
        import subprocess
        import time as _t
        pidfile = self.root / "c0.pid"
        code = (f"import os, signal, subprocess, sys, time; sys.path.insert(0, {str(Path(__file__).parent)!r}); "
                f"import rig; rig.install_stop_handlers(); {prelude}; "
                f"rig.execute([0], {workers}, lambda i: rig.sh(['sh', '-c', 'echo $$ > {pidfile}; exec sleep 60']))")
        proc = subprocess.Popen([sys.executable, "-c", code], stderr=subprocess.DEVNULL)
        start = _t.monotonic()
        if delay is not None:
            _t.sleep(delay)
            proc.send_signal(s_.SIGTERM)
        try:
            rc = proc.wait(timeout=30)
        except subprocess.TimeoutExpired:  # a hung rig is the failure; do not leave it behind
            proc.kill()
            rc = proc.wait()
        took = _t.monotonic() - start
        _t.sleep(0.5)
        child = int(pidfile.read_text()) if pidfile.exists() and pidfile.read_text().strip() else None
        alive = _pids_alive([child]) if child else []
        for pid in alive:
            os.kill(pid, s_.SIGKILL)
        return rc, took, alive

    def test_a_signal_while_the_child_is_being_registered_neither_deadlocks_nor_orphans(self):
        prelude = ("exec(" + repr("class S(dict):\n    def __setitem__(self, k, v):\n        os.kill(os.getpid(), signal.SIGTERM)\n"
                                  "        super().__setitem__(k, v)\nrig._LIVE = S()") + ")")
        rc, took, alive = self.injected(prelude, 1, None)
        self.assertEqual((rc, alive), (143, []))
        self.assertLess(took, 10)

    def test_a_signal_as_popen_returns_does_not_orphan_the_child(self):
        prelude = ("exec(" + repr("class P(subprocess.Popen):\n    fired = False\n    def __init__(self, *a, **k):\n"
                                  "        super().__init__(*a, **k)\n        if not P.fired:\n"
                                  "            P.fired = True\n            time.sleep(0.3)\n"
                                  "            os.kill(os.getpid(), signal.SIGTERM)\nrig.subprocess.Popen = P") + ")")
        rc, took, alive = self.injected(prelude, 1, None)
        self.assertEqual((rc, alive), (143, []))

    def test_a_worker_starting_as_the_rig_stops_does_not_keep_it_waiting(self):
        prelude = ("exec(" + repr("class P(subprocess.Popen):\n    def __init__(self, *a, **k):\n"
                                  "        if a and a[0][:1] == ['sh']:\n            time.sleep(1.0)\n"
                                  "        super().__init__(*a, **k)\nrig.subprocess.Popen = P") + ")")
        rc, took, alive = self.injected(prelude, 2, 0.5)
        self.assertEqual((rc, alive), (143, []))
        self.assertLess(took, 5)

    def test_nothing_starts_once_the_rig_is_stopping(self):
        rig._Stop.signum = 15
        try:
            with self.assertRaises(rig.Stopped):
                rig.sh(["true"])
        finally:
            rig._Stop.signum = 0


class RoundFourTests(UsingCache, unittest.TestCase):
    def test_a_secret_is_redacted_in_every_field_and_in_json_escaped_form(self):
        import os
        saved = os.environ.get("ABHED_BENCH_WATSONX_APIKEY")
        os.environ["ABHED_BENCH_WATSONX_APIKEY"] = 'k"ey\\with-quotes-123'
        try:
            obj = rig.redact_obj({"score": {"tail": 'x k"ey\\with-quotes-123 y'}, "l": ['k"ey\\with-quotes-123']})
            text = rig.redact(json.dumps(obj))
            self.assertNotIn("with-quotes-123", text)
        finally:
            if saved is None:
                os.environ.pop("ABHED_BENCH_WATSONX_APIKEY", None)
            else:
                os.environ["ABHED_BENCH_WATSONX_APIKEY"] = saved

    def test_stale_environments_and_ids_outside_the_pool_are_refused(self):
        real = rig.instances
        rig.instances = lambda repo=None: [{"instance_id": "i1", "repo": "pytest-dev/pytest"}]
        try:
            d = self.root / "envs" / "i1"
            d.mkdir(parents=True)
            (d / "ready").write_text("old spec")
            self.assertEqual(rig.stale_envs(["i1", "gone"]), ["i1", "gone"])
            (d / "ready").write_text(rig.env_spec("pytest-dev/pytest"))
            self.assertEqual(rig.stale_envs(["i1"]), [])
        finally:
            rig.instances = real

    def test_a_failed_rebuild_keeps_the_validated_suite(self):
        import argparse
        import contextlib
        import io
        real = (rig.instances, rig.sh)
        rig.instances = lambda repo=None: [{"instance_id": "i1", "repo": "pytest-dev/pytest", "base_commit": "x"}]

        def failing(cmd, **kw):
            raise RuntimeError("network down")

        rig.sh = failing
        try:
            (self.root / "suite.json").write_text('{"i1": []}')
            with contextlib.redirect_stdout(io.StringIO()):
                rig.prepare(argparse.Namespace(repo=None, limit=None))
            self.assertTrue((self.root / "suite.json").exists())
        finally:
            rig.instances, rig.sh = real

    def test_watch_reads_without_locks(self):
        seen = []
        real = rig.sh

        def sh(cmd, cwd=None, env=None, timeout=None, check=False):
            seen.append(cmd)
            return 0, ""

        rig.sh = sh
        try:
            rig.editing(self.root / "ws")
        finally:
            rig.sh = real
        self.assertTrue(seen and all("--no-optional-locks" in c for c in seen))

    def test_a_managed_config_keeps_abhed_out_of_the_run(self):
        real = rig.MANAGED_CONFIG
        rig.MANAGED_CONFIG = str(self.root / "managed.json")
        try:
            Path(rig.MANAGED_CONFIG).write_text("{}")
            ok, why = rig.Abhed().available()
            self.assertFalse(ok)
            self.assertIn("override", why)
        finally:
            rig.MANAGED_CONFIG = real

    def test_a_doctor_pass_is_tied_to_the_setup(self):
        a = rig.setup_fingerprint("abhed")
        real = rig.MAX_OUTPUT
        rig.MAX_OUTPUT = real + 1
        try:
            self.assertNotEqual(a, rig.setup_fingerprint("abhed"))
        finally:
            rig.MAX_OUTPUT = real


class ResumeTests(UsingCache, unittest.TestCase):
    def test_a_resume_with_another_setup_is_refused(self):
        plan = self.root / "plan.json"
        setup = {"base_model": "m", "max_output": 8192, "timeout_sec": 1800, "sessions": [1]}
        rig.check_resume(plan, setup)  # nothing to resume: fine
        plan.write_text(json.dumps({"started": "t", **setup}))
        rig.check_resume(plan, dict(setup))  # the same run: fine
        with self.assertRaises(SystemExit) as e:
            rig.check_resume(plan, dict(setup, max_output=4096))
        self.assertIn("max_output", str(e.exception))
        rig.check_resume(plan, dict(setup, max_output=4096), force=True)

    def test_a_result_is_written_whole_or_not_at_all(self):
        path = self.root / "r.json"
        rig.write_atomic(path, '{"a": 1}')
        self.assertEqual(json.loads(path.read_text()), {"a": 1})
        self.assertEqual([p.name for p in self.root.iterdir() if p.name.endswith(".tmp")], [])


ESCAPER = ("import os, subprocess, sys, time; "
           "c = subprocess.Popen(['sleep', '60'], start_new_session=True); "
           "open(sys.argv[1], 'w').write(str(c.pid)); time.sleep(60)")


class EscapedToolTests(UsingCache, unittest.TestCase):
    """pi and OpenHands start tools in a session of their own; the rig must
    still end them with the harness."""

    def escaped_pid(self, path):
        import time as _t
        for _ in range(100):
            if path.exists() and path.read_text().strip():
                return int(path.read_text())
            _t.sleep(0.05)
        self.fail("the tool never started")

    def test_a_tool_in_its_own_session_dies_with_a_timed_out_harness(self):
        import os
        import subprocess
        pidfile = self.root / "tool.pid"
        ws = self.root / "scratch" / "ws"
        ws.mkdir(parents=True)
        rig._SESSIONS["t-timeout"] = [str(ws)]
        self.addCleanup(rig._SESSIONS.pop, "t-timeout", None)
        env = dict(os.environ, **{rig.MARKER: "t-timeout"})
        with self.assertRaises(subprocess.TimeoutExpired):
            rig.sh([sys.executable, "-c", ESCAPER, str(pidfile)], cwd=ws, env=env, timeout=2)
        tool = self.escaped_pid(pidfile)
        import time as _t
        _t.sleep(0.3)
        self.assertEqual(_pids_alive([tool]), [], "the escaped tool outlived the timeout")

    def test_a_tool_in_its_own_session_dies_when_the_rig_is_stopped(self):
        import signal as s_
        import subprocess
        import time as _t
        pidfile = self.root / "tool.pid"
        ws = self.root / "scratch" / "ws"
        ws.mkdir(parents=True)
        code = (f"import os, sys; sys.path.insert(0, {str(Path(__file__).parent)!r}); import rig; "
                f"rig.CACHE = rig.Path({str(self.root)!r}); rig._SESSIONS['t-stop'] = [{str(ws)!r}]; "
                f"rig.install_stop_handlers(); "
                f"rig.sh([sys.executable, '-c', {ESCAPER!r}, {str(pidfile)!r}], cwd={str(ws)!r}, "
                f"env=dict(os.environ, **{{rig.MARKER: 't-stop'}}))")
        proc = subprocess.Popen([sys.executable, "-c", code], stderr=subprocess.DEVNULL)
        tool = self.escaped_pid(pidfile)
        proc.send_signal(s_.SIGTERM)
        try:
            proc.wait(timeout=20)
        except subprocess.TimeoutExpired:
            proc.kill()
        _t.sleep(0.3)
        alive = _pids_alive([tool])
        for pid in alive:
            import os
            os.kill(pid, s_.SIGKILL)
        self.assertEqual(alive, [], "the escaped tool outlived the rig")


class RoundSixTests(UsingCache, unittest.TestCase):
    def test_stopped_is_not_swallowed_by_a_broad_except(self):
        try:
            try:
                raise rig.Stopped()
            except Exception:  # noqa: BLE001 - the point of the test
                self.fail("Stopped was caught as an ordinary error")
        except rig.Stopped:
            pass

    def test_a_failed_write_leaves_the_old_result_whole(self):
        path = self.root / "r.json"
        path.write_text('{"old": 1}')
        real = os.replace
        os.replace = lambda *a: (_ for _ in ()).throw(OSError("disk full"))
        try:
            with self.assertRaises(OSError):
                rig.write_atomic(path, '{"new": 1}')
        finally:
            os.replace = real
        self.assertEqual(json.loads(path.read_text()), {"old": 1})

    def test_a_stop_during_analysis_leaves_no_record_beside_the_results(self):
        real = (rig.sh, rig.Abhed.binary)
        bin_ = self.root / "abhed"
        bin_.write_text("")
        rig.Abhed.binary = lambda self: str(bin_)

        def stop(*a, **k):
            raise SystemExit(143)

        rig.sh = stop
        try:
            out = self.root / "r" / "i1.json"
            out.parent.mkdir()
            with self.assertRaises(SystemExit):
                rig.hawkeye([{"type": "x", "seq": 1}], out)
            self.assertFalse((out.parent / "i1.events.json").exists())
        finally:
            rig.sh, rig.Abhed.binary = real

    def test_version_does_not_swallow_a_stop(self):
        real = rig.sh

        def stop(*a, **k):
            raise rig.Stopped()

        rig.sh = stop
        try:
            with self.assertRaises(rig.Stopped):
                rig.Pi().version()
        finally:
            rig.sh = real

    def test_the_recorded_endpoint_carries_no_credentials(self):
        saved = os.environ.get("ABHED_BENCH_ENDPOINT")
        os.environ["ABHED_BENCH_ENDPOINT"] = "https://user:pass@host.example:8443/v1?key=abc"
        try:
            self.assertEqual(rig.public_endpoint(), "https://host.example:8443/v1")
        finally:
            if saved is None:
                os.environ.pop("ABHED_BENCH_ENDPOINT", None)
            else:
                os.environ["ABHED_BENCH_ENDPOINT"] = saved



class OrphanSweepTests(UsingCache, unittest.TestCase):
    def test_a_tool_orphaned_by_a_harness_that_exited_is_swept(self):
        import time as _t
        pidfile = self.root / "tool.pid"
        ws = self.root / "scratch" / "ws"
        ws.mkdir(parents=True)
        rig._SESSIONS["t"] = [str(ws)]
        self.addCleanup(rig._SESSIONS.pop, "t", None)
        # The harness starts a tool in its own session and exits at once.
        quick = ("import subprocess, sys; c = subprocess.Popen(['sleep', '60'], start_new_session=True); "
                 "open(sys.argv[1], 'w').write(str(c.pid))")
        rig.sh([sys.executable, "-c", quick, str(pidfile)], cwd=ws, env=dict(os.environ, **{rig.MARKER: "t"}))
        tool = int(pidfile.read_text())
        _t.sleep(0.3)
        alive = _pids_alive([tool])
        for pid in alive:
            os.kill(pid, 9)
        self.assertEqual(alive, [], "the orphaned tool outlived its session")


TERM_PROOF = ("import os, signal, subprocess, sys, time; "
              "c = subprocess.Popen([sys.executable, '-c', "
              "'import signal, time; signal.signal(signal.SIGTERM, signal.SIG_IGN); time.sleep(60)'], "
              "start_new_session=True, cwd=os.environ['TMPDIR']); "
              "open(sys.argv[1], 'w').write(str(c.pid)); time.sleep({wait})")


class OutsideTheWorkspaceTests(UsingCache, unittest.TestCase):
    """A tool that ignores SIGTERM, runs in a session of its own and works in
    the session's temp dir, not its workspace: the hardest one to end."""

    def session(self):
        ws, tmp = self.root / "scratch" / "ws", self.root / "scratch" / "tmp"
        ws.mkdir(parents=True)
        tmp.mkdir(parents=True)
        rig._SESSIONS["t"] = [str(ws), str(tmp)]
        self.addCleanup(rig._SESSIONS.pop, "t", None)
        env = dict(os.environ, TMPDIR=str(tmp), **{rig.MARKER: "t"})
        return ws, env

    def tool_pid(self, pidfile):
        import time as _t
        for _ in range(100):
            if pidfile.exists() and pidfile.read_text().strip():
                return int(pidfile.read_text())
            _t.sleep(0.05)
        self.fail("the tool never started")

    def gone(self, pid):
        import time as _t
        _t.sleep(0.3)
        alive = _pids_alive([pid])
        for p in alive:
            os.kill(p, 9)
        return alive == []

    def test_on_timeout(self):
        import subprocess
        ws, env = self.session()
        pidfile = self.root / "tool.pid"
        with self.assertRaises(subprocess.TimeoutExpired):
            rig.sh([sys.executable, "-c", TERM_PROOF.format(wait=60), str(pidfile)], cwd=ws, env=env, timeout=2)
        self.assertTrue(self.gone(self.tool_pid(pidfile)))

    def test_after_a_normal_exit(self):
        ws, env = self.session()
        pidfile = self.root / "tool.pid"
        rig.sh([sys.executable, "-c", TERM_PROOF.format(wait=0), str(pidfile)], cwd=ws, env=env)
        self.assertTrue(self.gone(self.tool_pid(pidfile)))

    def run_rig_and_signal(self, signals):
        import signal as s_
        import subprocess
        import time as _t
        ws, env = self.session()
        pidfile = self.root / "tool.pid"
        code = (f"import os, sys; sys.path.insert(0, {str(Path(__file__).parent)!r}); import rig; "
                f"rig.CACHE = rig.Path({str(self.root)!r}); "
                f"rig._SESSIONS['t'] = [{str(ws)!r}, {env['TMPDIR']!r}]; rig.install_stop_handlers(); "
                f"rig.sh([sys.executable, '-c', {TERM_PROOF.format(wait=60)!r}, {str(pidfile)!r}], "
                f"cwd={str(ws)!r}, env={env!r})")
        proc = subprocess.Popen([sys.executable, "-c", code], stderr=subprocess.DEVNULL)
        tool = self.tool_pid(pidfile)
        for sig in signals:
            proc.send_signal(sig)
            _t.sleep(0.5)
        try:
            proc.wait(timeout=20)
        except subprocess.TimeoutExpired:
            proc.kill()
        return tool

    def test_on_stop(self):
        import signal as s_
        self.assertTrue(self.gone(self.run_rig_and_signal([s_.SIGTERM])))

    def test_a_second_ctrl_c_does_not_cut_the_stop_short(self):
        import signal as s_
        self.assertTrue(self.gone(self.run_rig_and_signal([s_.SIGINT, s_.SIGINT])))


class SparedProcessTests(UsingCache, unittest.TestCase):
    def test_someone_elses_process_in_the_workspace_is_left_alone(self):
        import subprocess
        ws = self.root / "scratch" / "ws"
        ws.mkdir(parents=True)
        rig._SESSIONS["t"] = [str(ws)]
        self.addCleanup(rig._SESSIONS.pop, "t", None)
        bystander = subprocess.Popen(["sleep", "30"], cwd=ws)  # the operator's shell, say
        try:
            rig.sh(["true"], cwd=ws, env=dict(os.environ, **{rig.MARKER: "t"}))
            self.assertIsNone(bystander.poll(), "a process that was not the session's was killed")
        finally:
            bystander.kill()
            bystander.wait()

    def test_each_session_gets_its_own_tmux_server(self):
        env = rig.task_env("i1", self.root / "ws", tmp=self.root / "t1")
        self.assertEqual(env["TMUX_TMPDIR"], str(self.root / "t1"))

    def test_a_plan_from_before_parallelism_was_recorded_resumes(self):
        plan = self.root / "plan.json"
        plan.write_text(json.dumps({"base_model": "m", "sessions": [1]}))
        rig.check_resume(plan, {"base_model": "m", "sessions": [1], "parallel": 1})

    def test_watch_finds_a_session_by_its_record_not_its_name(self):
        tag = rig.scratch_tag("pi", "full", "i9", 2, Path("/r/x.json"))
        (self.root / "scratch" / tag).mkdir(parents=True)
        rig.session_meta(tag).write_text(json.dumps({"harness": "pi", "condition": "full", "instance": "i9", "run": 2}))
        d, h, cond, iid = rig._current()
        self.assertEqual((d.name, h, cond, iid), (tag, "pi", "full", "i9"))


class SweepBoundaryTests(UsingCache, unittest.TestCase):
    """The sweep must never reach beyond the rig's own session directories:
    not the operator's home, not their temp dir, whatever an environment says."""

    def test_the_environment_never_names_what_is_swept(self):
        seen = {}
        real = rig._leftovers

        def spy(marker, dirs, parents=None):
            seen["dirs"] = list(dirs)
            return []

        rig._leftovers = spy
        try:
            rig.sh(["true"], cwd=Path.home(), env=dict(os.environ, **{rig.MARKER: "unregistered"}))
        finally:
            rig._leftovers = real
        self.assertEqual(seen["dirs"], [])

    def test_only_directories_inside_the_scratch_are_sweepable(self):
        self.assertFalse(rig._sweepable(Path.home()))
        self.assertFalse(rig._sweepable(tempfile.gettempdir()))
        self.assertFalse(rig._sweepable(self.root / "scratch"))
        self.assertFalse(rig._sweepable("/"))
        self.assertTrue(rig._sweepable(self.root / "scratch" / "s0123"))

    def test_a_registered_directory_outside_the_scratch_is_ignored(self):
        seen = {}
        real = rig._cwds
        rig._cwds = lambda: {12345: str(Path.home())}
        try:
            got = rig._leftovers("x", [str(Path.home())], parents={12345: 1})
        finally:
            rig._cwds = real
        self.assertEqual(got, [], "an orphan in the operator's home was targeted")


TWO_LEVEL = ("import os, subprocess, sys, time; "
             "p = subprocess.Popen([sys.executable, '-c', "
             "'import subprocess, sys, time; "
             "c = subprocess.Popen([\"sleep\", \"60\"], cwd=sys.argv[2]); "
             "open(sys.argv[1], \"w\").write(str(c.pid)); time.sleep(60)', sys.argv[1], sys.argv[2]], "
             "start_new_session=True); time.sleep({wait})")


class TwoLevelTreeTests(UsingCache, unittest.TestCase):
    """A detached parent working in the workspace, and its child working
    elsewhere: a flask reloader and its server, say."""

    def setUp(self):
        super().setUp()
        self.ws = self.root / "scratch" / "ws"
        self.ws.mkdir(parents=True)
        self.elsewhere = self.root / "elsewhere"
        self.elsewhere.mkdir()
        rig._SESSIONS["t2"] = [str(self.ws)]
        self.addCleanup(rig._SESSIONS.pop, "t2", None)
        self.pidfile = self.root / "child.pid"
        self.env = dict(os.environ, **{rig.MARKER: "t2"})

    def child(self):
        import time as _t
        for _ in range(100):
            if self.pidfile.exists() and self.pidfile.read_text().strip():
                return int(self.pidfile.read_text())
            _t.sleep(0.05)
        self.fail("the child never started")

    def assert_gone(self, pid):
        import time as _t
        _t.sleep(0.3)
        alive = _pids_alive([pid])
        for p in alive:
            os.kill(p, 9)
        self.assertEqual(alive, [], "the orphan's child survived")

    def cmd(self, wait):
        return [sys.executable, "-c", TWO_LEVEL.format(wait=wait), str(self.pidfile), str(self.elsewhere)]

    def test_after_a_normal_exit(self):
        import time as _t
        rig.sh(self.cmd(1), cwd=self.ws, env=self.env)
        self.assert_gone(self.child())

    def test_on_timeout(self):
        import subprocess
        with self.assertRaises(subprocess.TimeoutExpired):
            rig.sh(self.cmd(60), cwd=self.ws, env=self.env, timeout=2)
        self.assert_gone(self.child())


class RoundEightTests(UsingCache, unittest.TestCase):
    def test_a_second_stop_signal_leaves_the_first_one_to_finish(self):
        sent = []
        real = rig._signal
        rig._signal = lambda pids, sig, group=False: sent.append(sig)
        rig._Stop.handling = True
        try:
            rig.stop_all(2)  # must neither raise nor signal: the first handler is mid-way
        finally:
            rig._signal = real
            rig._Stop.handling = False
            rig._Stop.signum = 0
        self.assertEqual(sent, [])

    def test_a_session_that_names_the_answers_is_flagged(self):
        self.assertTrue(rig.touched_answers("cat ../../.cache/verified.jsonl"))
        self.assertTrue(rig.touched_answers(f"ls {rig.CACHE / 'envs'}"))
        self.assertFalse(rig.touched_answers("pytest testing/test_pastebin.py"))

    def test_the_venv_copy_does_not_name_the_task(self):
        iid = "pytest-dev__pytest-9999"
        src = self.root / "envs" / iid / "venv"
        site = src / "lib" / "python3.9" / "site-packages"
        site.mkdir(parents=True)
        (src / "bin").mkdir()
        repo = str(self.root / "envs" / iid / "repo")
        (site / "__editable__.pytest.pth").write_text(repo + "\n")
        (site / "direct_url.json").write_text(json.dumps({"url": "file://" + repo}))
        ws = self.root / "scratch" / "s0"
        copy = rig.session_venv(iid, self.root / "scratch" / "venv-s0", ws)
        for f in copy.rglob("*"):
            if f.is_file():
                self.assertNotIn(iid, f.read_text(), f.name)

    def test_the_session_record_is_outside_the_scratch_tree(self):
        self.assertFalse(rig.session_meta("s0").is_relative_to(self.root / "scratch"))

    def test_a_plan_from_an_older_rig_is_named_as_such(self):
        plan = self.root / "plan.json"
        plan.write_text(json.dumps({"sessions": [1]}))
        with self.assertRaises(SystemExit) as e:
            rig.check_resume(plan, {"sessions": [1], "max_output": 8192})
        self.assertIn("without max_output", str(e.exception))

    def test_a_cache_too_deep_for_tmux_sockets_is_refused(self):
        saved = rig.CACHE
        rig.CACHE = self.root / ("x" * 80)
        try:
            with self.assertRaises(SystemExit):
                rig.check_socket_room()
        finally:
            rig.CACHE = saved


if __name__ == "__main__":
    unittest.main()
