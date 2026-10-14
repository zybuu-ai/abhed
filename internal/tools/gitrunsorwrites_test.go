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
		"git -C sub log", "git -C sub diff", "git -p log", "git --paginate diff", "git --no-pager -p log",
		"git --git-dir=/x/.git log", "git --git-dir /x/.git log", "git --work-tree=/x diff", "git --work-tree /x diff",
		// A -- taken as an option's value does not end a parse-options subcommand.
		"git grep -e -- -O/path/prog", "git grep -e -- --open-files-in-pager=prog x", "git grep x -- -O",
		"git format-patch --subject-prefix -- -o /dir -1 HEAD", "git format-patch -- -o /dir", "git format-patch --subject-prefix -- --output-directory=/dir -1",
		"git rebase --onto -- -x make main",
		// diff --no-index reads -- as an option's value too.
		"git diff --no-index -O -- --output=/x a b", "git diff --no-index -O -- --outp=/x a b", "git diff -- --ext-diff",
		// Any prefix git accepts.
		"git grep --op=prog hello", "git rebase --ex 'prog rebase' HEAD~1", "git clone --u=prog file://repo dst", "git ls-remote --u=prog file://repo",
		// Configuration and hooks set on the clone or init line.
		"git clone -c core.sshCommand=prog ssh://x/y", "git clone -qc x=y a b", "git clone --config core.sshCommand=prog a b",
		"git clone --config=x=y a b", "git clone --template=/dir a b", "git clone --temp /dir a b", "git init --template=/dir", "git init --templ /dir x",
		// Subcommands that run a program.
		"git difftool -x evil", "git difftool --extcmd=evil", "git difftool HEAD", "git mergetool",
		"git filter-branch --tree-filter evil", "git filter-branch HEAD", "git send-email --to-cmd=evil x.patch", "git send-email --smtp-server=/tmp/x x.patch",
		"git bisect run evil", "git submodule foreach evil", "git submodule --quiet foreach evil",
		"git config core.pager evil", "git config --add x.y z", "git config -e", "git config --edit", "git config --get x --unset y",
		"git config set core.pager evil", "git config --unset core.pager", "git config --global alias.x '!sh'",
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
		"git diff -- a.go", "git diff HEAD -- a.go b.go", "git status -uall", "git log -p", "git diff --work-tree-x", "git blame --contents f.go f.go",
		"git grep -n x", "git log -p --grep -- --ext-diff", "git show HEAD -- -O", "git diff --text", "git init", "git clone a b", "git log --grep -- --ext-diff",
		"git bisect start", "git bisect good", "git submodule status", "git config --get core.pager", "git config -l",
		"git config --list --show-origin", "git config get core.pager", "git config list", "git config --get-regexp alias",
		"git log --format=%H --exclude=x", "git diff --color-moved", "echo --ext-diff",
	} {
		if why := CanonicalCommand(c).GitRunsOrWrites(); why != "" {
			t.Errorf("%s: named (%s)", c, why)
		}
	}
}
