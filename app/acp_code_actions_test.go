package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/lsprelay"
)

type codeActionsResult struct {
	Actions []struct {
		ID          string            `json:"id"`
		Kind        string            `json:"kind"`
		Title       string            `json:"title"`
		IsPreferred bool              `json:"isPreferred"`
		Prompt      []json.RawMessage `json:"prompt"`
	} `json:"actions"`
}

func rangeOf(l1, c1, l2, c2 int) map[string]any {
	return map[string]any{"start": map[string]any{"line": l1, "character": c1}, "end": map[string]any{"line": l2, "character": c2}}
}

// §7.13: the actions for a range are Explain, Fix and Add tests, each the
// prompt Studio sends; the editor's diagnostics on the range go with Fix
// and Explain as an attachment, and the one Fix runs is an ordinary prompt
// the model reads with them fenced as data.
func TestCodeActionsAreOrdinaryPrompts(t *testing.T) {
	r := newStudioRig(t, `,"permissions":{"deny":["read(**/hidden/**)"]}`)
	id := r.open()
	for name, body := range map[string]string{"pkg/calc.go": "package pkg\n\nfunc f() int {\n\treturn y\n}\n", "pkg/calc_test.go": "package pkg\n", "hidden/k.go": "package k\n"} {
		p := filepath.Join(r.ws, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	uri := lsprelay.URI(filepath.Join(r.ws, "pkg", "calc.go"))
	var res codeActionsResult
	r.cl.ok("_abhed/codeActions", map[string]any{"sessionId": id, "uri": uri, "range": rangeOf(2, 0, 4, 1),
		"diagnostics": []any{
			map[string]any{"range": rangeOf(3, 8, 3, 9), "severity": 1, "message": "undefined: y\nsecond line", "source": "compiler"},
			map[string]any{"range": rangeOf(40, 0, 40, 1), "severity": 1, "message": "elsewhere"},
		}}, &res)
	if len(res.Actions) != 3 || res.Actions[0].ID != "explain" || res.Actions[1].ID != "fix" || res.Actions[2].ID != "tests" {
		t.Fatalf("actions %+v", res.Actions)
	}
	fix := res.Actions[1]
	if !fix.IsPreferred || fix.Title != "Fix with Abhed: undefined: y second line" || fix.Kind != "quickfix.abhed" || len(fix.Prompt) != 2 {
		t.Fatalf("fix %+v", fix)
	}
	if s := string(fix.Prompt[1]); !strings.Contains(s, "pkg/calc.go:4:9: error: undefined: y second line (compiler)") || strings.Contains(s, "elsewhere") {
		t.Fatalf("fix's diagnostics: %s", s)
	}
	if s := string(fix.Prompt[0]); !strings.Contains(s, `lines 3-5 of \"pkg/calc.go\"`) {
		t.Fatalf("fix's prompt: %s", s)
	}
	if len(res.Actions[2].Prompt) != 1 || !strings.Contains(string(res.Actions[2].Prompt[0]), "Add tests for the code in lines 3-5") {
		t.Fatalf("tests: %s", res.Actions[2].Prompt)
	}

	// The action runs as the person's prompt does: the attachment is data.
	var out struct {
		StopReason string `json:"stopReason"`
	}
	r.cl.ok("session/prompt", map[string]any{"sessionId": id, "prompt": fix.Prompt}, &out)
	r.model.mu.Lock()
	sent := strings.Join(r.model.bodies, "\n")
	r.model.mu.Unlock()
	if out.StopReason != "end_turn" || !strings.Contains(sent, "Fix the problems the editor reports in lines 3-5") ||
		!strings.Contains(sent, "What the person attached in the editor, not read through the session's policy") || !strings.Contains(sent, "undefined: y") {
		t.Fatalf("stop %q; the model was sent:\n%s", out.StopReason, sent)
	}

	// No diagnostics: Fix is not preferred and carries none; a test file is offered no tests.
	res = codeActionsResult{}
	r.cl.ok("_abhed/codeActions", map[string]any{"sessionId": id, "uri": lsprelay.URI(filepath.Join(r.ws, "pkg", "calc_test.go")), "range": rangeOf(0, 0, 0, 3)}, &res)
	if len(res.Actions) != 2 || res.Actions[1].IsPreferred || res.Actions[1].Title != "Fix with Abhed" || len(res.Actions[1].Prompt) != 1 ||
		!strings.Contains(string(res.Actions[0].Prompt[0]), `line 1 of \"pkg/calc_test.go\"`) {
		t.Fatalf("test file: %+v", res.Actions)
	}

	// A file the agent may not read, one outside the workspace, Abhed's own
	// state and a range that is not one get no actions.
	for _, c := range []struct {
		uri  string
		rng  map[string]any
		code int
		want string
	}{
		{lsprelay.URI(filepath.Join(r.ws, "hidden", "k.go")), rangeOf(0, 0, 0, 1), errRefused, "a rule keeps this file from being read"},
		{"file:///etc/hosts", rangeOf(0, 0, 0, 1), errRefused, "not in the session's workspace"},
		{"https://example.com/a.go", rangeOf(0, 0, 0, 1), errRefused, "not a file"},
		{lsprelay.URI(filepath.Join(r.ws, "deploy.pem")), rangeOf(0, 0, 0, 1), errRefused, "keys or credentials"},
		{lsprelay.URI(filepath.Join(r.ws, ".abhed", "policy.json")), rangeOf(0, 0, 0, 1), errRefused, "Abhed's own state"},
		{uri, rangeOf(4, 0, 2, 0), errParams, "not a range"},
		{uri, rangeOf(-1, 0, 2, 0), errParams, "not a range"},
	} {
		if msg := r.cl.refused(c.code, "_abhed/codeActions", map[string]any{"sessionId": id, "uri": c.uri, "range": c.rng}); !strings.Contains(msg, c.want) {
			t.Fatalf("%s: %s", c.uri, msg)
		}
	}

	// A read-only session offers nothing, as its prompts would be refused.
	r.cl.conn.sessMu.Lock()
	r.cl.conn.sessions[id].readOnly = "the record failed verification at seq 3"
	r.cl.conn.sessMu.Unlock()
	if msg := r.cl.refused(errRecord, "_abhed/codeActions", map[string]any{"sessionId": id, "uri": uri, "range": rangeOf(0, 0, 0, 1)}); !strings.Contains(msg, "fork it to go on") {
		t.Fatalf("read-only: %s", msg)
	}
}
