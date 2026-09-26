package tools

import (
	"strings"
	"testing"
	"time"
)

// Every git spelling that throws away work with no undo confirms; the
// everyday forms beside them do not.
func TestGitDiscardsAreDestructive(t *testing.T) {
	for cmd, want := range map[string]bool{
		// restore: anything that touches the working tree.
		"git restore .":                     true,
		"git restore :/":                    true,
		"git restore README.md":             true,
		"git restore -- src/":               true,
		"git restore --worktree .":          true,
		"git restore -W x":                  true,
		"git restore --staged --worktree .": true,
		"git restore -SW .":                 true,
		"git restore --source=HEAD~2 x":     true,
		"git restore --staged Makefile":     false,
		"git restore -S .":                  false,

		// checkout: a pathspec, a forced switch, or two operands.
		"git checkout .":                      true,
		"git checkout -- .":                   true,
		"git checkout -- README.md":           true,
		"git checkout HEAD -- Makefile":       true,
		"git checkout HEAD Makefile":          true,
		"git checkout ./src":                  true,
		"git checkout :/":                     true,
		"git checkout '*.go'":                 true,
		"git checkout -f":                     true,
		"git checkout --force main":           true,
		"git checkout --ours x.go":            true,
		"git checkout --pathspec-from-file=f": true,
		"git checkout main":                   false,
		"git checkout origin/main":            false,
		"git checkout -b feature":             false,
		"git checkout -b feature origin/x":    false,
		"git checkout -B feature main":        false,

		"git switch --discard-changes main": true,
		"git switch -f main":                true,
		"git switch main":                   false,
		"git switch -c topic":               false,

		"git stash drop":           true,
		"git stash drop stash@{1}": true,
		"git stash clear":          true,
		"git stash":                false,
		"git stash list":           false,
		"git stash pop":            false,
		"git stash push -m drop":   false,

		"git branch -d topic":       true,
		"git branch -D topic":       true,
		"git branch --delete topic": true,
		"git branch -M old main":    true,
		"git branch":                false,
		"git branch -a":             false,
		"git branch topic":          false,
		"git tag -d v1":             true,
		"git tag --delete v1":       true,
		"git tag -f v1":             true,
		"git tag v1":                false,
		"git tag -a v1 -m released": false,
		"git tag -l":                false,

		"git clean":              true,
		"git clean -fd":          true,
		"git clean -x":           true,
		"git clean -n":           false,
		"git clean --dry-run -d": false,

		"git reset --hard":        true,
		"git -C sub reset --hard": true,
		"git reset --soft HEAD~1": false,
		"git reset HEAD x":        false,

		"git push -f":                  true,
		"git push origin +main":        true,
		"git push origin :old":         true,
		"git push --delete origin old": true,
		"git push origin main":         false,

		"git log --output=README.md": true,
		"git diff --output x":        true,
		"git show --output=a HEAD":   true,
		"git stash show --output=a":  true,
		"git log --oneline":          false,
		"git diff --stat":            false,

		// Long options shortened, as git accepts, and values inside a short cluster.
		"git switch --discard main":             true,
		"git switch --forc main":                true,
		"git branch --del topic":                true,
		"git branch --dele topic":               true,
		"git branch --del --forc topic":         true,
		"git tag --del v1":                      true,
		"git tag --forc v1":                     true,
		"git restore --staged --work .":         true,
		"git restore --stag .":                  true,
		"git restore -sSTABLE .":                true,
		"git restore -s STABLE --staged x":      false,
		"git reset --har":                       true,
		"git -c clean.requireForce=0 clean -en": true,
		"git clean -e -n":                       true,
		"git clean --dry":                       true,
		"git log --outp=x":                      true,
		"git tag -mdelete v1":                   false,
		"git branch --format=%(refname)":        false,
		"git switch --detach HEAD":              false,

		// Resetting a branch, and a worktree removed with its changes.
		"git branch -f main HEAD~3":      true,
		"git branch --force main HEAD~3": true,
		"git worktree remove --force wt": true,
		"git worktree remove -f wt":      true,
		"git worktree remove wt":         false,
		"git worktree add wt":            false,

		// An operand named git, and a first git that is not the command.
		"git branch topic git -D":              true,
		"git tag v1 git -d":                    true,
		"git branch topic ./git -D":            true,
		"git tag v1 x/git --del":               true,
		"sudo -u git git stash drop":           true,
		"git stash list -p --output=README.md": true,
		"git stash list --outp README.md":      true,
		"git stash list -p":                    false,

		// A value-taking option takes the next word, whatever it looks like.
		"git tag -a v1 --message -d": false,
		"git branch --sort -D":       false,

		// A later --no- form takes a safe flag back.
		"git clean -n --no-dry-run":          true,
		"git clean -n --no-dry":              true,
		"git restore --staged --no-staged x": true,

		// Other spellings of the same call.
		"git branch $'-D' topic":         true,
		"git branch $\"--delete\" topic": true,
		"git -C sub restore .":           true,
		"git --no-pager checkout .":      true,
		"git -c core.x=y stash drop":     true,
		"cd sub && git restore .":        true,
		"git status; git checkout .":     true,
		"/usr/bin/git stash clear":       true,
		"'git' restore .":                true,
		"env GIT_DIR=.git git restore .": true,
		"sh -c 'git stash drop'":         true,
		"echo $(git branch -D x)":        true,
		"git status":                     false,
		"grep git README.md":             false,
		"git -C restore status":          false,
	} {
		what, got := IsDestructive(cmd)
		if got != want {
			t.Errorf("IsDestructive(%q) = %v (%s), want %v", cmd, got, what, want)
		}
	}
}

// The check reads a line once: a long one full of git words stays fast.
func TestGitDestructiveIsLinear(t *testing.T) {
	// Growth, not a wall-clock budget: CI runs with -race and coverage.
	fastest := func(line string) time.Duration {
		best := time.Duration(1 << 62)
		for range 3 {
			start := time.Now()
			if _, ok := IsDestructive(line); !ok {
				t.Fatalf("a %d-byte line was not caught", len(line))
			}
			if took := time.Since(start); took < best {
				best = took
			}
		}
		return best
	}
	for _, shape := range []func(n int) string{
		func(n int) string { return strings.Repeat("git x ", n) + "git stash drop" },
		func(n int) string { return strings.Repeat("git x; ", n) + "git stash drop" },
		func(n int) string { return "git log " + strings.Repeat("x ", n) + "--output=x" },
	} {
		small, large := fastest(shape(10000)), fastest(shape(40000))
		if large > 10*small+50*time.Millisecond {
			t.Fatalf("4x the input took %s against %s: not linear", large, small)
		}
	}
	// Up to the cap, each git is read on its own merits.
	if what, ok := IsDestructive(strings.Repeat("git status; ", 20) + strings.Repeat("git log ", 16)); ok {
		t.Fatalf("harmless git commands read as destructive: %s", what)
	}
}
