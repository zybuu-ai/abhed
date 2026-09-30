package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// spoofPayload redraws the line: a zero-width joiner hides the comment, the
// carriage return and erase-line wipe what came before.
const spoofPayload = "touch pwned #\u200d\r\u001b[2K\b\u007f\u009b  $ ls -la"

// assertNothingHidden fails when the prompt carries a raw control or format
// character that would let the arguments rewrite what is on screen.
func assertNothingHidden(t *testing.T, out string) {
	t.Helper()
	for _, r := range out {
		switch {
		case r == '\n' || r == '\t':
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			t.Errorf("raw control character %U in the prompt: %q", r, out)
		case unicode.Is(unicode.Cf, r):
			t.Errorf("raw format character %U in the prompt: %q", r, out)
		}
	}
	if !strings.Contains(out, hiddenWarning) {
		t.Errorf("no warning line in the prompt: %q", out)
	}
}

func TestApprovePromptShowsHiddenCharacters(t *testing.T) {
	bash, _ := json.Marshal(map[string]string{"command": spoofPayload, "description": "x\r\u001b[2Kls"})
	write, _ := json.Marshal(map[string]string{"path": "a.sh", "content": "echo ok\n" + spoofPayload + "\n"})
	edit, _ := json.Marshal(map[string]string{"path": "a.sh", "old_string": spoofPayload, "new_string": spoofPayload})
	path, _ := json.Marshal(map[string]string{"path": spoofPayload, "content": "x"})
	cases := []struct {
		name, tool string
		args       []byte
		res        policy.Result
	}{
		{"bash", "bash", bash, policy.Result{}},
		{"write", "write", write, policy.Result{}},
		{"edit", "edit", edit, policy.Result{}},
		{"path", "write", path, policy.Result{Decision: policy.Ask, Step: "default", Scope: "write(" + spoofPayload + ")", Reason: spoofPayload}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out strings.Builder
			a := NewApprover(&out)
			a.In = strings.NewReader("r\n")
			ctx := agent.WithSubagent(context.Background(), spoofPayload)
			ok, err := a.Approve(ctx, c.tool, c.args, c.res)
			if err != nil || ok {
				t.Fatalf("got %v, %v; want a refusal", ok, err)
			}
			got := out.String()
			assertNothingHidden(t, got)
			for _, want := range []string{"touch pwned", "⟨U+200D⟩", `\r`, `\x1b`, "⟨U+009B⟩"} {
				if !strings.Contains(got, want) {
					t.Errorf("prompt lacks %q: %q", want, got)
				}
			}
		})
	}
}

// A plain call draws no warning: it must mean something when it appears.
func TestApprovePromptPlainCallHasNoWarning(t *testing.T) {
	var out strings.Builder
	a := NewApprover(&out)
	a.In = strings.NewReader("r\n")
	if _, err := a.Approve(context.Background(), "write", json.RawMessage(`{"path":"a.go","content":"package a\n\tfunc f() {}\n"}`), policy.Result{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), hiddenWarning) {
		t.Fatalf("warning on a plain call: %q", out.String())
	}
	if !strings.Contains(out.String(), "\tfunc f() {}") {
		t.Fatalf("a tab in the preview was altered: %q", out.String())
	}
}

// The one-line tool-call summary is drawn the same way.
func TestRendererToolLineShowsHiddenCharacters(t *testing.T) {
	var out strings.Builder
	r := NewRenderer(&out, false)
	args, _ := json.Marshal(map[string]string{"command": spoofPayload})
	payload, _ := json.Marshal(agent.ActionRequested{Tool: "bash", Args: args})
	r.Event(agent.Event{Type: agent.EvActionRequested, Payload: payload})
	got := out.String()
	if !strings.Contains(got, "touch pwned") || !strings.Contains(got, "⟨U+200D⟩") || strings.ContainsAny(got, "\r\u001b\u200d") {
		t.Fatalf("tool line not made visible: %q", got)
	}
}

func TestVisible(t *testing.T) {
	cases := []struct{ in, want, line string }{
		{"plain text", "plain text", "plain text"},
		{"a\tb", "a\tb", "a\tb"},
		{"a\nb", "a\nb", `a\nb`},
		{"a\rb", `a\rb`, `a\rb`},
		{"a\bb", `a\bb`, `a\bb`},
		{"\u001b[2K", `\x1b[2K`, `\x1b[2K`},
		{"\x00\x07\x0b\x0c", `\x00\a\v\f`, `\x00\a\v\f`},
		{"del\u007f", `del\x7f`, `del\x7f`},
		{"csi\u009b", "csi⟨U+009B⟩", "csi⟨U+009B⟩"},
		{"zw\u200bj\u200d", "zw⟨U+200B⟩j⟨U+200D⟩", "zw⟨U+200B⟩j⟨U+200D⟩"},
		{"bidi\u202e\u2066\u2069", "bidi⟨U+202E⟩⟨U+2066⟩⟨U+2069⟩", "bidi⟨U+202E⟩⟨U+2066⟩⟨U+2069⟩"},
		{"bom\ufeff", "bom⟨U+FEFF⟩", "bom⟨U+FEFF⟩"},
		{"tag\U000E0041", "tag⟨U+E0041⟩", "tag⟨U+E0041⟩"},
		{"sep\u2028", "sep⟨U+2028⟩", "sep⟨U+2028⟩"},
		{"bad\xffbyte", `bad\xffbyte`, `bad\xffbyte`},
		{"héllo ✓", "héllo ✓", "héllo ✓"},
	}
	for _, c := range cases {
		if got := Visible(c.in); got != c.want {
			t.Errorf("Visible(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := VisibleLine(c.in); got != c.line {
			t.Errorf("VisibleLine(%q) = %q, want %q", c.in, got, c.line)
		}
	}
}

// promptFor draws the approval prompt for one call and refuses it.
func promptFor(t *testing.T, tool string, args any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	a := NewApprover(&out)
	a.In = strings.NewReader("r\n")
	if ok, err := a.Approve(context.Background(), tool, raw, policy.Result{}); err != nil || ok {
		t.Fatalf("got %v, %v; want a refusal", ok, err)
	}
	return out.String()
}

// The warning covers every string in the call, including what no preview draws.
func TestApproveWarnsOnHiddenCharactersAnywhereInTheCall(t *testing.T) {
	var long []string
	for i := 0; i < 25; i++ {
		long = append(long, "line")
	}
	long[19] = "x\u202ey"
	manifest := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"},"data":{"k":"\u001b[2J"}}`
	cases := []struct {
		name, tool string
		args       any
	}{
		{"write past the preview", "write", map[string]string{"path": "a.txt", "content": strings.Join(long, "\n")}},
		{"bash carriage return", "bash", map[string]string{"command": "ls\rrm -rf x", "description": "list"}},
		{"k8s manifest escape", "k8s_apply", map[string]string{"action": "apply", "manifest": manifest}},
		{"task prompt joiner", "task", map[string]string{"description": "look", "prompt": "a\u200db"}},
		{"nested value", "mcp_x", map[string]any{"a": []any{1, map[string]any{"b": "\u2066"}}}},
		{"key", "mcp_x", map[string]any{"k\u200b": "v"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := promptFor(t, c.tool, c.args); !strings.Contains(got, hiddenWarning) {
				t.Errorf("no warning: %q", got)
			}
		})
	}
}

// The tools that had no preview show what they will do, escaped.
func TestApprovePreviewsClusterSubagentRemoteAndFetch(t *testing.T) {
	manifest := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"prod"},"spec":{"replicas":3,"note":"a\u202eb"}}`
	cases := []struct {
		name, tool string
		args       any
		want       []string
	}{
		{"k8s apply", "k8s_apply", map[string]string{"action": "apply", "cluster": "lab", "manifest": manifest},
			[]string{"apply · cluster lab · namespace prod · Deployment/web", `"replicas": 3`, "⟨U+202E⟩"}},
		{"k8s scale", "k8s_apply", map[string]any{"action": "scale", "resource": "deployments", "name": "web", "namespace": "prod", "replicas": 0},
			[]string{"scale · namespace prod · deployments/web · replicas 0"}},
		{"task", "task", map[string]string{"description": "look", "agent_type": "explore", "prompt": "one\ntwo\x1b[2J"},
			[]string{"subagent: explore", "    one", `two\x1b[2J`}},
		{"ssh", "ssh", map[string]string{"host": "db1", "command": "uptime\rrm"}, []string{`db1 $ uptime\rrm`}},
		{"web_fetch", "web_fetch", map[string]string{"url": "https://example.com/a?b=c#\u200d"}, []string{"https://example.com/a?b=c#⟨U+200D⟩"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := promptFor(t, c.tool, c.args)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("prompt lacks %q: %q", w, got)
				}
			}
			for _, r := range got {
				if r == 0x1b || r == '\r' || unicode.Is(unicode.Cf, r) {
					t.Errorf("raw %U in the prompt: %q", r, got)
				}
			}
		})
	}
	// A long prompt and manifest are cut, and say how much is left.
	got := promptFor(t, "task", map[string]string{"prompt": strings.Repeat("p\n", 30)})
	if !strings.Contains(got, "... 20 more lines") {
		t.Errorf("a long task prompt is not cut at 10 lines: %q", got)
	}
}

// A cut counts characters, so a multi-byte string is never split mid-rune.
func TestTruncateIsRuneSafe(t *testing.T) {
	if got := truncate("日本語テキスト", 3); got != "日本語..." {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("日本", 3); got != "日本" {
		t.Errorf("a short string was changed: %q", got)
	}
}
