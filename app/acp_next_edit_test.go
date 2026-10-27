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
