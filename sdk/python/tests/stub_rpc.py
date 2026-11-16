"""A stand-in for ``abhed rpc``: the protocol's shapes, with scripted answers.

What each prompt does is chosen by its text, so the tests can drive every
kind of line the real engine sends without a model. The shapes follow
app/rpc.go; test_e2e.py checks the same client against the real binary.
"""

import json
import sys

SID = "s-stub"
seq = 0
start_req = None
approvals = False
held = []  # requests read while a prompt waited for an approve or steer


def out(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def event(kind, payload, actor="agent"):
    global seq
    seq += 1
    out({"type": "event", "event": {"id": f"e{seq}", "session_id": SID, "seq": seq, "type": kind,
                                    "payload": payload, "actor": actor, "trust": "trusted",
                                    "created_at": "2026-11-17T00:00:00Z"}})


VERIFIED = {"mode": "label", "source": "text", "claims": [], "verified": 0, "unverified": 0,
            "contradicted": 0, "outcome": "verified", "note": "nothing to check", "turn": 1}


def read():
    if held:
        return held.pop(0)
    line = sys.stdin.readline()
    if not line:
        return None
    return json.loads(line)


def wait_for(method, rid=None):
    """Reads until a request of method arrives, holding the others."""
    later = []
    while True:
        line = sys.stdin.readline()
        if not line:
            held.extend(later)
            return None
        r = json.loads(line)
        if r.get("method") == method:
            if method == "approve" and r.get("request_id") != rid:
                out({"id": r.get("id"), "type": "error", "error": "no approval is waiting with request_id " + str(r.get("request_id"))})
                continue
            held.extend(later)
            return r
        later.append(r)


def prompt(r):
    text = r.get("prompt", "")
    rid = r.get("id")
    event("user.message", {"text": text}, "user")
    if text == "hello":
        event("agent.message", {"text": "hi"})
        event("answer.verdict", VERIFIED, "system")
        out({"id": rid, "type": "answer", "answer": "hi", "verdict": VERIFIED})
    elif text == "echo-start":
        out({"id": rid, "type": "answer", "answer": json.dumps(start_req, sort_keys=True)})
    elif text == "approve-me":
        args = {"command": "rm -rf build‮", "description": "clean"}
        event("action.requested", {"call_id": "c1", "tool": "bash", "args": args, "requires_approval": True})
        if not approvals:
            event("action.denied", {"call_id": "c1", "by": "headless"}, "system")
            out({"id": rid, "type": "answer", "answer": "refused"})
            return
        out({"type": "approval", "approval": {"request_id": "r-1", "call_id": "c1", "tool": "bash", "args": args,
                                               "reason": "a destructive command", "step": "destructive",
                                               "rule": "builtin:destructive", "offer": "", "destructive": True,
                                               "args_hidden": True}})
        a = wait_for("approve", "r-1")
        if a is None:
            event("action.denied", {"call_id": "c1", "by": "system", "reason": "rpc input ended"}, "system")
            return
        out({"id": a.get("id"), "type": "answered"})
        if a.get("decision") == "allow":
            event("action.approved", {"call_id": "c1", "by": "reviewer", "approver": a.get("approver", "")}, "user")
            out({"id": rid, "type": "answer", "answer": "allowed by " + a.get("approver", "")})
        else:
            event("action.denied", {"call_id": "c1", "by": "reviewer"}, "user")
            out({"id": rid, "type": "answer", "answer": "denied remember=%s" % a.get("remember", False)})
    elif text == "strict":
        v = dict(VERIFIED, mode="strict", outcome="unverified", unverified=1,
                 claims=[{"n": 1, "text": "tests pass", "kind": "test", "verdict": "unverified",
                          "reason": "no test run in the record", "load_bearing": True}])
        event("answer.verdict", v, "system")
        out({"id": rid, "type": "error", "error": "abhed: ended as unverified: 1 statement about the work is unverified",
             "answer": "the tests pass", "verdict": v})
    elif text == "steer-me":
        event("agent.delta", {"text": "working"})
        s = wait_for("steer")
        out({"id": s.get("id"), "type": "steered"})
        out({"id": rid, "type": "answer", "answer": "steered: " + s.get("prompt", "")})
    elif text == "wake-later":
        out({"id": rid, "type": "answer", "answer": "started"})
        event("subagent.notice", {"task_id": "t1"}, "system")
        out({"type": "woken", "answer": "background done"})
    elif text == "stray-error":
        out({"type": "error", "error": "1 queued steer message(s) were not delivered: test"})
        out({"id": rid, "type": "answer", "answer": "ok"})
    elif text == "die":
        sys.stderr.write("abhed: boom\n")
        sys.stderr.flush()
        sys.exit(3)
    else:
        out({"id": rid, "type": "answer", "answer": "echo: " + text})


def main():
    global start_req, approvals
    if sys.argv[-1] != "rpc":
        sys.stderr.write("stub: want rpc\n")
        sys.exit(2)
    started = False
    while True:
        r = read()
        if r is None:
            return 0
        m, rid = r.get("method"), r.get("id")
        if m == "start":
            if r.get("workspace") == "/nope":
                out({"id": rid, "type": "error", "error": "abhed: workspace /nope does not exist"})
                continue
            start_req, approvals, started = r, bool(r.get("approvals")), True
            out({"id": rid, "type": "ready", "workspace_trust": {"workspace": r.get("workspace", "/w"),
                                                                  "trusted": False, "reason": "none"}})
        elif m == "providers":
            out({"id": rid, "type": "providers", "providers": ["anthropic", "openai-compatible"]})
        elif m == "quit":
            out({"id": rid, "type": "bye"})
            return 0
        elif m == "approve":
            out({"id": rid, "type": "error", "error": "no approval is waiting with request_id " + str(r.get("request_id"))})
        elif m == "steer":
            out({"id": rid, "type": "queued"})
        elif not started:
            out({"id": rid, "type": "error", "error": "no session: send start first"})
        elif m == "prompt":
            prompt(r)
        elif m == "usage":
            out({"id": rid, "type": "usage", "usage": {"InputTokens": 120, "OutputTokens": 8, "CachedTokens": 64,
                                                       "Turns": 2, "Compactions": 0, "CostUSD": 0.0012, "Unpriced": False}})
        elif m == "export":
            out({"id": rid, "type": "export", "answer": "<!doctype html><title>s-stub</title>"})
        elif m == "tasks":
            out({"id": rid, "type": "tasks", "tasks": [{"task_id": "t1", "kind": "task", "description": "scan",
                                                        "status": "running", "started": "2026-11-17T00:00:00Z"}]})
        elif m == "cancel_task":
            if r.get("task_id") == "t1":
                out({"id": rid, "type": "cancelled"})
            else:
                out({"id": rid, "type": "error", "error": "no background task " + str(r.get("task_id"))})
        elif m == "wake":
            out({"id": rid, "type": "error", "error": "nothing to wake"})
        else:
            out({"id": rid, "type": "error", "error": "unknown method %r" % m})


if __name__ == "__main__":
    sys.exit(main())
