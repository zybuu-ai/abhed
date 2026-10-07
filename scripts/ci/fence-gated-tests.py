#!/usr/bin/env python3
"""Print the tests in the given Go files gated on the fence, directly or
through a helper, so the fence job can require each one. --self-test checks this."""
import re
import sys

# A top-level function or method, generic or not, or a function held in a var.
DECL = re.compile(r"^(?:func (?:\([^)]*\) )?(\w+)(?:\[[^\]]*\])?\(|var (\w+)\s*=\s*func\b)", re.M)
GATE = r'ABHED_REQUIRE_FENCE|Pending\(\w+, "fence\w*"'
ALIAS = re.compile(r'(\w+)(?:\s+\w+)?\s*:?=\s*"ABHED_REQUIRE_FENCE"')
TEST = re.compile(r"Test(?![a-z])\w*$")


def skip_literal(text, i):
    """Returns the index just past the string, rune or comment at text[i]."""
    c = text[i]
    if c == "`":
        j = text.find("`", i + 1)
    elif c in "\"'":
        j = i + 1
        while j < len(text) and text[j] != c:
            j += 2 if text[j] == "\\" else 1
    elif text.startswith("//", i):
        j = text.find("\n", i)
    else:
        j = text.find("*/", i + 2) + 1
    return len(text) if j <= 0 else j + 1


def body_end(text, i):
    """Returns where the declaration starting at i ends: the brace closing its
    body, past literals, comments and type literals such as interface{}."""
    depth = 0
    while i < len(text):
        c = text[i]
        if c in "`\"'" or text.startswith("//", i) or text.startswith("/*", i):
            i = skip_literal(text, i)
            continue
        if c == "{":
            depth += 1
        elif c == "}":
            depth -= 1
            # A body ends its line; a type literal in the signature does not.
            if depth == 0 and (i + 1 >= len(text) or text[i + 1] == "\n"):
                return i + 1
        i += 1
    return len(text)


def code_only(text):
    """Returns text with strings, runes and comments blanked, so a word in one
    is not read as a reference."""
    out, i = [], 0
    while i < len(text):
        if text[i] in "`\"'" or text.startswith("//", i) or text.startswith("/*", i):
            j = skip_literal(text, i)
            out.append(" " * (j - i))
            i = j
        else:
            out.append(text[i])
            i += 1
    return "".join(out)


def gated(sources):
    """sources maps a file name to its text. Returns the sorted tests, from
    _test.go files, gated directly or through anything they name."""
    body, tests, plain = {}, set(), set()
    for name, text in sources.items():
        for m in DECL.finditer(text):
            fn = m.group(1) or m.group(2)
            if fn == "init":
                continue  # never named, so nothing reaches the gate through it
            body[fn] = body.get(fn, "") + "\n" + text[m.end():body_end(text, m.end())]
            if not name.endswith("_test.go"):
                plain.add(fn)
            elif m.group(1) and TEST.match(fn) and fn != "TestMain":
                tests.add(fn)
    # A constant that names the variable gates whatever reads it.
    alias = {m.group(1) for t in sources.values() for m in ALIAS.finditer(t)}
    gate = re.compile(GATE + "".join(r"|\b" + re.escape(a) + r"\b" for a in sorted(alias)))
    marked = {fn for fn, b in body.items() if gate.search(b)}
    code = {fn: code_only(b) for fn, b in body.items()}
    # Naming a gated function, by a call or as a value, gates the namer too;
    # non-test code cannot see a test file's functions.
    while True:
        more = {fn for fn, b in code.items() if fn not in marked
                and any(re.search(r"\b" + re.escape(g) + r"\b", b) for g in marked
                        if fn not in plain or g in plain)}
        if not more:
            break
        marked |= more
    return sorted(tests & marked)


SELF_TEST_HELPERS = '''package p

const requireEnv string = "ABHED_REQUIRE_FENCE"

func helper(t *testing.T) {
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		t.Skip()
	}
}

func viaConst(t *testing.T) {
	_ = os.Getenv(requireEnv)
}

func gen[T any](t *testing.T, v T) {
	helper(t)
}

var held = func(t *testing.T) {
	helper(t)
}

type suite struct{}

func (s suite) gate(t *testing.T) {
	helper(t)
}

func run(t *testing.T, f func(*testing.T)) {
	f(t)
}

func plainCode() {
	deeper := true // a local that shares a test helper's name
	_ = deeper
}

func init() {
	helper(nil)
}
'''

SELF_TEST_TESTS = '''package p

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

func TestViaTypedConst(t *testing.T) {
	viaConst(t)
}

func TestGeneric(t *testing.T) {
	gen[int](t, 1)
}

func TestVarFunc(t *testing.T) {
	held(t)
}

func TestAfterARawString(t *testing.T) {
	s := `
}
`
	_ = s
	helper(t)
}

func TestFuncValue(t *testing.T) {
	var s suite
	run(t, s.gate)
}

func Test2X(t *testing.T) {
	helper(t)
}

func TestWithInterfaceResult(t *testing.T) interface{} {
	helper(t)
	return nil
}

func TestOtherPending(t *testing.T) {
	Pending(t, "editor", "x")
}

func TestPlain(t *testing.T) {
	nothelper(t, "helper") // helper() named only in a string and a comment
	plainCode()
}

func Testlower(t *testing.T) {
	helper(t)
}

func TestMain(m *testing.M) {
	helper(nil)
}

func init() {
	helper(nil)
}

func TestLocalNamedInit(t *testing.T) {
	init := 0 // a local; the package's init cannot be named
	_ = init
}
'''


def self_test():
    want = ["Test2X", "TestAfterARawString", "TestDirect", "TestFuncValue", "TestGeneric", "TestPending",
            "TestThroughHelper", "TestVarFunc", "TestViaTypedConst", "TestWithInterfaceResult"]
    got = gated({"h.go": SELF_TEST_HELPERS, "x_test.go": SELF_TEST_TESTS})
    ok = got == want
    print("self-test passed" if ok else f"self-test FAILED:\n got  {got}\n want {want}")
    return 0 if ok else 1


if __name__ == "__main__":
    if sys.argv[1:] == ["--self-test"]:
        sys.exit(self_test())
    files = {}
    for name in sys.argv[1:]:
        with open(name, encoding="utf-8") as f:
            files[name] = f.read()
    print("\n".join(gated(files)))
