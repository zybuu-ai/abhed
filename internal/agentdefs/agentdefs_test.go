package agentdefs

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

func writeDef(t *testing.T, dir, file, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func def(name, extra string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s role\n%s---\nDo the %s work.\n", name, name, extra, name)
}

func errText(errs []error) string {
	var b strings.Builder
	for _, e := range errs {
		b.WriteString(e.Error())
		b.WriteString("\n")
	}
	return b.String()
}

// Files in the common agent-file format load unchanged: a comma tool string
// with capitalised names, camelCase keys, model inherit, a cosmetic key.
func TestCommonFormatLoads(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "code-reviewer.md", "---\nname: code-reviewer\ndescription: Reviews a change for bugs\n"+
		"tools: Read, Grep, Glob\ndisallowedTools: [Bash]\nmodel: inherit\nmaxTurns: 12\npermissionMode: plan\ncolor: blue\n---\nReview carefully.\n")
	writeDef(t, dir, "lister.md", "---\ndescription: >\n  Lists files\n  when asked\ntools:\n  - glob\n  - \"mcp__docs__*\"\n---\nList.\n")
	defs, errs := Load(Options{Dirs: []string{dir}})
	if len(defs) != 2 {
		t.Fatalf("want 2 definitions, got %d: %s", len(defs), errText(errs))
	}
	r := defs[0]
	if r.Name != "code-reviewer" || !reflect.DeepEqual(r.Tools, []string{"Read", "Grep", "Glob"}) ||
		!reflect.DeepEqual(r.DisallowedTools, []string{"Bash"}) || r.Model != "" || r.MaxTurns != 12 ||
		r.PermissionMode != "plan" || r.Instruction != "Review carefully." || r.Source != agent.SourceOperator || r.SHA256 == "" {
		t.Fatalf("code-reviewer: %+v", r)
	}
	if !strings.Contains(errText(errs), `"color" is ignored`) {
		t.Fatalf("the cosmetic key was not reported: %s", errText(errs))
	}
	l := defs[1]
	if l.Name != "lister" || l.Description != "Lists files when asked" || !reflect.DeepEqual(l.Tools, []string{"glob", "mcp__docs__*"}) {
		t.Fatalf("lister: %+v", l)
	}
}

// A file cannot take a built-in role's name; the built-in stays.
func TestReservedNameRefused(t *testing.T) {
	for _, name := range agent.ReservedNames {
		_, _, err := Parse(name+".md", []byte(def(name, "tools: bash\n")), agent.SourceWorkspace, nil)
		if err == nil || !strings.Contains(err.Error(), "built-in agent type") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	ws := t.TempDir()
	p := writeDef(t, ws, "explore.md", "---\ndescription: explore with a shell\ntools: bash\n---\nRun anything.\n")
	defs, errs := Load(Options{Workspace: []config.AgentFile{{Path: p, Data: []byte("---\ndescription: explore with a shell\ntools: bash\n---\nRun anything.\n")}}})
	if len(defs) != 0 || !strings.Contains(errText(errs), "cannot be redefined") {
		t.Fatalf("a workspace explore.md loaded: %+v %s", defs, errText(errs))
	}
	all := agent.WithDefinitions(defs...)
	if d, _ := all.Get("explore"); d.Source != agent.SourceBuiltin || reflect.DeepEqual(d.Tools, []string{"bash"}) {
		t.Fatalf("the built-in explore was replaced: %+v", d)
	}
}

// Keys that would concern authority, and a mode wider than default, refuse
// the definition rather than load a looser agent than its author meant.
func TestWideningKeyRefusesDefinition(t *testing.T) {
	for _, extra := range []string{
		"permissionMode: bypassPermissions\n",
		"permission_mode: acceptEdits\n",
		"hooks:\n  PreToolUse: x\n",
		"mcpServers:\n  - gh\n",
		"allowed-tools: bash\n",
		"allowedTools: [bash]\n",
		"permissions:\n  allow: [bash]\n",
		"sandbox: none\n",
		// Lookalikes of honoured keys, which a reader would take as restrictions.
		"denied_tools: [bash]\n",
		"blocked_tools: bash\n",
		"permission: plan\n",
		"max_turn: 3\n",
		"mode: plan\n",
		"exclude: bash\n",
		"block: [bash]\n",
		"blocked: bash\n",
		"restrict: read\n",
		// Nested under a key that is not ours.
		"settings:\n  disallowedTools: bash\n",
		"settings:\n  tools: read\n",
	} {
		_, _, err := Parse("x.md", []byte(def("x", extra)), agent.SourceOperator, nil)
		if err == nil {
			t.Fatalf("%q loaded", extra)
		}
	}
}

// A quoted key is the key: a quoted restriction binds as an unquoted one does.
func TestQuotedKeysBind(t *testing.T) {
	d, _, err := Parse("x.md", []byte("---\n\"name\": x\n'description': d\n\"tools\": [read]\n\"permissionMode\": plan\n\"max_turns\": 5\n\"disallowed_tools\": [grep]\n---\nbody\n"),
		agent.SourceOperator, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Tools, []string{"read"}) || d.PermissionMode != "plan" || d.MaxTurns != 5 ||
		!reflect.DeepEqual(d.DisallowedTools, []string{"grep"}) || d.Name != "x" || d.Description != "d" {
		t.Fatalf("quoted keys were not honoured: %+v", d)
	}
	if _, _, err := Parse("x.md", []byte(def("x", "\"hooks\": x\n")), agent.SourceOperator, nil); err == nil {
		t.Fatal("a quoted authority key loaded")
	}
}

// Everything a definition must hold, and the shapes it must have.
func TestDefinitionValidation(t *testing.T) {
	long := strings.Repeat("a", MaxDescription+1)
	for name, body := range map[string]string{
		"no description": "---\nname: x\n---\nbody\n",
		"long":           "---\nname: x\ndescription: " + long + "\n---\nbody\n",
		"empty body":     "---\nname: x\ndescription: d\n---\n\n",
		"big body":       "---\nname: x\ndescription: d\n---\n" + strings.Repeat("b", MaxBody+1),
		"bad name":       "---\nname: Bad_Name\ndescription: d\n---\nbody\n",
		"turns":          "---\nname: x\ndescription: d\nmax_turns: 101\n---\nbody\n",
		"turns word":     "---\nname: x\ndescription: d\nmax_turns: many\n---\nbody\n",
		"isolation":      "---\nname: x\ndescription: d\nisolation: container\n---\nbody\n",
		"wildcard":       "---\nname: x\ndescription: d\ntools: \"*\"\n---\nbody\n",
		"glob tool":      "---\nname: x\ndescription: d\ntools: read*\n---\nbody\n",
		"twice":          "---\nname: x\ndescription: d\ntools: read\ntools: bash\n---\nbody\n",
		"map tools":      "---\nname: x\ndescription: d\ntools:\n  read: yes\n---\nbody\n",
		"no header":      "just a body\n",
	} {
		if _, _, err := Parse("x.md", []byte(body), agent.SourceOperator, nil); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	d, _, err := Parse("/defs/from-file.md", []byte("---\ndescription: d\n---\nbody\n"), agent.SourceOperator, nil)
	if err != nil || d.Name != "from-file" {
		t.Fatalf("the name defaults to the file's: %+v %v", d, err)
	}
}

// A model is a configured provider name, never an endpoint; an unknown one
// refuses the definition and names what is configured.
func TestDefinitionModelIsAConfiguredName(t *testing.T) {
	models := []string{"fast", "local"}
	if d, _, err := Parse("x.md", []byte(def("x", "model: fast\n")), agent.SourceOperator, models); err != nil || d.Model != "fast" {
		t.Fatalf("a configured model: %+v %v", d, err)
	}
	_, _, err := Parse("x.md", []byte(def("x", "model: swift\n")), agent.SourceOperator, models)
	if err == nil || !strings.Contains(err.Error(), "available: fast, local") {
		t.Fatalf("an alias that is not configured: %v", err)
	}
	for _, v := range []string{"http://evil.example/v1", "vendor/big", "a b"} {
		if _, _, err := Parse("x.md", []byte(def("x", "model: "+v+"\n")), agent.SourceOperator, append(models, v)); err == nil {
			t.Fatalf("model %q loaded", v)
		}
	}
}

// Managed definitions win over the workspace's and the operator's, and a
// shadowed file is named; the later operator directory wins over an earlier.
func TestManagedAgentWins(t *testing.T) {
	managed, ws, op1, op2 := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	writeDef(t, managed, "shared.md", def("shared", "tools: read\n"))
	wsPath := writeDef(t, ws, "shared.md", def("shared", "tools: bash\n"))
	writeDef(t, op1, "shared.md", def("shared", "tools: write\n"))
	writeDef(t, op1, "later.md", def("later", "tools: read\n"))
	writeDef(t, op2, "later.md", def("later", "tools: grep\n"))
	wsData, _ := os.ReadFile(wsPath)
	defs, errs := Load(Options{ManagedDir: managed, Dirs: []string{op1, op2},
		Workspace: []config.AgentFile{{Path: wsPath, Data: wsData}}})
	got := map[string]*agent.Definition{}
	for _, d := range defs {
		got[d.Name] = d
	}
	if s := got["shared"]; s == nil || s.Source != agent.SourceManaged || s.Tools[0] != "read" {
		t.Fatalf("managed did not win: %+v", s)
	}
	if l := got["later"]; l == nil || l.Tools[0] != "grep" {
		t.Fatalf("the later operator directory did not win: %+v", l)
	}
	msg := errText(errs)
	if strings.Count(msg, "is shadowed by") != 3 || !strings.Contains(msg, wsPath) {
		t.Fatalf("shadowed files not named: %s", msg)
	}

	// Disabled keeps the managed definitions only.
	defs, _ = Load(Options{ManagedDir: managed, Dirs: []string{op1, op2}, Disabled: true,
		Workspace: []config.AgentFile{{Path: wsPath, Data: wsData}}})
	if len(defs) != 1 || defs[0].Source != agent.SourceManaged {
		t.Fatalf("disabled loaded more than managed: %+v", defs)
	}
}

// A managed file that does not load on this host still owns its name: no
// workspace or operator file takes the role in its place.
func TestRefusedManagedNameStaysReserved(t *testing.T) {
	managed, ws, op := t.TempDir(), t.TempDir(), t.TempDir()
	writeDef(t, managed, "sec.md", def("sec", "model: corp\n")) // corp is not offered here
	writeDef(t, managed, "stem.md", "no header at all")         // claims its file name
	wsPath := writeDef(t, ws, "sec.md", def("sec", ""))
	wsData, _ := os.ReadFile(wsPath)
	writeDef(t, op, "sec.md", def("sec", ""))
	writeDef(t, op, "stem.md", def("stem", ""))
	writeDef(t, op, "free.md", def("free", ""))
	defs, errs := Load(Options{ManagedDir: managed, Dirs: []string{op}, Models: []string{"local"},
		Workspace: []config.AgentFile{{Path: wsPath, Data: wsData}}})
	if len(defs) != 1 || defs[0].Name != "free" {
		t.Fatalf("a lower level took a managed name: %+v", defs)
	}
	if msg := errText(errs); strings.Count(msg, "belongs to the organisation's") != 3 {
		t.Fatalf("the refusals do not say why: %s", msg)
	}

	// Files refused while the directory is read still claim their names: a
	// link to an unsafe target, an oversized file, a second hard link and an
	// unreadable file.
	managed, op = t.TempDir(), t.TempDir()
	target := writeDef(t, t.TempDir(), "t.md", def("linked", ""))
	if err := os.Symlink(target, filepath.Join(managed, "linked.md")); err != nil {
		t.Fatal(err)
	}
	writeDef(t, managed, "big.md", def("big", "")+strings.Repeat("x", 70<<10))
	hard := writeDef(t, t.TempDir(), "h.md", def("hard", ""))
	if err := os.Link(hard, filepath.Join(managed, "hard.md")); err != nil {
		t.Fatal(err)
	}
	locked := writeDef(t, managed, "locked.md", def("locked", ""))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
	for _, n := range []string{"linked", "big", "hard", "locked", "free"} {
		writeDef(t, op, n+".md", def(n, ""))
	}
	defs, errs = Load(Options{ManagedDir: managed, Dirs: []string{op}})
	if len(defs) != 1 || defs[0].Name != "free" {
		t.Fatalf("a file refused while reading gave up its name: %+v\n%s", defs, errText(errs))
	}

	// A link to a root-owned, unshared file is followed.
	old := managedOwnerOK
	managedOwnerOK = func(os.FileInfo) bool { return true }
	defer func() { managedOwnerOK = old }()
	defs, _ = Load(Options{ManagedDir: managed, Dirs: []string{op}})
	names := map[string]string{}
	for _, d := range defs {
		names[d.Name] = d.Source
	}
	if names["linked"] != agent.SourceManaged {
		t.Fatalf("a safe managed link was not followed: %v", names)
	}
}

// A managed directory that exists but cannot be listed fails closed: no
// workspace or operator definition loads, since which names it holds is
// unknown.
func TestUnlistableManagedDirFailsClosed(t *testing.T) {
	managed, op := t.TempDir(), t.TempDir()
	writeDef(t, op, "free.md", def("free", ""))
	if err := os.Chmod(managed, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(managed, 0o700) })
	if _, err := os.ReadDir(managed); err == nil {
		t.Skip("the directory can still be listed (running as root?)")
	}
	defs, errs := Load(Options{ManagedDir: managed, Dirs: []string{op}})
	if len(defs) != 0 || !strings.Contains(errText(errs), "cannot be listed") {
		t.Fatalf("an unlistable managed directory let definitions load: %+v %s", defs, errText(errs))
	}
}

// An operator's definition that is a link is refused, as a workspace's is.
func TestOperatorLinkRefused(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	target := writeDef(t, other, "x.md", def("x", ""))
	if err := os.Symlink(target, filepath.Join(dir, "x.md")); err != nil {
		t.Fatal(err)
	}
	defs, errs := Load(Options{Dirs: []string{dir}})
	if len(defs) != 0 || !strings.Contains(errText(errs), "not a regular file") {
		t.Fatalf("a linked definition loaded: %+v %s", defs, errText(errs))
	}
}

// The ownership check itself: a file the test owns, not root, fails it, as
// does one others may write.
func TestRootOwnership(t *testing.T) {
	p := writeDef(t, t.TempDir(), "x.md", def("x", ""))
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getuid() != 0 && rootOwnedNotShared(info) {
		t.Fatal("a file owned by the test user passed as root's")
	}
}
