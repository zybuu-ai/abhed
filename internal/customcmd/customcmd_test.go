package customcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func file(rel, content string) File {
	return File{Rel: rel, Path: "/x/" + rel, Data: []byte(content)}
}

func TestParseFrontmatter(t *testing.T) {
	c, err := Parse(file("review.md", "---\ndescription: Review  the diff\nargument-hint: <path>\nallowed-tools: [read, grep]\nmodel: big\n---\nReview $1 please"), SourceUser, []string{"big"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "/review" || c.Description != "Review the diff" || c.ArgumentHint != "<path>" || c.Model != "big" ||
		strings.Join(c.AllowedTools, ",") != "read,grep" || c.Body != "Review $1 please" || len(c.SHA256) != 64 {
		t.Fatalf("%+v", c)
	}
	if c, err := Parse(file("front/test.md", "just a prompt"), SourceUser, nil); err != nil || c.Name != "/front:test" || c.Body != "just a prompt" {
		t.Fatalf("no header: %+v %v", c, err)
	}
	for _, bad := range []string{
		"---\npermission-mode: bypass\n---\nx",        // a command cannot change the mode
		"---\nallowed-tools: Bash(rm:*)\n---\nx",      // tools, not rules
		"---\nmodel: elsewhere\n---\nx",               // not a configured provider
		"---\nhooks: x\n---\nx",                       // not a key a command sets
		"---\ndescription: x\n---\n",                  // no body
		"---\nallowed-tools: []\n---\nx",              // an empty list narrows to nothing by mistake
		"---\ndisable-model-invocation: true\n---\nx", // another product's key
	} {
		if _, err := Parse(file("x.md", bad), SourceUser, []string{"big"}); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
	if _, err := Parse(file("bad name!.md", "x"), SourceUser, nil); err == nil {
		t.Error("accepted a name that is not plain")
	}
}

func TestExpandArguments(t *testing.T) {
	cases := []struct{ body, args, want string }{
		{"fix $ARGUMENTS now", "the bug", "fix the bug now"},
		{"$1 then $2, not $3", `a "b c"`, "a then b c, not "},
		{"plain", "extra words", "plain\n\nArguments: extra words"},
		{"plain", "", "plain"},
	}
	for _, c := range cases {
		if got := Expand(c.body, c.args); got != c.want {
			t.Errorf("Expand(%q, %q) = %q, want %q", c.body, c.args, got, c.want)
		}
	}
	body := "status: !`git status` and !`ls`"
	if got := InlineCommands(body); len(got) != 2 || got[0] != "git status" || got[1] != "ls" {
		t.Fatalf("inline: %v", got)
	}
	if got := ReplaceInline(body, strings.ToUpper); got != "status: GIT STATUS and LS" {
		t.Fatalf("replace: %q", got)
	}
}

func TestReadDirRefusesLinksAndHashCoversContent(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "a.md"), []byte("A"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "ns"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "ns", "b.md"), []byte("B"), 0o644))
	must(os.WriteFile(filepath.Join(outside, "evil.md"), []byte("EVIL"), 0o644))
	if err := os.Symlink(filepath.Join(outside, "evil.md"), filepath.Join(dir, "evil.md")); err != nil {
		t.Skip(err)
	}
	files, errs := ReadDir(dir)
	if len(files) != 2 || files[0].Rel != "a.md" || files[1].Rel != "ns/b.md" || len(errs) != 1 || !strings.Contains(errs[0].Error(), "a link is not followed") {
		t.Fatalf("files %+v errs %v", files, errs)
	}
	sum := HashFiles(files)
	files[1].Data = []byte("B2")
	if HashFiles(files) == sum {
		t.Fatal("the hash does not cover content")
	}
}

func TestReadWorkspaceRefusesALinkedDirectory(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "x.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, ".abhed", "commands")); err != nil {
		t.Skip(err)
	}
	if files, _, errs := ReadWorkspace(ws); len(files) != 0 || len(errs) != 1 {
		t.Fatalf("read through a linked directory: %v %v", files, errs)
	}
}

// The organisation's names win, then the person's; a repository cannot
// take a name either uses.
func TestLoadPrecedence(t *testing.T) {
	managed, user := t.TempDir(), t.TempDir()
	for dir, body := range map[string]string{managed: "MANAGED", user: "USER"} {
		if err := os.WriteFile(filepath.Join(dir, "deploy.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(user, "mine.md"), []byte("MINE"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmds, errs := Load(Options{ManagedDir: managed, UserDirs: []string{user},
		Workspace: []File{file("deploy.md", "WORKSPACE"), file("mine.md", "WS-MINE"), file("ws.md", "WS")}})
	got := map[string]string{}
	for _, c := range cmds {
		got[c.Name] = c.Source + ":" + c.Body
	}
	if got["/deploy"] != "managed:MANAGED" || got["/mine"] != "user:MINE" || got["/ws"] != "workspace:WS" || len(errs) != 3 {
		t.Fatalf("%v %v", got, errs)
	}
}

func TestTrustStore(t *testing.T) {
	s := TrustStore{Path: filepath.Join(t.TempDir(), "t.json")}
	ws := t.TempDir()
	if ok, why := s.Decide(ws, "h1"); ok || why != "new" {
		t.Fatalf("%v %s", ok, why)
	}
	if err := s.Record(ws, "h1", true); err != nil {
		t.Fatal(err)
	}
	if ok, why := s.Decide(ws, "h1"); !ok || why != "stored" {
		t.Fatalf("%v %s", ok, why)
	}
	if ok, why := s.Decide(ws, "h2"); ok || why != "changed" {
		t.Fatalf("a change stayed trusted: %v %s", ok, why)
	}
	if err := s.Record(ws, "h2", false); err != nil {
		t.Fatal(err)
	}
	if ok, why := s.Decide(ws, "h2"); ok || why != "declined" {
		t.Fatalf("%v %s", ok, why)
	}
	if info, err := os.Stat(s.Path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("store mode: %v %v", info, err)
	}
}

func TestReadRegularRefusesLinksAndSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.md")
	if err := os.WriteFile(p, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadRegular(p, 5); err != nil || string(data) != "12345" {
		t.Fatalf("%q %v", data, err)
	}
	if _, err := ReadRegular(p, 4); err == nil {
		t.Fatal("read past the cap")
	}
	if err := os.Symlink(p, filepath.Join(dir, "l.md")); err != nil {
		t.Skip(err)
	}
	if _, err := ReadRegular(filepath.Join(dir, "l.md"), 5); err == nil {
		t.Fatal("read through a link")
	}
}
