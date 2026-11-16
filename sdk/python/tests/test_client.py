import json
import os
import sys
import threading
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from abhed_sdk import (  # noqa: E402
    Abhed,
    AbhedError,
    Answer,
    ApprovalRequest,
    Decision,
    Event,
    ProcessExited,
    Runbook,
    RunError,
    args_hidden,
    visible,
)

STUB = os.path.join(os.path.dirname(__file__), "stub_rpc.py")


def stub(**kw):
    return Abhed(sys.executable, args=["-I", STUB], **kw)


class ClientTest(unittest.TestCase):
    def test_prompt_returns_answer_and_verdict(self):
        seen = []
        with stub() as ab:
            s = ab.start(on_event=seen.append)
            self.assertFalse(s.workspace_trust.trusted)
            a = s.prompt("hello")
            self.assertEqual(a.text, "hi")
            self.assertIsNotNone(a.verdict)
            self.assertTrue(a.verdict.ok)
            self.assertEqual(s.session_id, "s-stub")
        types = [e.type for e in seen]
        self.assertEqual(types, ["user.message", "agent.message", "answer.verdict"])
        self.assertEqual([e.seq for e in seen], [1, 2, 3])

    def test_start_options_reach_the_engine(self):
        rb = Runbook(plan="p", runbook=[{"tool": "bash", "args": {"command": "make"}}], sha256="ab")
        with stub() as ab:
            s = ab.start(workspace="/w", mode="plan", allow=["bash(go test*)"], deny=["web_fetch"],
                         verify="strict", wake="notify", runbook=rb, approvals=True)
            got = json.loads(s.prompt("echo-start").text)
        self.assertEqual(got["method"], "start")
        self.assertEqual(got["workspace"], "/w")
        self.assertEqual(got["mode"], "plan")
        self.assertEqual(got["allow"], ["bash(go test*)"])
        self.assertEqual(got["deny"], ["web_fetch"])
        self.assertEqual(got["verify"], "strict")
        self.assertEqual(got["wake"], "notify")
        self.assertEqual(got["runbook"], {"plan": "p", "runbook": [{"tool": "bash", "args": {"command": "make"}}], "sha256": "ab"})
        self.assertIs(got["approvals"], True)

    def test_no_approvals_unless_asked(self):
        with stub() as ab:
            got = json.loads(ab.start().prompt("echo-start").text)
            self.assertNotIn("approvals", got)
            self.assertEqual(ab.start().prompt("approve-me").text, "refused")

    def test_approval_handler_answers(self):
        asked = []

        def decide(req):
            asked.append(req)
            return Decision.allow_once(approver="olga@example.com")

        with stub() as ab:
            s = ab.start(on_approval=decide)
            a = s.prompt("approve-me")
        self.assertEqual(a.text, "allowed by olga@example.com")
        self.assertEqual(len(asked), 1)
        r = asked[0]
        self.assertIsInstance(r, ApprovalRequest)
        self.assertEqual((r.request_id, r.tool, r.step), ("r-1", "bash", "destructive"))
        self.assertTrue(r.destructive)
        self.assertTrue(r.args_hidden)
        self.assertTrue(args_hidden(r.args))
        self.assertIn("\\u{202E}", visible(r.args["command"]))

    def test_approval_handler_that_raises_denies(self):
        def boom(req):
            raise RuntimeError("no")

        notes = []
        with stub(on_error=notes.append) as ab:
            a = ab.start(on_approval=boom).prompt("approve-me")
        self.assertEqual(a.text, "denied remember=False")
        self.assertTrue(any("on_approval raised" in n for n in notes))

    def test_stream_yields_events_approvals_and_answer(self):
        with stub() as ab:
            s = ab.start(approvals=True)
            items = []
            for item in s.stream("approve-me"):
                items.append(item)
                if isinstance(item, ApprovalRequest):
                    s.approve(item.request_id, Decision.deny())
        self.assertIsInstance(items[-1], Answer)
        self.assertEqual(items[-1].text, "denied remember=False")
        self.assertTrue(any(isinstance(i, ApprovalRequest) for i in items))
        kinds = [i.type for i in items if isinstance(i, Event)]
        self.assertEqual(kinds[:2], ["user.message", "action.requested"])
        self.assertIn("action.denied", kinds)

    def test_strict_unverified_is_a_run_error(self):
        with stub() as ab:
            s = ab.start(verify="strict")
            with self.assertRaises(RunError) as cm:
                s.prompt("strict")
        e = cm.exception
        self.assertTrue(e.unverified)
        self.assertEqual(e.reason, "unverified")
        self.assertEqual(e.detail, "1 statement about the work is unverified")
        self.assertEqual(e.answer, "the tests pass")
        self.assertEqual(e.verdict.outcome, "unverified")
        self.assertEqual(e.verdict.claims[0].reason, "no test run in the record")

    def test_steer_reaches_running_prompt(self):
        with stub() as ab:
            s = ab.start()
            result = {}
            t = threading.Thread(target=lambda: result.setdefault("a", s.prompt("steer-me")))
            t.start()
            self.assertEqual(s.steer("look at auth too"), "steered")
            t.join(10)
            self.assertEqual(result["a"].text, "steered: look at auth too")
            self.assertEqual(s.steer("later"), "queued")

    def test_usage_export_tasks_providers(self):
        with stub() as ab:
            self.assertEqual(ab.providers(), ["anthropic", "openai-compatible"])
            s = ab.start()
            u = s.usage()
            self.assertEqual((u.input_tokens, u.output_tokens, u.turns), (120, 8, 2))
            self.assertAlmostEqual(u.cost_usd, 0.0012)
            self.assertIn("<title>s-stub</title>", s.export_html())
            ts = s.tasks()
            self.assertEqual((ts[0].task_id, ts[0].status), ("t1", "running"))
            s.cancel_task("t1")
            with self.assertRaises(RunError):
                s.cancel_task("t9")
            with self.assertRaises(RunError):
                s.wake()

    def test_woken_and_stray_errors(self):
        woken, notes = threading.Event(), []
        got = {}

        def on_woken(w):
            got["w"] = w
            woken.set()

        with stub(on_error=notes.append) as ab:
            s = ab.start(on_woken=on_woken)
            self.assertEqual(s.prompt("wake-later").text, "started")
            self.assertTrue(woken.wait(10))
            self.assertEqual(got["w"].answer, "background done")
            self.assertEqual(s.prompt("stray-error").text, "ok")
        self.assertTrue(any("not delivered" in n for n in notes))

    def test_failed_start_and_superseded_session(self):
        with stub() as ab:
            with self.assertRaises(RunError) as cm:
                ab.start(workspace="/nope")
            self.assertIn("does not exist", str(cm.exception))
            first = ab.start()
            ab.start()
            with self.assertRaises(AbhedError):
                first.prompt("hello")

    def test_process_death_fails_the_request(self):
        ab = stub()
        s = ab.start()
        with self.assertRaises(ProcessExited) as cm:
            s.prompt("die")
        self.assertEqual(cm.exception.returncode, 3)
        self.assertIn("boom", ab.stderr)
        with self.assertRaises(ProcessExited):
            s.prompt("hello")
        ab.close()

    def test_missing_binary(self):
        with self.assertRaises(ProcessExited):
            Abhed("/nonexistent/abhed")

    def test_close_reports_exit(self):
        ab = stub()
        ab.start()
        self.assertEqual(ab.close(), 0)


class VisibleTest(unittest.TestCase):
    def test_marks_hidden(self):
        self.assertEqual(visible("a​b"), "a\\u{200B}b")
        self.assertEqual(visible("x\x1b[2J"), "x\\u{001B}[2J")
        self.assertEqual(visible("a\nb"), "a\nb")
        self.assertEqual(visible("a\nb", one_line=True), "a\\u{000A}b")
        self.assertEqual(visible("plain text\tok"), "plain text\tok")

    def test_args_hidden(self):
        self.assertFalse(args_hidden({"command": "ls -la\n"}))
        self.assertTrue(args_hidden({"command": "ls ‮"}))
        self.assertTrue(args_hidden({"k​": "v"}))
        self.assertTrue(args_hidden({"manifest": '{"a": "\\u001b"}'}))


if __name__ == "__main__":
    unittest.main()
