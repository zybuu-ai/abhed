"""The client against the real ``abhed rpc``, on a scripted model.

Skipped unless ABHED_BIN names an abhed binary. The model is a local
OpenAI-compatible stub on 127.0.0.1, HOME is a temporary folder, and the
workspace's configuration is the test's own, so nothing outside the test is
read or written and no model is contacted.
"""

import json
import os
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from abhed_sdk import Abhed, Decision, RunError  # noqa: E402

BIN = os.environ.get("ABHED_BIN", "")


def sse(chunks):
    return "".join("data: %s\n\n" % json.dumps(c) for c in chunks) + "data: [DONE]\n\n"


class Model:
    """Answers the first request with one bash call, the rest with text."""

    def __init__(self, command):
        self.command = command
        self.calls = 0
        model = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", "0") or 0))
                model.calls += 1
                if model.calls == 1:
                    args = json.dumps({"command": model.command, "description": "probe"})
                    body = sse([
                        {"choices": [{"delta": {"tool_calls": [{"index": 0, "id": "c1", "function": {"name": "bash", "arguments": args}}]}}]},
                        {"choices": [{"delta": {}, "finish_reason": "tool_calls"}]},
                    ])
                else:
                    body = sse([{"choices": [{"delta": {"content": "done"}, "finish_reason": "stop"}]}])
                data = body.encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.url = "http://127.0.0.1:%d/v1" % self.srv.server_address[1]

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


@unittest.skipUnless(BIN, "set ABHED_BIN to an abhed binary to run against the real engine")
class EndToEnd(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.home = os.path.join(self.tmp.name, "home")
        self.ws = os.path.realpath(os.path.join(self.tmp.name, "ws"))
        os.makedirs(self.home)
        os.makedirs(os.path.join(self.ws, ".abhed"))

    def tearDown(self):
        self.tmp.cleanup()

    def client(self, model):
        cfg = {"model": {"default": "stub", "providers": {"stub": {
            "type": "openai-compatible", "base_url": model.url, "model": "m"}}}}
        with open(os.path.join(self.ws, ".abhed", "config.json"), "w") as f:
            json.dump(cfg, f)
        env = {"HOME": self.home, "PATH": os.environ.get("PATH", "/usr/bin:/bin"), "TMPDIR": self.tmp.name}
        return Abhed(BIN, cwd=self.ws, env=env, trust_workspace=True)

    def test_approved_call_runs_and_is_recorded(self):
        m = Model("touch made-by-e2e")
        self.addCleanup(m.close)
        asked, events = [], []

        def decide(req):
            asked.append(req)
            return Decision.allow_once(approver="e2e@example.com")

        with self.client(m) as ab:
            self.assertIn("openai-compatible", ab.providers())
            s = ab.start(on_approval=decide, on_event=events.append)
            self.assertTrue(s.workspace_trust.trusted)
            a = s.prompt("make a file")
            self.assertEqual(a.text, "done")
            self.assertIsNotNone(a.verdict)
            self.assertGreaterEqual(s.usage().turns, 2)
            self.assertIn("<!doctype html", s.export_html().lower())
            self.assertEqual(s.tasks(), [])
        self.assertEqual(len(asked), 1)
        self.assertEqual(asked[0].tool, "bash")
        self.assertEqual(asked[0].args["command"], "touch made-by-e2e")
        self.assertTrue(asked[0].request_id)
        approved = [e for e in events if e.type == "action.approved"]
        self.assertEqual(approved[0].payload.get("approver"), "e2e@example.com")
        self.assertTrue(os.path.exists(os.path.join(self.ws, "made-by-e2e")))
        self.assertTrue(s.session_id)

    def test_denied_call_does_not_run(self):
        m = Model("touch refused-e2e")
        self.addCleanup(m.close)
        with self.client(m) as ab:
            items = []
            s = ab.start(approvals=True)
            for item in s.stream("make a file"):
                items.append(item)
                if hasattr(item, "request_id"):
                    s.approve(item.request_id, Decision.deny())
        self.assertEqual(items[-1].text, "done")
        self.assertIn("action.denied", [getattr(i, "type", "") for i in items])
        self.assertFalse(os.path.exists(os.path.join(self.ws, "refused-e2e")))

    def test_without_approvals_the_call_is_refused(self):
        m = Model("touch never-e2e")
        self.addCleanup(m.close)
        with self.client(m) as ab:
            ab.start().prompt("make a file")
        self.assertFalse(os.path.exists(os.path.join(self.ws, "never-e2e")))

    def test_prompt_before_start_is_an_error(self):
        m = Model("true")
        self.addCleanup(m.close)
        with self.client(m) as ab:
            with self.assertRaises(RunError) as cm:
                ab._send({"method": "prompt", "prompt": "x"})
            self.assertIn("send start first", str(cm.exception))


if __name__ == "__main__":
    unittest.main()
