#!/usr/bin/env python3
"""Checks on the parts of the rig that would misreport quietly if wrong."""
import sys
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


if __name__ == "__main__":
    unittest.main()
