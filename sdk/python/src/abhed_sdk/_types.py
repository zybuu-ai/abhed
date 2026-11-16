"""The values abhed rpc sends, as typed Python objects.

Each keeps the line it came from in ``raw``, so a field added to the
protocol later is readable before this package names it.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Any, Dict, List, Literal, Mapping, Optional

VerifyMode = Literal["off", "label", "strict"]
WakeMode = Literal["off", "notify", "auto"]
Mode = Literal["default", "plan", "accept-edits", "auto", "bypass"]
SteerResult = Literal["steered", "queued"]


def _list(v: Any) -> List[Any]:
    return list(v) if isinstance(v, list) else []


@dataclass(frozen=True)
class Event:
    """One recorded event, as the session's record holds it."""

    id: str
    session_id: str
    seq: int
    type: str
    payload: Dict[str, Any]
    actor: str
    trust: str
    created_at: str
    parent_id: str = ""
    raw: Mapping[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> "Event":
        payload = d.get("payload")
        return cls(
            id=str(d.get("id", "")),
            session_id=str(d.get("session_id", "")),
            seq=int(d.get("seq", 0) or 0),
            type=str(d.get("type", "")),
            payload=payload if isinstance(payload, dict) else {},
            actor=str(d.get("actor", "")),
            trust=str(d.get("trust", "")),
            created_at=str(d.get("created_at", "")),
            parent_id=str(d.get("parent_id", "") or ""),
            raw=dict(d),
        )


@dataclass(frozen=True)
class ClaimVerdict:
    """What the checks found for one statement of an answer."""

    n: int
    text: str
    kind: str
    verdict: str
    reason: str
    load_bearing: bool = False
    evidence: List[int] = field(default_factory=list)
    cited: List[str] = field(default_factory=list)

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> "ClaimVerdict":
        return cls(
            n=int(d.get("n", 0) or 0),
            text=str(d.get("text", "")),
            kind=str(d.get("kind", "")),
            verdict=str(d.get("verdict", "")),
            reason=str(d.get("reason", "")),
            load_bearing=bool(d.get("load_bearing", False)),
            evidence=[int(x) for x in _list(d.get("evidence"))],
            cited=[str(x) for x in _list(d.get("cited"))],
        )


@dataclass(frozen=True)
class Verdict:
    """The check of an answer against what the session observed.

    ``outcome`` is ``verified`` or ``unverified``. ``verified`` means nothing
    about the work was left unproven, not that every statement was checked:
    read ``note`` too.
    """

    outcome: str
    mode: str
    source: str
    verified: int
    unverified: int
    contradicted: int
    claims: List[ClaimVerdict]
    note: str = ""
    turn: int = 0
    raw: Mapping[str, Any] = field(default_factory=dict, repr=False)

    @property
    def ok(self) -> bool:
        return self.outcome == "verified"

    @classmethod
    def from_json(cls, d: Optional[Mapping[str, Any]]) -> Optional["Verdict"]:
        if not isinstance(d, Mapping):
            return None
        return cls(
            outcome=str(d.get("outcome", "")),
            mode=str(d.get("mode", "")),
            source=str(d.get("source", "")),
            verified=int(d.get("verified", 0) or 0),
            unverified=int(d.get("unverified", 0) or 0),
            contradicted=int(d.get("contradicted", 0) or 0),
            claims=[ClaimVerdict.from_json(c) for c in _list(d.get("claims")) if isinstance(c, Mapping)],
            note=str(d.get("note", "") or ""),
            turn=int(d.get("turn", 0) or 0),
            raw=dict(d),
        )


@dataclass(frozen=True)
class Answer:
    """A prompt's final answer, with its verdict when answers are checked."""

    text: str
    verdict: Optional[Verdict] = None


@dataclass(frozen=True)
class Usage:
    """Tokens, turns and cost so far in the session."""

    input_tokens: int
    output_tokens: int
    cached_tokens: int
    turns: int
    compactions: int
    cost_usd: float
    unpriced: bool
    raw: Mapping[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> "Usage":
        return cls(
            input_tokens=int(d.get("InputTokens", 0) or 0),
            output_tokens=int(d.get("OutputTokens", 0) or 0),
            cached_tokens=int(d.get("CachedTokens", 0) or 0),
            turns=int(d.get("Turns", 0) or 0),
            compactions=int(d.get("Compactions", 0) or 0),
            cost_usd=float(d.get("CostUSD", 0) or 0),
            unpriced=bool(d.get("Unpriced", False)),
            raw=dict(d),
        )


@dataclass(frozen=True)
class TaskInfo:
    """One background task: a subagent (kind ``task``) or a shell (``shell``)."""

    task_id: str
    kind: str
    description: str
    status: str
    reason: str = ""
    summary: str = ""
    raw: Mapping[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> "TaskInfo":
        return cls(
            task_id=str(d.get("task_id", "")),
            kind=str(d.get("kind", "")),
            description=str(d.get("description", "")),
            status=str(d.get("status", "")),
            reason=str(d.get("reason", "") or ""),
            summary=str(d.get("summary", "") or ""),
            raw=dict(d),
        )


@dataclass(frozen=True)
class WorkspaceTrust:
    """Whether the workspace's ``.abhed/config.json`` applied whole."""

    workspace: str
    trusted: bool
    reason: str
    sha256: str = ""
    raw: Mapping[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_json(cls, d: Optional[Mapping[str, Any]]) -> "WorkspaceTrust":
        d = d if isinstance(d, Mapping) else {}
        return cls(
            workspace=str(d.get("workspace", "")),
            trusted=bool(d.get("trusted", False)),
            reason=str(d.get("reason", "")),
            sha256=str(d.get("sha256", "") or ""),
            raw=dict(d),
        )


@dataclass(frozen=True)
class ApprovalRequest:
    """A call that needs approval, put to the caller.

    ``args``, ``reason`` and ``offer`` are the copy the record holds. They
    are the model's, unescaped: pass anything shown to a person through
    :func:`abhed_sdk.visible`, and consider refusing outright when
    ``args_hidden`` is true.
    """

    request_id: str
    tool: str
    args: Any
    reason: str = ""
    step: str = ""
    rule: str = ""
    source: str = ""
    offer: str = ""
    call_id: str = ""
    destructive: bool = False
    args_hidden: bool = False
    subagent: str = ""
    task_id: str = ""
    via: str = ""
    raw: Mapping[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_json(cls, d: Mapping[str, Any]) -> "ApprovalRequest":
        def s(k: str) -> str:
            return str(d.get(k, "") or "")

        return cls(
            request_id=s("request_id"), tool=s("tool"), args=d.get("args"), reason=s("reason"),
            step=s("step"), rule=s("rule"), source=s("source"), offer=s("offer"), call_id=s("call_id"),
            destructive=bool(d.get("destructive", False)), args_hidden=bool(d.get("args_hidden", False)),
            subagent=s("subagent"), task_id=s("task_id"), via=s("via"), raw=dict(d),
        )


@dataclass(frozen=True)
class Decision:
    """An answer to an :class:`ApprovalRequest`.

    ``remember`` keeps the offered scope for the rest of the session; it is
    ignored when the request offered none. ``approver`` is recorded as you
    assert it: authenticate the person before you name them.
    """

    allow: bool
    remember: bool = False
    approver: str = ""

    @classmethod
    def allow_once(cls, approver: str = "") -> "Decision":
        return cls(True, False, approver)

    @classmethod
    def allow_always(cls, approver: str = "") -> "Decision":
        return cls(True, True, approver)

    @classmethod
    def deny(cls, approver: str = "") -> "Decision":
        return cls(False, False, approver)


@dataclass(frozen=True)
class Woken:
    """The answer of a run a background result started between prompts."""

    answer: str
    error: str = ""


@dataclass(frozen=True)
class Runbook:
    """A plan's runbook, approved for the session (see the Runbooks guide)."""

    plan: str
    runbook: List[Dict[str, Any]]
    sha256: str = ""

    def to_json(self) -> Dict[str, Any]:
        out: Dict[str, Any] = {"plan": self.plan, "runbook": self.runbook}
        if self.sha256:
            out["sha256"] = self.sha256
        return out


class AbhedError(Exception):
    """Anything abhed rpc answered with an error, or a broken connection."""


class ProcessExited(AbhedError):
    """abhed rpc ended, or could not start, before answering."""

    def __init__(self, message: str, returncode: Optional[int] = None, stderr: str = "") -> None:
        super().__init__(message)
        self.returncode = returncode
        self.stderr = stderr


_ENDED = re.compile(r"^abhed: ended as ([a-z_]+)(?:: (.*))?$", re.S)


class RunError(AbhedError):
    """A request abhed rpc answered with an error.

    For a prompt that ended otherwise than completed, ``reason`` is the
    terminal reason (``unverified``, ``max_turns``, ``prompt_refused``, ...)
    and ``detail`` what followed it; ``answer`` and ``verdict`` are what the
    run produced, which may be set even though it failed.
    """

    def __init__(self, message: str, answer: str = "", verdict: Optional[Verdict] = None) -> None:
        super().__init__(message)
        self.message = message
        self.answer = answer
        self.verdict = verdict
        m = _ENDED.match(message.split("\n", 1)[0])
        self.reason: str = m.group(1) if m else ""
        self.detail: str = (m.group(2) or "") if m else ""

    @property
    def unverified(self) -> bool:
        return self.reason == "unverified"
