package tools

import "testing"

// Each form of a git option that writes a file or runs a program is named:
// joined with =, as a separate word, abbreviated, quoted, after operands and
// before the subcommand. Read-only options, and anything after --, are not.
func TestGitRunsOrWritesNamesEveryForm(t *testing.T) {
	for _, c := range []string{
		"git diff --output=x", "git diff --output x", "git log --outp=x", "git diff HEAD a.go --output=x",
		"git diff --ext-diff", "git diff --ext", "git log -p --ext-d", "git diff a b --ext-diff",
		"git diff '--ext-diff'", `git diff --e"xt-diff"`, `git diff --ext\-diff`,
		"git log -p --textconv", "git show --textc", "git diff HEAD --textconv=x",
		"git -c diff.external=x diff", "git -cdiff.external=x diff", "git -c core.pager=x log",
		"git --config-env=diff.external=X diff", "git --config-env diff.external=X diff",
		"git --exec-path=/tmp log", "git --exec-path /tmp log", "git -C sub -c x=y log",
		"git format-patch -o /tmp HEAD~1", "git format-patch -ko/tmp HEAD~1",
		"git format-patch --output-directory=/tmp HEAD~1", "git format-patch --output-dir /tmp HEAD~1",
		"git grep -O x", "git grep -Ovim x", "git grep -nO x", "git grep --open-files-in-pager=vim x", "git grep --open x",
		"git fetch --upload-pack=x origin", "git ls-remote -u x origin", "git clone -u x a b",
		"git push --receive-pack x", "git archive --remote=x --exec=y HEAD", "git rebase -x make main",
		"FOO=1 git diff --ext-diff", "cd sub && git diff --ext-diff", "/usr/bin/git diff --ext-diff", "$(which git) diff --ext-diff",
	} {
		if CanonicalCommand(c).GitRunsOrWrites() == "" {
			t.Errorf("%s: not named", c)
		}
	}
	for _, c := range []string{
		"git diff", "git diff --stat HEAD~1", "git diff --no-ext-diff --no-textconv", "git diff --text", "git diff -c",
		"git log -O order.txt -p", "git log --exit-code", "git log --output-indicator-new=+", "git log -- --ext-diff",
		"git diff HEAD -- a --output=x", "git status -uall", "git -C sub log", "git blame --contents f.go f.go",
		"git grep -n x", "git log --format=%H --exclude=x", "git diff --color-moved", "echo --ext-diff",
	} {
		if why := CanonicalCommand(c).GitRunsOrWrites(); why != "" {
			t.Errorf("%s: named (%s)", c, why)
		}
	}
}
