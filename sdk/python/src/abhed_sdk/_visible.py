"""Marking characters that would not print as themselves.

A tool call's arguments are the model's, unescaped: a value can hold a
right-to-left override, a terminal escape or a zero-width character that
makes it read as something else. Pass anything shown to a person through
:func:`visible` first. It marks more than the engine's own check does (a
joiner inside an emoji, say), never less.
"""

from __future__ import annotations

import json
import unicodedata
from typing import Any

_HIDDEN_CATEGORIES = {"Cc", "Cf", "Co", "Cs", "Cn", "Zl", "Zp"}


def _hidden(ch: str, keep_newline: bool) -> bool:
    if ch == "\t" or (keep_newline and ch == "\n"):
        return False
    if ch == " ":
        return False
    cat = unicodedata.category(ch)
    return cat in _HIDDEN_CATEGORIES or (cat == "Zs" and ch != " ")


def visible(text: str, *, one_line: bool = False) -> str:
    """``text`` with each hidden character shown as ``\\u{XXXX}``.

    With ``one_line``, a newline is marked too, so a value cannot start a
    line that looks like part of the prompt around it.
    """
    out = []
    for ch in text:
        if _hidden(ch, not one_line):
            out.append("\\u{%04X}" % ord(ch))
        else:
            out.append(ch)
    return "".join(out)


def args_hidden(args: Any, _depth: int = 0) -> bool:
    """Whether any key or string in a call's arguments, at any depth, holds a
    character :func:`visible` would mark (a newline or tab alone does not)."""
    if isinstance(args, str):
        if any(_hidden(c, True) for c in args):
            return True
        t = args.strip()
        if _depth < 4 and t[:1] in ("{", "["):
            try:
                return args_hidden(json.loads(t), _depth + 1)
            except ValueError:
                return False
        return False
    if isinstance(args, list):
        return any(args_hidden(v, _depth) for v in args)
    if isinstance(args, dict):
        return any(args_hidden(k, _depth) or args_hidden(v, _depth) for k, v in args.items())
    return False
