package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

// A git option that writes a file or runs a program is not approved by a
// wildcard allow rule, and offers no scope; a rule naming the command exactly
// still approves it, as does one allowing every command.
func TestWildcardAllowSkipsGitOptionsThatRunOrWrite(t *testing.T) {
	e := New(ModeDefault)
	if err := e.AddAllow("bash(git diff *)", "bash(git log *)", "bash(git show *)", "bash(git grep *)",
		"bash(git format-patch *)", "bash(git fetch *)", "bash(git *)", "bash(git diff --ext-diff HEAD)"); err != nil {
		t.Fatal(err)
	}
	ev := func(c string) Result {
		args, _ := json.Marshal(map[string]string{"command": c})
		return e.Evaluate("bash", true, args)
	}
	for _, c := range []string{
		"git diff --ext-diff", "git diff --ext", "git diff a.go --ext-diff", "git log -p --ext-diff",
		"git show --textconv", "git log -p --textc", "git -c diff.external=x diff", "git --config-env=diff.external=X diff",
		"git --exec-path=/tmp diff", "git -C sub diff", "git -p log", "git --paginate diff",
		"git grep -e -- -O/path/prog", "git grep --op=prog hello",
		"git rebase --ex 'prog rebase' HEAD~1", "git clone --u=prog file://repo dst", "git ls-remote --u=prog file://repo",
		"git clone -c core.sshCommand=prog a b", "git clone --template=/dir a b", "git init --template=/dir", "git format-patch --subject-prefix -- -o /dir -1 HEAD",
		"git difftool -x evil", "git filter-branch --tree-filter evil", "git send-email --to-cmd=evil x.patch",
		"git bisect run evil", "git submodule foreach evil", "git config core.pager evil",
		"git --git-dir=/x/.git log", "git --git-dir /x/.git log", "git --work-tree=/x diff", "git --work-tree /x status", "git grep -O x", "git grep --open-files-in-pager=vim x", "git grep --open x",
		"git format-patch -o /tmp HEAD~1", "git format-patch --output-directory /tmp HEAD~1",
	} {
		r := ev(c)
		if r.Decision != Ask || r.Offer() != "" || !strings.Contains(r.Reason, "no allow rule with a wildcard") || r.Suggest != "bash("+c+")" {
			t.Errorf("%s: %s offer %q suggest %q (%s)", c, r.Decision, r.Offer(), r.Suggest, r.Reason)
		}
	}
	// These already ask in every mode, as destructive.
	for _, c := range []string{"git diff --output=x", "git log --outp x", "git fetch --upload-pack=x origin", "git diff --no-index -O -- --output=/x a b"} {
		if r := ev(c); r.Decision != Ask || r.Offer() != "" {
			t.Errorf("%s: %s offer %q (%s)", c, r.Decision, r.Offer(), r.Reason)
		}
	}
	for _, c := range []string{"git diff --ext-diff HEAD", "git diff --stat", "git log -O order.txt", "git grep -n x", "git diff -- a.go",
		"git log -p --grep -- --ext-diff", "git config --get core.pager", "git bisect good", "git submodule status"} {
		if r := ev(c); r.Decision != Allow {
			t.Errorf("%s: %s (%s)", c, r.Decision, r.Reason)
		}
	}
	all := New(ModeDefault)
	if err := all.AddAllow("bash"); err != nil {
		t.Fatal(err)
	}
	if r := all.Evaluate("bash", true, []byte(`{"command":"git diff --ext-diff"}`)); r.Decision != Allow {
		t.Errorf("an allow for every command: %s (%s)", r.Decision, r.Reason)
	}
	// With no rule, the question offers no "always allow git diff" scope.
	none := New(ModeDefault)
	if r := none.Evaluate("bash", true, []byte(`{"command":"git diff --ext-diff"}`)); r.Offer() != "" {
		t.Errorf("offered %q", r.Offer())
	}
}
