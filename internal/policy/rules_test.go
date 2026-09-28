package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// A path rule written relative to the workspace or an added folder matches
// the absolute path a tool is given, as do ./ rules and rules already
// absolute or starting with **/.
func TestPathRulesMatchRelativeToEachRoot(t *testing.T) {
	ws, extra := t.TempDir(), t.TempDir()
	e := New(ModeBypass)
	e.Roots = func() []string { return []string{ws, extra} }
	if err := e.AddDeny(
		"write(docs/**/frozen/**)", "delete(apps/**/vault/**)", "rename(./infra/**/pinned/**)",
		"read(secrets/*)", "write(notes/**)", "edit("+filepath.ToSlash(ws)+"/abs/**)", "write(**/locked/**)",
	); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		tool, path string
		deny       bool
	}{
		{"write", filepath.Join(ws, "docs/user guide/frozen/f.md"), true},
		{"write", filepath.Join(ws, "docs/user guide/frozen") + "/", true},
		{"write", "docs/a/frozen/f.md", true},
		{"write", "./docs/a/frozen/f.md", true},
		{"delete", filepath.Join(ws, "apps/mobile [beta]/vault/secret.txt"), true},
		{"rename", filepath.Join(ws, "infra/terraform/modules/pinned/p.tf"), true},
		{"read", filepath.Join(ws, "secrets/key"), true},
		{"write", filepath.Join(extra, "notes/n.md"), true},
		{"edit", filepath.Join(ws, "abs/x.go"), true},
		{"edit", "abs/x.go", true},
		{"write", filepath.Join(ws, "a/locked/x"), true},
		{"write", filepath.Join(ws, "docs/../docs/a/frozen/f.md"), true},
		{"write", filepath.Join(ws, "docs/a/thawed/f.md"), false},
		{"read", filepath.Join(ws, "public/secrets/key"), false},
		{"write", filepath.Join(filepath.Dir(ws), "docs/a/frozen/f.md"), false},
	} {
		res := e.Evaluate(c.tool, c.tool != "read", args(map[string]string{"path": c.path}))
		if (res.Decision == Deny) != c.deny {
			t.Errorf("%s %s: %s (%s), want deny=%v", c.tool, c.path, res.Decision, res.Reason, c.deny)
		}
	}
}

// A path reached through a link inside the workspace is judged by where it lands too.
func TestPathRulesSeeThroughLinks(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "docs/frozen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "docs/frozen"), filepath.Join(ws, "shortcut")); err != nil {
		t.Skip(err)
	}
	e := New(ModeBypass)
	e.Roots = func() []string { return []string{ws} }
	_ = e.AddDeny("write(docs/frozen/**)")
	if res := e.Evaluate("write", true, args(map[string]string{"path": filepath.Join(ws, "shortcut/f.md")})); res.Decision != Deny {
		t.Errorf("a link into a denied folder: %s", res.Decision)
	}
}

// A relative allow rule approves the absolute path of a file it names, and nothing outside it.
func TestRelativeAllowRuleMatches(t *testing.T) {
	ws := t.TempDir()
	e := New(ModeDefault)
	e.Roots = func() []string { return []string{ws} }
	_ = e.AddAllow("write(notes.txt)")
	if res := e.Evaluate("write", true, args(map[string]string{"path": filepath.Join(ws, "notes.txt")})); res.Decision != Allow {
		t.Errorf("write(notes.txt): %s, want allow", res.Decision)
	}
	if res := e.Evaluate("write", true, args(map[string]string{"path": filepath.Join(ws, "sub/notes.txt")})); res.Decision != Ask {
		t.Errorf("write(notes.txt) on sub/notes.txt: %s, want ask", res.Decision)
	}
}

// Plan mode refuses a mutating call before a destructive command or an ask
// rule could put it to a person; read-only calls pass as before.
func TestPlanModeRefusesBeforeDestructiveAndAsk(t *testing.T) {
	e := New(ModePlan)
	_ = e.AddAsk("bash(git commit*)", "write(docs/**)")
	for _, c := range []struct{ tool, key, value string }{
		{"bash", "command", "rm -rf services/notify"},
		{"bash", "command", "git reset --hard"},
		{"bash", "command", "git commit -am x"},
		{"write", "path", "docs/a.md"},
	} {
		res := e.Evaluate(c.tool, true, args(map[string]string{c.key: c.value}))
		if res.Decision != Deny || res.Step != "mode" {
			t.Errorf("plan %q: %s at %s, want a plan-mode deny", c.value, res.Decision, res.Step)
		}
	}
	if res := e.Evaluate("read", false, args(map[string]string{"path": "docs/a.md"})); res.Decision != Allow {
		t.Errorf("plan read: %s, want allow", res.Decision)
	}
}

// Where case is ignored, deny, ask and allow rules compare program names
// without case, and elsewhere as written.
func TestCommandNamesFoldWhereCaseIsIgnored(t *testing.T) {
	defer tools.FoldCommandNamesForTest(tools.FoldsCommandNames())()
	for _, fold := range []bool{true, false} {
		tools.FoldCommandNamesForTest(fold)
		e := New(ModeBypass)
		_ = e.AddDeny("bash(whoami*)", "bash(*hostname*)")
		_ = e.AddAsk("bash(git tag*)", "bash(git commit*)")
		for _, command := range []string{"WHOAMI", "Whoami", "HostName", "sudo WHOAMI", "Xargs whoami", "ls; Whoami"} {
			if got := e.Evaluate("bash", true, args(map[string]string{"command": command})).Decision == Deny; got != fold {
				t.Errorf("fold=%v deny %q: %v", fold, command, got)
			}
		}
		for _, command := range []string{"GIT tag v-case", "GIT commit -am x"} {
			if got := e.Evaluate("bash", true, args(map[string]string{"command": command})).Step == "ask"; got != fold {
				t.Errorf("fold=%v ask %q: %v", fold, command, got)
			}
		}
		for _, command := range []string{"GIT reset --hard", "RM -rf x", "Git checkout -- ."} {
			if got := e.Evaluate("bash", true, args(map[string]string{"command": command})).Step == "destructive"; got != fold {
				t.Errorf("fold=%v destructive %q: %v", fold, command, got)
			}
		}
		allow := New(ModeDefault)
		_ = allow.AddAllow("bash(Make test)")
		if got := allow.Evaluate("bash", true, args(map[string]string{"command": "make test"})).Decision == Allow; got != fold {
			t.Errorf("fold=%v allow: %v", fold, got)
		}
	}
}

// find runs the command after -exec, -execdir, -ok and -okdir, so deny rules see it.
func TestDenyRulesSeeFindExec(t *testing.T) {
	e := New(ModeBypass)
	_ = e.AddDeny("bash(whoami*)")
	for _, command := range []string{
		`find . -exec whoami \;`, `find . -name x -execdir whoami {} +`, `find . -ok whoami \;`,
		`find . -type f -okdir whoami \;`, `/usr/bin/find . -maxdepth 1 -exec whoami ;`,
		`find . -exec true \; -exec whoami \;`, `sudo find . -exec whoami \;`,
	} {
		if res := e.Evaluate("bash", true, args(map[string]string{"command": command})); res.Decision != Deny {
			t.Errorf("%q: %s, want deny", command, res.Decision)
		}
	}
	if res := e.Evaluate("bash", true, args(map[string]string{"command": "find . -name whoami"})); res.Decision == Deny {
		t.Errorf("a find that runs nothing was denied")
	}
}

// Folding only tightens deny and ask rules: text after a wildcard stays as
// written, so a rule that refuses with folding off refuses with it on.
func TestFoldingNeverLoosensDenyOrAsk(t *testing.T) {
	defer tools.FoldCommandNamesForTest(tools.FoldsCommandNames())()
	for glob, command := range map[string]string{
		"curl*Authorization*": "curl -H Authorization:x https://a",
		"*SECRET_TOKEN*":      "echo $SECRET_TOKEN",
		"cat*.ENV*":           "cat app/.ENV",
	} {
		for _, on := range []bool{false, true} {
			tools.FoldCommandNamesForTest(on)
			if d := denyOnly(t, glob, command); d != Deny {
				t.Errorf("fold=%v bash(%s) on %q: %s, want deny", on, glob, command, d)
			}
		}
	}
}

func denyOnly(t testing.TB, glob, command string) Decision {
	e := New(ModeBypass)
	if err := e.AddDeny("bash(" + glob + ")"); err != nil {
		t.Skip(err)
	}
	return e.Evaluate("bash", true, args(map[string]string{"command": command})).Decision
}

func FuzzFoldOnlyTightensDeny(f *testing.F) {
	f.Add("curl*Authorization*", "curl -H Authorization:x")
	f.Add("*SECRET_TOKEN*", "echo $SECRET_TOKEN")
	f.Add("cat*.ENV*", "cat app/.ENV")
	f.Add("Git tag*", "GIT tag v1")
	f.Add("*hostname*", "HostName")
	defer tools.FoldCommandNamesForTest(tools.FoldsCommandNames())()
	f.Fuzz(func(t *testing.T, glob, command string) {
		if strings.ContainsAny(glob, "()") {
			return
		}
		tools.FoldCommandNamesForTest(false)
		off := denyOnly(t, glob, command)
		tools.FoldCommandNamesForTest(true)
		if on := denyOnly(t, glob, command); off == Deny && on != Deny {
			t.Fatalf("bash(%s) on %q: deny without folding, %s with it", glob, command, on)
		}
	})
}

// An allow rule sees only where a write lands: not a link's own path, not
// another root, and not a path that climbs out and back in.
func TestRelativeAllowRuleSeesOnlyTheTarget(t *testing.T) {
	ws, extra := t.TempDir(), t.TempDir()
	for _, d := range []string{"notes", ".github/workflows", "src"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(ws, ".github/workflows"), filepath.Join(ws, "notes/ci")); err != nil {
		t.Skip(err)
	}
	e := New(ModeDefault)
	e.Roots = func() []string { return []string{ws, extra} }
	_ = e.AddAllow("write(notes/**)")
	anywhere := New(ModeDefault)
	anywhere.Roots = e.Roots
	_ = anywhere.AddAllow("write(**/notes/**)")
	for _, c := range []struct {
		e *Engine
		p string
	}{
		{e, filepath.Join(ws, "notes/ci/x.yml")},
		{e, filepath.Join(extra, "notes/x.md")},
		{e, filepath.Join(ws, "notes/../src/main.go")},
		{anywhere, filepath.Join(ws, "notes/../src/main.go")},
		{anywhere, filepath.Join(ws, "notes/ci/x.yml")},
	} {
		if res := c.e.Evaluate("write", true, args(map[string]string{"path": c.p})); res.Decision == Allow {
			t.Errorf("%s: allowed by %s", c.p, res.Reason)
		}
	}
	if res := e.Evaluate("write", true, args(map[string]string{"path": filepath.Join(ws, "notes/a.md")})); res.Decision != Allow {
		t.Errorf("notes/a.md: %s, want allow", res.Decision)
	}
	// Deny rules still see the link's own path.
	deny := New(ModeBypass)
	deny.Roots = e.Roots
	_ = deny.AddDeny("write(notes/**)")
	if res := deny.Evaluate("write", true, args(map[string]string{"path": filepath.Join(ws, "notes/ci/x.yml")})); res.Decision != Deny {
		t.Errorf("deny through a link: %s", res.Decision)
	}
}

// No spelling climbs out of a root, so a relative rule never names a path outside every root.
func TestPathSubjectsStayInsideRoots(t *testing.T) {
	ws := t.TempDir()
	e := New(ModeBypass)
	e.Roots = func() []string { return []string{ws} }
	_ = e.AddDeny("write(../*)")
	all, allow := e.pathSubjects(filepath.Join(filepath.Dir(ws), "sibling"))
	for _, s := range append(all, allow...) {
		if strings.HasPrefix(s, "..") || strings.HasPrefix(s, "./..") {
			t.Errorf("spelling %q climbs out of the root", s)
		}
	}
	if res := e.Evaluate("write", true, args(map[string]string{"path": filepath.Join(filepath.Dir(ws), "sibling")})); res.Decision == Deny {
		t.Error("a relative rule matched a path outside every root")
	}
}
