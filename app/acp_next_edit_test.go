package app

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

type nextEdits struct {
	Edits []struct {
		URI   string    `json:"uri"`
		Range editRange `json:"range"`
		Text  string    `json:"newText"`
	} `json:"edits"`
	Disabled string `json:"disabled"`
	Refused  string `json:"refused"`
	Dropped  string `json:"dropped"`
}

// §7.11: the window and the recent edit go to the model as a chat turn, and
// its rewrite comes back as edits by line, inside the document.
func TestStudioNextEdit(t *testing.T) {
	window := "func area(w, h int) int {\n\tsize := w * h\n\treturn size\n}\n"
	model := newFIMServer(t, "func area(w, h int) int {\n\tarea := w * h\n\treturn area\n}\n")
	r := completionRig(t, model.URL, "")
	noHold(t)
	id := r.open()
	uri := "file://" + r.ws + "/area.go"
	var res nextEdits
	r.cl.ok("_abhed/nextEdit", map[string]any{"sessionId": id, "uri": uri, "language": "go",
		"recentEdits": []map[string]any{{"uri": uri, "range": map[string]any{"start": map[string]int{"line": 11}, "end": map[string]int{"line": 11}}, "oldText": "size", "newText": "area"}},
		"cursor":      map[string]int{"line": 11, "character": 2}, "window": map[string]any{"startLine": 10, "text": window}}, &res)
	if len(res.Edits) != 1 || res.Edits[0].Range.Start.Line != 11 || res.Edits[0].Range.End.Line != 13 ||
		res.Edits[0].Text != "\tarea := w * h\n\treturn area\n" || res.Edits[0].URI != uri {
		t.Fatalf("%+v", res)
	}
	model.mu.Lock()
	paths := model.paths
	model.mu.Unlock()
	if len(paths) != 1 || paths[0] != "/v1/chat/completions" {
		t.Fatalf("paths %v", paths)
	}
	r.cl.call("session/close", map[string]any{"sessionId": id})
	enabled, _ := r.recorded(id, agent.EvNextEditEnabled)
	sum, _ := r.recorded(id, agent.EvNextEditSummary)
	if len(enabled) != 1 || len(sum) != 1 || sum[0]["served"] != float64(1) {
		t.Fatalf("enabled %v summary %v", enabled, sum)
	}
}

// Prose, a wholesale rewrite, and a change to a line holding a secret are
// not offered; Abhed's state and a file outside are refused; no model is off.
func TestStudioNextEditRefusals(t *testing.T) {
	window := "a := 1\nb := 2\nAPI_TOKEN=\"Zq81kLm0Pw7xYb2cXYZ\"\nc := 3\nd := 4\ne := 5\n"
	for _, tc := range []struct{ reply, want string }{
		{"Sure! Here is the **updated** code with the change applied.", "prose"},
		{"x\ny\nz\nw\nv\nu\n", "most of the window"},
		{"a := 1\nb := 2\nAPI_TOKEN=\"[secret]2\"\nc := 3\nd := 4\ne := 5\n", "secret"},
	} {
		model := newFIMServer(t, tc.reply)
		r := completionRig(t, model.URL, "")
		noHold(t)
		id := r.open()
		var res nextEdits
		r.cl.ok("_abhed/nextEdit", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go",
			"cursor": map[string]int{"line": 0}, "window": map[string]any{"startLine": 0, "text": window}}, &res)
		if len(res.Edits) != 0 || !strings.Contains(res.Dropped, tc.want) {
			t.Fatalf("%q: %+v", tc.reply, res)
		}
	}
	model := newFIMServer(t, "x")
	r := completionRig(t, model.URL, "")
	noHold(t)
	id := r.open()
	for _, uri := range []string{"file://" + r.ws + "/.abhed/x.go", "file:///etc/hosts"} {
		var res nextEdits
		r.cl.ok("_abhed/nextEdit", map[string]any{"sessionId": id, "uri": uri, "window": map[string]any{"text": "x\n"}}, &res)
		if res.Refused == "" {
			t.Fatalf("%s: %+v", uri, res)
		}
	}
	off := completionRig(t, model.URL, `,"completion":{"enabled":false}`)
	oid := off.open()
	var res nextEdits
	off.cl.ok("_abhed/nextEdit", map[string]any{"sessionId": oid, "uri": "file://" + off.ws + "/a.go", "window": map[string]any{"text": "x\n"}}, &res)
	if !strings.Contains(res.Disabled, "next-edit suggestions are turned off") {
		t.Fatalf("%+v", res)
	}
}

// A change at the end of a window with no newline after it ends its range
// at the end of the last line, not one line past it.
func TestStudioNextEditAtTheEndOfTheDocument(t *testing.T) {
	model := newFIMServer(t, "a := 1\nb := 3")
	r := completionRig(t, model.URL, "")
	noHold(t)
	id := r.open()
	var res nextEdits
	r.cl.ok("_abhed/nextEdit", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go",
		"cursor": map[string]int{"line": 4}, "window": map[string]any{"startLine": 3, "text": "a := 1\nb := 2"}}, &res)
	if len(res.Edits) != 1 || res.Edits[0].Range.Start.Line != 4 || res.Edits[0].Range.End.Line != 4 ||
		res.Edits[0].Range.End.Character != 6 || res.Edits[0].Text != "b := 3" {
		t.Fatalf("%+v", res)
	}
}

// A suggestion that only changes whitespace at a line's end, as a cursor
// marker left behind does, is no edit: nothing is offered.
func TestStudioNextEditDropsNoOps(t *testing.T) {
	model := newFIMServer(t, "a := 1  \nb := 2\t\n")
	r := completionRig(t, model.URL, "")
	noHold(t)
	id := r.open()
	var res nextEdits
	r.cl.ok("_abhed/nextEdit", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go",
		"cursor": map[string]int{"line": 0}, "window": map[string]any{"startLine": 0, "text": "a := 1\nb := 2\n"}}, &res)
	if len(res.Edits) != 0 || !strings.Contains(res.Dropped, "no change") {
		t.Fatalf("%+v", res)
	}
}

// The QA case: a rename's rewrite with spaces added at a line's end and the
// closing brace's newline dropped gives clean edits of the renamed lines only.
func TestStudioNextEditTakesTheWindowsLineEnds(t *testing.T) {
	window := "func sum(xs []int64) int64 {\n\tvar total int64\n\tfor _, x := range xs {\n\t\tt += x\n\t}\n\treturn t\n}\n"
	model := newFIMServer(t, "func sum(xs []int64) int64 {\n\tvar total int64  \n\tfor _, x := range xs {\n\t\ttotal += x\n\t}\n\treturn total\n}")
	r := completionRig(t, model.URL, "")
	noHold(t)
	id := r.open()
	var res nextEdits
	r.cl.ok("_abhed/nextEdit", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/s.go",
		"cursor": map[string]int{"line": 1}, "window": map[string]any{"startLine": 0, "text": window}}, &res)
	if len(res.Edits) != 2 || res.Edits[0].Text != "\t\ttotal += x\n" || res.Edits[1].Text != "\treturn total\n" {
		t.Fatalf("%+v", res)
	}
}

// Whitespace the person wrote at a line's end stays (a Markdown hard break),
// and a CRLF window takes an LF rewrite as CRLF.
func TestStudioNextEditKeepsThePersonsWhitespace(t *testing.T) {
	for _, tc := range []struct{ window, reply, want string }{
		{"line one  \nline two\nline three\n", "line one\nline 2\nline three\n", "line 2\n"},
		{"a := 1\r\nb := 2\r\nc := 3\r\n", "a := 1\nb := 20\nc := 3\n", "b := 20\r\n"},
	} {
		model := newFIMServer(t, tc.reply)
		r := completionRig(t, model.URL, "")
		noHold(t)
		id := r.open()
		var res nextEdits
		r.cl.ok("_abhed/nextEdit", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.md",
			"cursor": map[string]int{"line": 1}, "window": map[string]any{"startLine": 0, "text": tc.window}}, &res)
		if len(res.Edits) != 1 || res.Edits[0].Text != tc.want || res.Edits[0].Range.Start.Line != 1 {
			t.Fatalf("%q: %+v", tc.window, res)
		}
	}
}

// A whitespace-only line's spaces are not given to an empty line, and a
// block scalar in the window is withheld whole, so its line is not edited.
func TestStudioNextEditBlanksAndBlocks(t *testing.T) {
	model := newFIMServer(t, "func a() {\n    \n}\n\nx := 2\n")
	r := completionRig(t, model.URL, "")
	noHold(t)
	id := r.open()
	var res nextEdits
	r.cl.ok("_abhed/nextEdit", map[string]any{"sessionId": id, "uri": "file://" + r.ws + "/a.go",
		"cursor": map[string]int{"line": 4}, "window": map[string]any{"startLine": 0, "text": "func a() {\n    \n}\n\nx := 1\n"}}, &res)
	if len(res.Edits) != 1 || res.Edits[0].Text != "x := 2\n" || res.Edits[0].Range.Start.Line != 4 {
		t.Fatalf("%+v", res)
	}

	block := newFIMServer(t, "db:\n  password: |\n    s3cretvalue9\n  user: alice\n")
	r2 := completionRig(t, block.URL, "")
	noHold(t)
	id2 := r2.open()
	r2.cl.ok("_abhed/nextEdit", map[string]any{"sessionId": id2, "uri": "file://" + r2.ws + "/v.yaml",
		"cursor": map[string]int{"line": 3}, "window": map[string]any{"startLine": 0, "text": "db:\n  password: |\n    s3cretvalue9\n  user: bob\n"}}, &res)
	block.mu.Lock()
	sent := strings.Join(block.bodies, "\n")
	block.mu.Unlock()
	if strings.Contains(sent, "s3cretvalue9") {
		t.Fatal("the block scalar's value reached the model")
	}
}

// chunkText is the file the parity review renamed step to stride in, at
// line 8 (index 7); the uses of step on later lines are what is left.
const chunkText = `def chunk_text(text, size, overlap=0):
    """Split text into chunks of at most size characters, each starting
    a step after the previous one."""
    if size <= 0:
        raise ValueError("size must be positive")
    if overlap >= size:
        raise ValueError("overlap must be smaller than size")
    stride = size - overlap
    chunks = []
    for start in range(0, len(text) - size, step):  # each step
        chunks.append(text[start:start + size])
    print("step", step)
    return chunks


def other(step):
    return step * 2
`

// A rename is finished from the file: the other uses of the old name in its
// definition become one edit, with no model asked; uses in strings,
// comments, docstrings and another definition are left as they are.
func TestStudioNextEditRename(t *testing.T) {
	model := newFIMServer(t, "never asked")
	r := completionRig(t, model.URL, "")
	noHold(t)
	id := r.open()
	uri := "file://" + r.ws + "/text.py"
	ask := func(extra map[string]any) nextEdits {
		p := map[string]any{"sessionId": id, "uri": uri, "language": "python",
			"recentEdits": []map[string]any{{"uri": uri, "range": map[string]any{"start": map[string]int{"line": 17, "character": 4}, "end": map[string]int{"line": 17, "character": 8}}, "oldText": "step", "newText": "stride"}},
			"cursor":      map[string]int{"line": 17, "character": 10}, "window": map[string]any{"startLine": 10, "text": chunkText}}
		for k, v := range extra {
			p[k] = v
		}
		var res nextEdits
		r.cl.ok("_abhed/nextEdit", p, &res)
		return res
	}
	res := ask(nil)
	if len(res.Edits) != 1 || res.Edits[0].Range.Start.Line != 19 || res.Edits[0].Range.End.Line != 22 ||
		res.Edits[0].Text != "    for start in range(0, len(text) - size, stride):  # each step\n        chunks.append(text[start:start + size])\n    print(\"step\", stride)\n" {
		t.Fatalf("%+v", res)
	}
	// The language server's sites, when Studio sends them, are taken instead of a scan.
	res = ask(map[string]any{"sites": []map[string]any{{"start": map[string]int{"line": 21, "character": 18}, "end": map[string]int{"line": 21, "character": 22}},
		{"start": map[string]int{"line": 26, "character": 0}, "end": map[string]int{"line": 26, "character": 4}}}})
	if len(res.Edits) != 1 || res.Edits[0].Range.Start.Line != 21 || res.Edits[0].Text != "    print(\"step\", stride)\n" {
		t.Fatalf("with sites: %+v", res)
	}
	// Studio's own reading of the rename, for edits that were not one replacement.
	res = ask(map[string]any{"recentEdits": []any{}, "rename": map[string]any{"from": "step", "to": "stride", "at": map[string]int{"line": 17, "character": 4}}})
	if len(res.Edits) != 1 || res.Edits[0].Range.Start.Line != 19 {
		t.Fatalf("with a hint: %+v", res)
	}
	// A rename the file cannot finish safely (the name bound again in a nested
	// function) offers nothing, and asks no model.
	at10 := []map[string]any{{"uri": uri, "range": map[string]any{"start": map[string]int{"line": 10, "character": 4}, "end": map[string]int{"line": 10, "character": 8}}, "oldText": "step", "newText": "stride"}}
	res = ask(map[string]any{"window": map[string]any{"startLine": 9, "text": "def f(size):\n    stride = size\n    def g(step):\n        return step\n    return step\n"}, "recentEdits": at10})
	if len(res.Edits) != 0 || !strings.Contains(res.Dropped, "bound again") {
		t.Fatalf("shadowed: %+v", res)
	}
	paths := func() int { model.mu.Lock(); defer model.mu.Unlock(); return len(model.paths) }
	if n := paths(); n != 0 {
		t.Fatalf("a rename asked the model %d time(s)", n)
	}
	// No other use: a one-word edit is no rename, and the model is asked as for any edit.
	ask(map[string]any{"window": map[string]any{"startLine": 9, "text": "def f(size, overlap):\n    stride = size - overlap\n    return 1\n"}, "recentEdits": at10})
	if n := paths(); n != 1 {
		t.Fatalf("no other use: the model was asked %d time(s); want 1", n)
	}
	r.cl.call("session/close", map[string]any{"sessionId": id})
	sum, _ := r.recorded(id, agent.EvNextEditSummary)
	if len(sum) != 1 || sum[0]["local"] != float64(3) || sum[0]["local_dropped"] != float64(1) || sum[0]["requests"] != float64(1) {
		t.Fatalf("next_edit.summary %v", sum)
	}
}

// What is not a rename goes to the model as before: a new name that is a
// keyword, an edit that is not a whole identifier, or one in another file.
func TestStudioNextEditNotARename(t *testing.T) {
	for _, tc := range []struct {
		name, old, new, written string
		uri                     string
	}{
		{"a keyword", "step", "class", "class", ""},
		{"part of a word", "ste", "stride", "stridep", ""},
		{"another file", "step", "stride", "stride", "/other.py"},
	} {
		model := newFIMServer(t, chunkText)
		r := completionRig(t, model.URL, "")
		noHold(t)
		id := r.open()
		uri := "file://" + r.ws + "/text.py"
		window := strings.Replace(chunkText, "stride =", tc.written+" =", 1)
		editURI := uri
		if tc.uri != "" {
			editURI = "file://" + r.ws + tc.uri
		}
		var res nextEdits
		r.cl.ok("_abhed/nextEdit", map[string]any{"sessionId": id, "uri": uri, "language": "python",
			"recentEdits": []map[string]any{{"uri": editURI, "range": map[string]any{"start": map[string]int{"line": 7, "character": 4}, "end": map[string]int{"line": 7, "character": 8}}, "oldText": tc.old, "newText": tc.new}},
			"cursor":      map[string]int{"line": 7}, "window": map[string]any{"startLine": 0, "text": window}}, &res)
		model.mu.Lock()
		paths := len(model.paths)
		model.mu.Unlock()
		if paths != 1 {
			t.Fatalf("%s: the model was asked %d time(s); want 1", tc.name, paths)
		}
	}
}

func TestRenameScopeAndCode(t *testing.T) {
	lines := linediffSplit(chunkText)
	code := codeOnly(lines, "python")
	if from, to, ok := scopeOf(lines, code, 7, "python"); !ok || from != 0 || to != 15 {
		t.Fatalf("scope of line 8: [%d, %d) %v", from, to, ok)
	}
	if from, to, ok := scopeOf(lines, code, 15, "python"); !ok || from != 15 || to != len(lines) {
		t.Fatalf("scope of a header: [%d, %d) %v", from, to, ok)
	}
	got := codeOnly([]string{"x = 'a # b' # c\n", "s = \"\"\"one\n", "two\"\"\" + y\n"}, "python")
	if got[0] != "x ="+strings.Repeat(" ", 12)+"\n" || got[1] != "s =       \n" || got[2] != "       + y\n" {
		t.Fatalf("%q", got)
	}
	got = codeOnly([]string{"a := `x` // y\n", "/* z\n", "*/ b\n"}, "go")
	if got[0] != "a :=         \n" || got[2] != "   b\n" {
		t.Fatalf("%q", got)
	}
	for _, tc := range []struct {
		name, language string
		ok             bool
	}{
		{"class", "python", false}, {"stride", "python", true}, {"1x", "go", false}, {"größe", "go", true},
		{"nil", "go", false}, {"null", "java", false}, {"true", "java", false}, {"int", "c", false}, {"nullptr", "cpp", false},
	} {
		if isIdent(tc.name, tc.language) != tc.ok {
			t.Errorf("isIdent(%q, %s) = %v", tc.name, tc.language, !tc.ok)
		}
	}
}

func linediffSplit(s string) []string { return strings.SplitAfter(strings.TrimSuffix(s, "\n"), "\n") }

// renameCase asks for the next edit after the person renamed from to to at
// line (0-based) of text, in language; the answer, and whether a model was asked.
func renameCase(t *testing.T, language, text string, line, char int, from, to string, sites ...map[string]any) (nextEdits, bool) {
	t.Helper()
	model := newFIMServer(t, text)
	r := completionRig(t, model.URL, "")
	noHold(t)
	id := r.open()
	uri := "file://" + r.ws + "/f"
	p := map[string]any{"sessionId": id, "uri": uri, "language": language,
		"rename": map[string]any{"from": from, "to": to, "at": map[string]int{"line": line, "character": char}},
		"cursor": map[string]int{"line": line}, "window": map[string]any{"startLine": 0, "text": text}}
	if len(sites) > 0 {
		p["sites"] = sites
	}
	var res nextEdits
	r.cl.ok("_abhed/nextEdit", p, &res)
	model.mu.Lock()
	asked := len(model.paths) > 0
	model.mu.Unlock()
	return res, asked
}

// The scan leaves members, keyword arguments and object keys alone: they are
// other names that happen to be spelt the same.
func TestStudioNextEditRenameSkipsMembersAndKeywordArguments(t *testing.T) {
	py := "def f(user):\n    title = user.name\n    print(name, key=name)\n    g(name=1)\n    return name\n"
	res, asked := renameCase(t, "python", py, 1, 4, "name", "title")
	if asked || len(res.Edits) != 1 || res.Edits[0].Range.Start.Line != 2 || res.Edits[0].Range.End.Line != 5 ||
		res.Edits[0].Text != "    print(title, key=title)\n    g(name=1)\n    return title\n" {
		t.Fatalf("python: %+v", res)
	}
	js := "function f(o) {\n  const key = 1;\n  return o.id + id + {id: 2}.id;\n}\n"
	res, asked = renameCase(t, "javascript", js, 1, 8, "id", "key")
	if asked || len(res.Edits) != 1 || res.Edits[0].Text != "  return o.id + key + {id: 2}.id;\n" {
		t.Fatalf("javascript: %+v", res)
	}
}

// The scope is the innermost function: a sibling method's variable of the
// same name is not touched, and a name bound again in a nested function,
// lambda or comprehension leaves the rename to the person.
func TestStudioNextEditRenameScopes(t *testing.T) {
	sib := "class C:\n    def a(self):\n        y = 1\n        return x\n\n    def b(self):\n        x = 2\n        return x\n"
	res, _ := renameCase(t, "python", sib, 2, 8, "x", "y")
	if len(res.Edits) != 1 || res.Edits[0].Range.Start.Line != 3 || res.Edits[0].Text != "        return y\n" {
		t.Fatalf("sibling method: %+v", res)
	}
	for name, text := range map[string]string{
		"nested def":    "def f():\n    y = 1\n    def g(x):\n        return x\n    return g(x)\n",
		"comprehension": "def f(xs):\n    y = 1\n    return [x for x in xs] + [x]\n",
		"lambda":        "def f():\n    y = 1\n    h = lambda x: x\n    return h(x)\n",
	} {
		res, asked := renameCase(t, "python", text, 1, 4, "x", "y")
		if asked || len(res.Edits) != 0 || !strings.Contains(res.Dropped, "bound again") {
			t.Fatalf("%s: %+v asked=%v", name, res, asked)
		}
	}
	goShadow := "func f() int {\n\ty := 1\n\tif true {\n\t\tx := 2\n\t\t_ = x\n\t}\n\treturn x\n}\n"
	if res, _ := renameCase(t, "go", goShadow, 1, 1, "x", "y"); len(res.Edits) != 0 || !strings.Contains(res.Dropped, "bound again") {
		t.Fatalf("go inner block: %+v", res)
	}
	java := "class A {\n    int f() {\n        int total = 1;\n        return sum * 2 + this.sum;\n    }\n\n    int g() {\n        return sum;\n    }\n}\n"
	if res, _ := renameCase(t, "java", java, 2, 12, "sum", "total"); len(res.Edits) != 1 || res.Edits[0].Text != "        return total * 2 + this.sum;\n" {
		t.Fatalf("java: %+v", res)
	}
}

// Only languages whose reserved words and comments the scan knows are
// scanned; C takes a language server's sites, Ruby nothing; a reserved word
// is never a new name; a new name already in use is no rename, and the
// model is asked as before.
func TestStudioNextEditRenameLanguages(t *testing.T) {
	c := "int f(void) {\n    int y = 1;\n    return x; // x\n}\n"
	if _, asked := renameCase(t, "c", c, 1, 8, "x", "y"); !asked {
		t.Fatal("c without sites was scanned")
	}
	site := map[string]any{"start": map[string]int{"line": 2, "character": 11}, "end": map[string]int{"line": 2, "character": 12}}
	if res, asked := renameCase(t, "c", c, 1, 8, "x", "y", site); asked || len(res.Edits) != 1 || res.Edits[0].Text != "    return y; // x\n" {
		t.Fatalf("c with sites: %+v", res)
	}
	cInt := strings.Replace(c, "int y", "int int", 1)
	if _, asked := renameCase(t, "c", cInt, 1, 8, "x", "int", site); !asked {
		t.Fatal("c: x renamed to int")
	}
	javaNull := "class A {\n    void f() {\n        Object null = 1;\n        g(x);\n    }\n}\n"
	if _, asked := renameCase(t, "java", javaNull, 2, 15, "x", "null"); !asked {
		t.Fatal("java: x renamed to null")
	}
	ruby := "def f\n  y = 1\n  # x is here\n  x\nend\n"
	if _, asked := renameCase(t, "ruby", ruby, 1, 2, "x", "y"); !asked {
		t.Fatal("ruby was scanned")
	}
	used := "def f(x, y):\n    z = y\n    return x\n"
	if _, asked := renameCase(t, "python", used, 1, 8, "x", "y"); !asked {
		t.Fatal("a swap to a name already used was taken for a rename")
	}
}

// What follows a name decides whether it is the variable: a ternary's and a
// case's colon, a typed declaration and a parameter's default all are; an
// object key is not.
func TestStudioNextEditRenameNotAVariableTable(t *testing.T) {
	for _, tc := range []struct {
		name, language, text string
		line, char           int
		want                 string // the edit's text; "" for the model asked
	}{
		{"js ternary", "javascript", "function f(c, other) {\n  const y = 1;\n  return c ? x : other;\n}\n", 1, 8, "  return c ? y : other;\n"},
		{"java ternary", "java", "class A {\n    int f(boolean c) {\n        int y = 1;\n        return c ? x : 0;\n    }\n}\n", 2, 12, "        return c ? y : 0;\n"},
		{"js case", "javascript", "function f(v) {\n  const y = 1;\n  switch (v) {\n    case x: return 1;\n  }\n}\n", 1, 8, "    case y: return 1;\n"},
		{"go case", "go", "func f(v int) int {\n\ty := 1\n\tswitch v {\n\tcase x:\n\t\treturn 1\n\t}\n\treturn 0\n}\n", 1, 1, "\tcase y:\n"},
		{"ts typed use", "typescript", "function f(): number {\n  const y: number = 1;\n  const z: number = x;\n  return z;\n}\n", 1, 8, "  const z: number = y;\n"},
		{"object key kept", "javascript", "function f() {\n  const y = 1;\n  return {x: x, k: 2};\n}\n", 1, 8, "  return {x: y, k: 2};\n"},
		{"go literal key kept", "go", "func f() T {\n\ty := 1\n\treturn T{x: x}\n}\n", 1, 1, "\treturn T{x: y}\n"},
		{"python default", "python", "def f(x=1):\n    return y + x\n", 1, 11, "def f(y=1):\n    return y + y\n"},
		{"js default", "javascript", "function f(x = 1) {\n  return y + x;\n}\n", 1, 9, "function f(y = 1) {\n  return y + y;\n"},
		{"ts typed declaration elsewhere", "typescript", "function f() {\n  let x: number = 1;\n  return y;\n}\n", 2, 9, ""},
	} {
		res, asked := renameCase(t, tc.language, tc.text, tc.line, tc.char, "x", "y")
		if tc.want == "" {
			if !asked {
				t.Errorf("%s: the model was not asked: %+v", tc.name, res)
			}
			continue
		}
		if asked || len(res.Edits) != 1 || res.Edits[0].Text != tc.want {
			t.Errorf("%s: %+v (asked %v)", tc.name, res, asked)
		}
	}
}
