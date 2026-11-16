"""A client for ``abhed rpc``: one child process, line-delimited JSON on its pipes.

Nothing here opens a socket. The model, the tools, the policy and the record
are all the engine's; this module only starts the process, writes requests
and reads what comes back.
"""

from __future__ import annotations

import itertools
import json
import os
import queue
import subprocess
import threading
from collections import deque
from typing import (
    Any,
    Callable,
    Deque,
    Dict,
    Iterator,
    List,
    Mapping,
    Optional,
    Sequence,
    Union,
)

from ._types import (
    AbhedError,
    Answer,
    ApprovalRequest,
    Decision,
    Event,
    ProcessExited,
    Runbook,
    RunError,
    SteerResult,
    TaskInfo,
    Usage,
    Verdict,
    Woken,
    WorkspaceTrust,
)

EventHandler = Callable[[Event], None]
ApprovalHandler = Callable[[ApprovalRequest], Decision]
StreamItem = Union[Event, ApprovalRequest, Answer]

_STDERR_KEEP = 64 * 1024
_MAX_LINE = 8 << 20


class _Pending:
    __slots__ = ("done", "value")

    def __init__(self) -> None:
        self.done = threading.Event()
        self.value: Union[Dict[str, Any], BaseException, None] = None


class Abhed:
    """One ``abhed rpc`` process.

    ``binary`` is the abhed executable, ``cwd`` the folder it starts in (the
    default workspace), and ``env`` its environment (this process's when
    None): the model's API key reaches the engine from there, never through
    the protocol. ``trust_workspace`` passes ``-trust-workspace``, trusting
    the workspace's own ``.abhed/config.json``; leave it off for a checkout
    you did not write.

    Use it as a context manager, or call :meth:`close`.
    """

    def __init__(
        self,
        binary: str = "abhed",
        *,
        cwd: Optional[str] = None,
        env: Optional[Mapping[str, str]] = None,
        trust_workspace: bool = False,
        args: Sequence[str] = (),
        on_error: Optional[Callable[[str], None]] = None,
    ) -> None:
        argv = [binary, *(["-trust-workspace"] if trust_workspace else []), *args, "rpc"]
        try:
            self._proc = subprocess.Popen(
                argv,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                cwd=cwd,
                env=dict(env) if env is not None else None,
                bufsize=0,
            )
        except OSError as e:
            raise ProcessExited(f"abhed rpc did not start: {e}") from e
        self._ids = itertools.count(1)
        self._lock = threading.Lock()
        self._write_lock = threading.Lock()
        self._pending: Dict[str, _Pending] = {}
        self._closed: Optional[ProcessExited] = None
        self._stderr: Deque[bytes] = deque()
        self._stderr_len = 0
        self._session: Optional[Session] = None
        self._on_error = on_error
        self._dispatch: "queue.Queue[Optional[Callable[[], None]]]" = queue.Queue()
        self._threads = [
            threading.Thread(target=self._read, name="abhed-rpc-read", daemon=True),
            threading.Thread(target=self._read_stderr, name="abhed-rpc-stderr", daemon=True),
            threading.Thread(target=self._run_dispatch, name="abhed-rpc-dispatch", daemon=True),
        ]
        for t in self._threads:
            t.start()

    # -- lifecycle ---------------------------------------------------------

    def __enter__(self) -> "Abhed":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    @property
    def stderr(self) -> str:
        """The last 64 KiB abhed wrote to stderr: its warnings and notes."""
        with self._lock:
            return b"".join(self._stderr).decode("utf-8", "replace")

    @property
    def returncode(self) -> Optional[int]:
        return self._proc.poll()

    def close(self, timeout: float = 30.0) -> int:
        """Quit the session and wait for the process to exit.

        Running background tasks end as ``session_closed``. A process that
        has not exited after ``timeout`` seconds is sent SIGTERM, which ends
        the running prompt and records its end, then killed.
        """
        if self._proc.poll() is None and self._closed is None:
            try:
                self._send({"method": "quit"}, wait=False)
            except AbhedError:
                pass
        try:
            self._proc.stdin.close()  # type: ignore[union-attr]
        except OSError:
            pass
        try:
            code = self._proc.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            self._proc.terminate()
            try:
                code = self._proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self._proc.kill()
                code = self._proc.wait()
        for t in self._threads[:2]:
            t.join(timeout=5)
        for f in (self._proc.stdout, self._proc.stderr):
            if f is not None and not self._threads[0].is_alive() and not self._threads[1].is_alive():
                f.close()
        self._dispatch.put(None)
        self._threads[2].join(timeout=5)
        return code

    # -- requests ----------------------------------------------------------

    def start(
        self,
        *,
        workspace: Optional[str] = None,
        mode: Optional[str] = None,
        allow: Sequence[str] = (),
        deny: Sequence[str] = (),
        verify: Optional[str] = None,
        wake: Optional[str] = None,
        runbook: Optional[Runbook] = None,
        on_event: Optional[EventHandler] = None,
        on_approval: Optional[ApprovalHandler] = None,
        approvals: Optional[bool] = None,
        on_woken: Optional[Callable[[Woken], None]] = None,
    ) -> "Session":
        """Open a session; a session opened earlier on this process closes.

        ``on_event`` is called with every recorded event, in order, on a
        thread of this client's own. With ``on_approval``, each call needing
        approval is put to it on a thread of its own, and its
        :class:`Decision` answers. ``approvals=True`` without a handler puts
        them to :meth:`Session.stream` instead. Without either, a call that
        needs approval is refused, as in a headless run.
        """
        req: Dict[str, Any] = {"method": "start"}
        for k, v in (("workspace", workspace), ("mode", mode), ("verify", verify), ("wake", wake)):
            if v:
                req[k] = v
        if allow:
            req["allow"] = list(allow)
        if deny:
            req["deny"] = list(deny)
        if runbook is not None:
            req["runbook"] = runbook.to_json()
        ask = approvals if approvals is not None else on_approval is not None
        if ask:
            req["approvals"] = True
        sess = Session(self, on_event, on_approval, on_woken)
        # Set first: events of the new session may arrive before its ready.
        with self._lock:
            old, self._session = self._session, sess
        if old is not None:
            old._superseded = True
        r = self._send(req)
        sess.workspace_trust = WorkspaceTrust.from_json(r.get("workspace_trust"))
        return sess

    def providers(self) -> List[str]:
        """The model provider types this build of abhed supports."""
        return [str(p) for p in self._send({"method": "providers"}).get("providers") or []]

    # -- plumbing ----------------------------------------------------------

    def _send(self, req: Dict[str, Any], wait: bool = True, pending: Optional[_Pending] = None) -> Dict[str, Any]:
        rid = f"py-{next(self._ids)}"
        req = {"id": rid, **req}
        p = pending or _Pending()
        with self._lock:
            if self._closed is not None:
                raise self._closed
            self._pending[rid] = p
        line = (json.dumps(req, ensure_ascii=False) + "\n").encode("utf-8")
        try:
            with self._write_lock:
                self._proc.stdin.write(line)  # type: ignore[union-attr]
                self._proc.stdin.flush()  # type: ignore[union-attr]
        except (OSError, ValueError) as e:
            with self._lock:
                self._pending.pop(rid, None)
            raise ProcessExited(f"abhed rpc is not reading: {e}", self._proc.poll(), self.stderr) from e
        if not wait:
            return {}
        p.done.wait()
        v = p.value
        if isinstance(v, BaseException):
            raise v
        assert isinstance(v, dict)
        if v.get("type") == "error":
            raise RunError(str(v.get("error", "")), str(v.get("answer", "") or ""), Verdict.from_json(v.get("verdict")))
        return v

    def _read(self) -> None:
        out = self._proc.stdout
        assert out is not None
        while True:
            line = out.readline(_MAX_LINE + 1)
            if not line:
                break
            line = line.strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except ValueError:
                self._note(f"abhed rpc wrote a line that is not JSON: {line[:200]!r}")
                continue
            if isinstance(msg, dict):
                self._route(msg)
        code = self._proc.wait()
        err = ProcessExited(f"abhed rpc exited with code {code}", code, "")
        with self._lock:
            self._closed = err
            pending, self._pending = self._pending, {}
        for p in pending.values():
            p.value = ProcessExited(err.args[0], code, self.stderr)
            p.done.set()

    def _read_stderr(self) -> None:
        e = self._proc.stderr
        assert e is not None
        for chunk in iter(lambda: os.read(e.fileno(), 4096), b""):
            with self._lock:
                self._stderr.append(chunk)
                self._stderr_len += len(chunk)
                while self._stderr_len > _STDERR_KEEP and len(self._stderr) > 1:
                    self._stderr_len -= len(self._stderr.popleft())

    def _run_dispatch(self) -> None:
        while True:
            fn = self._dispatch.get()
            if fn is None:
                return
            try:
                fn()
            except Exception as e:  # a handler's failure must not stop delivery
                self._note(f"an abhed_sdk handler raised: {e!r}")

    def _note(self, text: str) -> None:
        if self._on_error is not None:
            try:
                self._on_error(text)
            except Exception:
                pass

    def _route(self, msg: Dict[str, Any]) -> None:
        rid = msg.get("id")
        if isinstance(rid, str) and rid:
            with self._lock:
                p = self._pending.pop(rid, None)
            if p is not None:
                p.value = msg
                p.done.set()
            return
        with self._lock:
            sess = self._session
        kind = msg.get("type")
        if kind == "event" and isinstance(msg.get("event"), dict):
            ev = Event.from_json(msg["event"])
            if sess is not None:
                sess._event(ev)
        elif kind == "approval" and isinstance(msg.get("approval"), dict):
            req = ApprovalRequest.from_json(msg["approval"])
            if sess is not None:
                sess._approval(req)
            else:
                self._answer(req.request_id, Decision.deny())
        elif kind == "woken":
            w = Woken(str(msg.get("answer", "") or ""), str(msg.get("error", "") or ""))
            if sess is not None:
                sess._woken(w)
        elif kind == "error":
            text = str(msg.get("error", ""))
            self._dispatch.put(lambda: self._note(text))

    def _answer(self, request_id: str, d: Decision) -> None:
        req: Dict[str, Any] = {"method": "approve", "request_id": request_id,
                               "decision": "allow" if d.allow else "deny"}
        if d.remember:
            req["remember"] = True
        if d.approver:
            req["approver"] = d.approver
        self._send(req)


class Session:
    """A session opened by :meth:`Abhed.start`. Every prompt continues it."""

    workspace_trust: WorkspaceTrust

    def __init__(self, client: Abhed, on_event: Optional[EventHandler], on_approval: Optional[ApprovalHandler],
                 on_woken: Optional[Callable[[Woken], None]]) -> None:
        self._c = client
        self._on_event = on_event
        self._on_approval = on_approval
        self._on_woken = on_woken
        self._superseded = False
        self._lock = threading.Lock()
        self._streams: List["queue.Queue[Any]"] = []
        self._events: List[Event] = []
        self.workspace_trust = WorkspaceTrust.from_json(None)

    # -- the surface -------------------------------------------------------

    def prompt(self, text: str) -> Answer:
        """Send a prompt and wait for the final answer.

        Raises :class:`RunError` when the run ended otherwise than
        completed; under ``verify="strict"`` an unverified answer is
        ``RunError.reason == "unverified"``.
        """
        r = self._send({"method": "prompt", "prompt": text})
        return Answer(str(r.get("answer", "") or ""), Verdict.from_json(r.get("verdict")))

    def stream(self, text: str) -> Iterator[StreamItem]:
        """Send a prompt and yield its events as they are recorded.

        Yields :class:`Event` values, an :class:`ApprovalRequest` for each ask
        when the session takes approvals with no handler (answer it with
        :meth:`approve`), and last the :class:`Answer`. A failed run raises
        :class:`RunError` after its events.
        """
        q: "queue.Queue[Any]" = queue.Queue()
        with self._lock:
            self._streams.append(q)
        done = _Pending()

        def run() -> None:
            try:
                done.value = self._send({"method": "prompt", "prompt": text})
            except BaseException as e:
                done.value = e
            # Events are delivered before the reply; the marker follows them.
            self._c._dispatch.put(lambda: q.put(done))

        threading.Thread(target=run, name="abhed-rpc-prompt", daemon=True).start()
        try:
            while True:
                item = q.get()
                if item is done:
                    v = done.value
                    if isinstance(v, BaseException):
                        raise v
                    assert isinstance(v, dict)
                    yield Answer(str(v.get("answer", "") or ""), Verdict.from_json(v.get("verdict")))
                    return
                yield item
        finally:
            with self._lock:
                self._streams.remove(q)

    def steer(self, text: str) -> SteerResult:
        """Redirect the running prompt: ``steered`` when it will read the text
        before answering, ``queued`` when the next prompt will."""
        r = self._send({"method": "steer", "prompt": text})
        return "steered" if r.get("type") == "steered" else "queued"

    def approve(self, request_id: str, decision: Decision) -> None:
        """Answer an approval yielded by :meth:`stream`."""
        self._c._answer(request_id, decision)

    def usage(self) -> Usage:
        return Usage.from_json(self._send({"method": "usage"}).get("usage") or {})

    def export_html(self) -> str:
        """The session as a self-contained HTML transcript."""
        return str(self._send({"method": "export"}).get("answer", ""))

    def tasks(self) -> List[TaskInfo]:
        return [TaskInfo.from_json(t) for t in self._send({"method": "tasks"}).get("tasks") or [] if isinstance(t, dict)]

    def cancel_task(self, task_id: str) -> None:
        self._send({"method": "cancel_task", "task_id": task_id})

    def wake(self) -> Answer:
        """Run the agent on background results waiting for it."""
        r = self._send({"method": "wake"})
        return Answer(str(r.get("answer", "") or ""))

    def events(self) -> List[Event]:
        """Every event this session has delivered so far."""
        with self._lock:
            return list(self._events)

    @property
    def session_id(self) -> str:
        with self._lock:
            for ev in self._events:
                if ev.session_id:
                    return ev.session_id
        return ""

    # -- from the client ---------------------------------------------------

    def _send(self, req: Dict[str, Any]) -> Dict[str, Any]:
        if self._superseded:
            raise AbhedError("this session was replaced by a later start")
        return self._c._send(req)

    def _event(self, ev: Event) -> None:
        def deliver() -> None:
            with self._lock:
                self._events.append(ev)
                streams = list(self._streams)
            for q in streams:
                q.put(ev)
            if self._on_event is not None:
                self._on_event(ev)

        self._c._dispatch.put(deliver)

    def _approval(self, req: ApprovalRequest) -> None:
        handler = self._on_approval
        if handler is not None:
            def ask() -> None:
                try:
                    d = handler(req)
                    if not isinstance(d, Decision):
                        d = Decision.deny()
                except Exception as e:
                    self._c._note(f"on_approval raised, so the call is refused: {e!r}")
                    d = Decision.deny()
                try:
                    self._c._answer(req.request_id, d)
                except AbhedError as e:
                    self._c._note(f"the approval could not be answered: {e}")

            threading.Thread(target=ask, name="abhed-rpc-approval", daemon=True).start()
            return

        def offer() -> None:
            with self._lock:
                streams = list(self._streams)
            if not streams:
                # Nobody can answer it: refused rather than left waiting.
                self._c._note(f"approval {req.request_id} for {req.tool} had no handler and no stream; refused")
                threading.Thread(target=lambda: self._c._answer(req.request_id, Decision.deny()), daemon=True).start()
                return
            for q in streams:
                q.put(req)

        self._c._dispatch.put(offer)

    def _woken(self, w: Woken) -> None:
        if self._on_woken is not None:
            fn = self._on_woken
            self._c._dispatch.put(lambda: fn(w))

