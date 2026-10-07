#!/usr/bin/env python3
"""Print the tests in the given Go test files gated on the fence, directly or
through a helper, so the fence job can require each one. --self-test checks this."""
import re
import sys

FUNC = re.compile(r"^func (?:\([^)]*\) )?(\w+)\(", re.M)
GATE = re.compile(r'ABHED_REQUIRE_FENCE|Pending\(\w+, "fence\w*"')


def gated(sources):
    """Returns the sorted Test names gated directly or through a helper."""
    bodies = {}
    for text in sources:
        for m in FUNC.finditer(text):
            # gofmt closes a top-level function with a brace in column one.
            end = text.find("\n}", m.end())
            bodies.setdefault(m.group(1), []).append(text[m.end():end if end >= 0 else len(text)])
    body = {name: "\n".join(parts) for name, parts in bodies.items()}
    # A constant that names the variable gates whatever reads it.
    alias = {m.group(1) for t in sources for m in re.finditer(r'(\w+)\s*=\s*"ABHED_REQUIRE_FENCE"', t)}
    gate = re.compile(GATE.pattern + "".join(r"|\b" + re.escape(a) + r"\b" for a in sorted(alias)))
    marked = {name for name, b in body.items() if gate.search(b)}
    # A caller of a gated helper is gated too, however deep the chain.
    while True:
        more = {name for name, b in body.items() if name not in marked
                and any(re.search(r"\b" + re.escape(g) + r"\(", b) for g in marked)}
        if not more:
            break
        marked |= more
    return sorted(n for n in marked if re.match(r"Test[A-Z_]", n) and n != "TestMain")


def self_test():
    src = '''package p

const requireEnv = "ABHED_REQUIRE_FENCE"

func helper(t *testing.T) {
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		t.Skip()
	}
}

func deeper(t *testing.T) {
	helper(t)
}

func TestDirect(t *testing.T) {
	_ = os.Getenv("ABHED_REQUIRE_FENCE")
}

func TestThroughHelper(t *testing.T) {
	deeper(t)
}

func TestPending(t *testing.T) {
	Pending(t, "fencenet", "x")
}

func TestOtherPending(t *testing.T) {
	Pending(t, "editor", "x")
}

func TestPlain(t *testing.T) {
	nothelper(t)
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

func viaConst(t *testing.T) {
	_ = os.Getenv(requireEnv)
}

func TestViaConst(t *testing.T) {
	viaConst(t)
}
'''
    want = ["TestDirect", "TestPending", "TestThroughHelper", "TestViaConst"]
    got = gated([src])
    ok = got == want
    print("self-test passed" if ok else f"self-test FAILED: got {got}, want {want}")
    return 0 if ok else 1


if __name__ == "__main__":
    if sys.argv[1:] == ["--self-test"]:
        sys.exit(self_test())
    texts = []
    for name in sys.argv[1:]:
        with open(name, encoding="utf-8") as f:
            texts.append(f.read())
    print("\n".join(gated(texts)))
