"""Marking characters that would not print as themselves.

A tool call's arguments are the model's, unescaped: a value can hold a
right-to-left override, a terminal escape, a zero-width character, or a run
of blanks that pushes the rest of a line out of view. :func:`visible` shows
each as a marked escape (``⟨U+202E⟩``, ``⟨\\e⟩``, ``⟨80 spaces⟩``) and drops
nothing. It is a port of the engine's own escaper (``internal/visible``),
checked against the engine's output for a shared set of cases
(``sdk/testdata/visible.json``); the one difference is that a Python string
holds no invalid UTF-8 for it to mark. Character classes come from this
Python's ``unicodedata``, so a format character newer than its Unicode
version goes unmarked where the engine marks it. The ``args_hidden`` field
of an approval is the engine's own verdict and the authoritative one.
"""

from __future__ import annotations

import json
import unicodedata
from typing import Any, List, Tuple


def _hidden(r: int) -> bool:
    if r < 0x20 or r == 0x7F or 0x80 <= r < 0xA0:
        return True
    if r in (0x2028, 0x2029):
        return True
    return unicodedata.category(chr(r)) == "Cf"


def _pictograph(r: int) -> bool:
    return (0x1F000 <= r <= 0x1FAFF or 0x2600 <= r <= 0x27BF or 0x2300 <= r <= 0x23FF or 0x2B00 <= r <= 0x2BFF
            or r in (0x00A9, 0x00AE, 0x203C, 0x2049, 0x2122, 0x2139))


def _keep_joiner(rs: List[int], i: int) -> bool:
    if i == 0 or i + 1 >= len(rs):
        return False
    left = rs[i - 1]
    j = i - 1
    while j > 0 and (left == 0xFE0F or 0x1F3FB <= left <= 0x1F3FF):
        left = rs[j - 1]
        j -= 1
    return _pictograph(left) and _pictograph(rs[i + 1])


def _emoji_base(r: int) -> bool:
    return (unicodedata.category(chr(r)) in ("So", "Sm") or 0x30 <= r <= 0x39 or r in (0x23, 0x2A)
            or r in (0x203C, 0x2049, 0x2139))


def _draws_nothing(r: int, prev: int) -> bool:
    if r in (0x034F, 0x115F, 0x1160, 0x2800, 0x3164, 0xFFA0):
        return True
    if r == 0xFE0F:
        return not _emoji_base(prev)
    return False


def _other_space(r: int) -> bool:
    return r != 0x20 and unicodedata.category(chr(r)) == "Zs"


def _plural(n: int, one: str, many: str) -> str:
    return f"1 {one}" if n == 1 else f"{n} {many}"


def _blank_run(rs: List[int], i: int) -> Tuple[int, str]:
    j, sp, tb = i, 0, 0
    while j < len(rs) and rs[j] in (0x20, 0x09):
        if rs[j] == 0x20:
            sp += 1
        else:
            tb += 1
        j += 1
    limit = 32 if i > 0 and rs[i - 1] == 0x0A else 8
    if sp + tb < 2 or sp + 8 * tb < limit:
        return j, ""
    if tb == 0:
        count = _plural(sp, "space", "spaces")
    elif sp == 0:
        count = _plural(tb, "tab", "tabs")
    else:
        count = _plural(sp, "space", "spaces") + ", " + _plural(tb, "tab", "tabs")
    return j, "⟨" + count + "⟩"


_NAMED = {0x0D: "⟨\\r⟩", 0x08: "⟨\\b⟩", 0x07: "⟨\\a⟩", 0x1B: "⟨\\e⟩", 0x00: "⟨\\0⟩"}


def visible(text: str, *, one_line: bool = False) -> str:
    """``text`` as a person should see it before approving it.

    Every character that would not print as itself is shown as a marked
    escape, and a run of eight columns of blanks or more (32 at the start of
    a line) as a count. With ``one_line``, a newline is shown too, so a value
    cannot start a line that looks like part of the prompt around it.
    """
    rs = [ord(c) for c in text]
    out: List[str] = []
    i = 0
    while i < len(rs):
        r = rs[i]
        if r in (0x20, 0x09):
            end, mark = _blank_run(rs, i)
            out.append(mark if mark else "".join(chr(c) for c in rs[i:end]).replace("\t", "    "))
            i = end
            continue
        prev = rs[i - 1] if i > 0 else 0
        if r == 0x0A:
            out.append("⟨\\n⟩" if one_line else "\n")
        elif r == 0x200D and _keep_joiner(rs, i):
            out.append(chr(r))
        elif r in _NAMED:
            out.append(_NAMED[r])
        elif _hidden(r) or r == 0x200D or _draws_nothing(r, prev) or _other_space(r):
            out.append("⟨U+%04X⟩" % r)
        else:
            out.append(chr(r))
        i += 1
    return "".join(out)


def has_hidden(text: str) -> bool:
    """Whether :func:`visible` would change ``text`` beyond widening its tabs."""
    return visible(text) != text.replace("\t", "    ")


def args_hidden(args: Any, _depth: int = 0) -> bool:
    """Whether any key or string in a call's arguments, at any depth, holds
    something :func:`visible` would mark, as the engine's ``args_hidden``
    decides it. A string that is itself JSON is read too."""
    if isinstance(args, str):
        if has_hidden(args):
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
        return any(has_hidden(k) or args_hidden(v, _depth) for k, v in args.items())
    return False
