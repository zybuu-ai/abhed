"""A Python client for Abhed, the secure harness for deep agents.

It drives ``abhed rpc``, a child process speaking line-delimited JSON on its
standard input and output. Policy, the sandbox, the record and the answer
checks are the engine's: this package adds no way around them.

    from abhed_sdk import Abhed, Decision

    with Abhed(cwd="/srv/work") as ab:
        s = ab.start(mode="default", verify="strict",
                     on_approval=lambda req: Decision.deny())
        answer = s.prompt("fix the failing tests")
        print(answer.text, answer.verdict.outcome)
"""

from ._client import Abhed, ApprovalHandler, EventHandler, Session, StreamItem
from ._types import (
    AbhedError,
    Answer,
    ApprovalRequest,
    ClaimVerdict,
    Decision,
    Event,
    ProcessExited,
    Runbook,
    RunError,
    TaskInfo,
    Usage,
    Verdict,
    Woken,
    WorkspaceTrust,
)
from ._visible import args_hidden, has_hidden, visible

__all__ = [
    "Abhed",
    "AbhedError",
    "Answer",
    "ApprovalHandler",
    "ApprovalRequest",
    "ClaimVerdict",
    "Decision",
    "Event",
    "EventHandler",
    "ProcessExited",
    "Runbook",
    "RunError",
    "Session",
    "StreamItem",
    "TaskInfo",
    "Usage",
    "Verdict",
    "Woken",
    "WorkspaceTrust",
    "args_hidden",
    "has_hidden",
    "visible",
]

__version__ = "1.3.0.dev0"
