package agentdefs

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// -agents definitions load through the same checks as files, with source
// session; one that widens or is malformed is refused and named.
func TestParseSession(t *testing.T) {
	raw := `{
	  "reviewer": {"description": "Reviews diffs", "prompt": "Review carefully.", "tools": ["Read", "Grep"],
	    "permissionMode": "plan", "effort": "low", "mcpServers": ["docs"], "color": "blue", "maxTurns": 5},
	  "wide": {"description": "d", "prompt": "p", "permissionMode": "bypassPermissions"},
	  "inline": {"description": "d", "prompt": "p", "mcpServers": {"gh": {"command": "npx"}}},
	  "hooked": {"description": "d", "prompt": "p", "hooks": {"PreToolUse": []}},
	  "nested": {"description": "d", "prompt": "p", "settings": {"tools": ["bash"]}},
	  "twice": {"description": "d", "prompt": "p", "tools": ["read"], "Tools": ["bash"]},
	  "noprompt": {"description": "d"},
	  "explore": {"description": "d", "prompt": "p"},
	  "Bad Name": {"description": "d", "prompt": "p"},
	  "renamed": {"name": "other", "description": "d", "prompt": "p"}
	}`
	defs, _, errs := ParseSession([]byte(raw), nil)
	if len(defs) != 1 {
		t.Fatalf("loaded %d: %v", len(defs), errs)
	}
	d := defs[0]
	if d.Name != "reviewer" || d.Source != agent.SourceSession || d.PermissionMode != "plan" || d.Effort != "low" ||
		d.MaxTurns != 5 || d.Color != "blue" || len(d.MCPServers) != 1 || d.Instruction != "Review carefully." || len(d.SHA256) != 64 {
		t.Fatalf("%+v", d)
	}
	text := errText(errs)
	for _, name := range []string{"wide", "inline", "hooked", "nested", "twice", "noprompt", "explore", "Bad Name", "renamed"} {
		if !strings.Contains(text, "-agents "+name+" refused") {
			t.Errorf("%s was not refused:\n%s", name, text)
		}
	}
	for _, bad := range []string{`[]`, `{"a":`, `"x"`, `{} {}`} {
		if defs, _, errs := ParseSession([]byte(bad), nil); len(defs) != 0 || len(errs) == 0 {
			t.Errorf("%s parsed", bad)
		}
	}
}

// A session definition wins a name over the operator's, never over the
// organisation's, and none loads when only managed definitions may.
func TestSessionDefinitionsPrecedence(t *testing.T) {
	mdir, odir := t.TempDir(), t.TempDir()
	writeDef(t, mdir, "sec.md", def("sec", ""))
	writeDef(t, odir, "helper.md", def("helper", ""))
	session, _, errs := ParseSession([]byte(`{"sec":{"description":"d","prompt":"mine"},"helper":{"description":"d","prompt":"mine"}}`), nil)
	if len(session) != 2 || len(errs) != 0 {
		t.Fatalf("%v %v", session, errs)
	}
	defs, errs := Load(Options{ManagedDir: mdir, Dirs: []string{odir}, Session: session})
	got := map[string]string{}
	for _, d := range defs {
		got[d.Name] = d.Source
	}
	if got["sec"] != agent.SourceManaged || got["helper"] != agent.SourceSession || len(defs) != 2 {
		t.Fatalf("sources %v", got)
	}
	text := errText(errs)
	if !strings.Contains(text, "-agents sec refused") || !strings.Contains(text, "shadowed by -agents helper") {
		t.Fatalf("errors:\n%s", text)
	}
	defs, errs = Load(Options{Disabled: true, Session: session[1:]})
	if len(defs) != 0 || !strings.Contains(errText(errs), "only the organisation's definitions") {
		t.Fatalf("agents.disabled let a session definition load: %v", defs)
	}
}
