package policy

import "testing"

// A one-click scope is offered only for a listed tool and subcommand, spelled
// exactly; everything else, however it is spelled, is offered none.
func TestScopeIsOfferedOnlyForListedTools(t *testing.T) {
	e := New(ModeDefault)
	for command, want := range map[string]string{
		"git status":         "bash(git status *)",
		"git commit -m done": "bash(git commit *)",
		"git diff --stat":    "bash(git diff *)",
		"ls -la":             "bash(ls *)",
		"ls src":             "bash(ls *)",
		"ls *.go":            "bash(ls *)",
		"cat README.md":      "bash(cat *)",
		"mkdir -p a/b":       "bash(mkdir *)",
		"npm ls --depth=0":   "bash(npm ls *)",
		"pip3 list":          "bash(pip3 list *)",
		"docker ps -a":       "bash(docker ps *)",

		// The bypasses: other spellings, prefixes, paths and runners.
		"Python3 -m unittest":     "",
		"Bash x.sh":               "",
		"Node x.js":               "",
		"node18 x.js":             "",
		"ruby3.2 x.rb":            "",
		"nodejs x.js":             "",
		"php8.2 x.php":            "",
		"python3-dbg x.py":        "",
		"python3 -m unittest":     "",
		"builtin eval ls":         "",
		"! python3 x.py":          "",
		"coproc python3 x.py":     "",
		"/usr/bin/python3 x.py":   "",
		"/usr/bin/git status":     "",
		"Git status":              "",
		"git2 status":             "",
		"mytool -ec ls":           "",
		"go -C . run x.go":        "",
		"npm --prefix . exec x":   "",
		"arch -arm64 python3 x":   "",
		"make test":               "",
		"npm test":                "",
		"npm install vitest":      "",
		"docker run alpine":       "",
		"kubectl exec pod -- sh":  "",
		"kubectl get pods":        "",
		"cargo build":             "",
		"go test ./...":           "",
		"go fmt ./...":            "",
		"go mod tidy":             "",
		"go version":              "",
		"go run .":                "",
		"git -c alias.x=!sh x":    "",
		"git config alias.x !sh":  "",
		"git fetch --upload-pack": "",
		"git":                     "",
		"env git status":          "",
		"sort --compress-program": "",
		"rg --pre sh x":           "",

		// Code flags, and words the shell would rewrite.
		"grep -e x f":      "",
		"grep -ce x f":     "",
		"cat --exec=x":     "",
		"git \"status\"":   "",
		"'git' status":     "",
		"\\git status":     "",
		"$'git' status":    "",
		"FOO=* git status": "",
		"git {status,log}": "",
		"FOO=1":            "",
		"git status; rm x": "",
		// Removed from the list, and any assignment in front.
		"git remote update":            "",
		"git remote add -f x zzz::bar": "",
		"yarn list":                    "",
		"yarn outdated":                "",
		"pnpm ls":                      "",
		"pnpm outdated":                "",
		"GIT_PAGER=cat git log -n 3":   "",
		"LD_PRELOAD=/tmp/x.so ls":      "",
		"PAGER=sh git log":             "",
		"wc -c f":                      "",
		"git status $(id)":             "",
	} {
		res := e.Evaluate("bash", true, args(map[string]string{"command": command}))
		if res.Scope != want {
			t.Errorf("%q: scope %q, want %q", command, res.Scope, want)
		}
	}
}

// restore, checkout and stash, and programs that can delete or write over
// work, get no subcommand-wide scope; stash offers only its read-only forms.
func TestNoScopeReachesAFormThatDiscardsWork(t *testing.T) {
	e := New(ModeDefault)
	for command, want := range map[string]string{
		"git restore --staged Makefile": "",
		"git checkout main":             "",
		"git stash":                     "",
		"git stash push -m wip":         "",
		"git stash pop":                 "",
		"git stash list":                "bash(git stash list *)",
		"git stash show -p":             "bash(git stash show *)",
		"mv a b":                        "",
		"cp a b":                        "",
		"uniq in out":                   "",
		"tree":                          "",
		"git branch":                    "bash(git branch *)",
		"git tag":                       "bash(git tag *)",
	} {
		if got := e.Evaluate("bash", true, args(map[string]string{"command": command})).Offer(); got != want {
			t.Errorf("%q: scope %q, want %q", command, got, want)
		}
	}
}

// A session scope must never approve a command the destructive check catches:
// with every scope Abhed offers, or ever offered, allowed as a rule, each
// discarding spelling still confirms, in every mode, and offers no scope.
func TestNoScopeApprovesADestructiveCommand(t *testing.T) {
	scopes := []string{
		"bash(git restore *)", "bash(git checkout *)", "bash(git stash *)", "bash(git switch *)",
		"bash(git branch *)", "bash(git tag *)", "bash(git log *)", "bash(git diff *)", "bash(git show *)",
		"bash(git stash show *)", "bash(mv *)", "bash(cp *)",
	}
	for _, command := range []string{"git status", "git log -3", "git diff x", "git show HEAD", "git branch -a",
		"git tag v1", "git switch main", "git stash list", "git stash show", "mkdir a", "ls"} {
		if s := New(ModeDefault).Evaluate("bash", true, args(map[string]string{"command": command})).Offer(); s != "" {
			scopes = append(scopes, s)
		}
	}
	destructive := []string{
		"git restore .", "git restore :/", "git restore README.md", "git restore --worktree .",
		"git restore --staged --worktree .", "git checkout .", "git checkout -- .", "git checkout HEAD -- Makefile",
		"git checkout -f", "git switch --discard-changes main", "git stash drop", "git stash clear",
		"git branch -D topic", "git branch -d topic", "git tag -d v1", "git clean -fd", "git clean",
		"git reset --hard", "git log --output=README.md", "git stash show --output=x",
		// Shortened long options, which git accepts, and values in a short cluster.
		"git switch --discard main", "git switch --forc main", "git branch --del topic",
		"git branch --dele topic", "git branch --del --forc topic", "git tag --del v1", "git tag --forc v1",
		"git restore --staged --work .", "git reset --har", "git restore -sSTABLE .", "git clean -en",
		"git branch -f main HEAD~3", "git branch $'-D' topic",
		// An operand named git, and --output on stash list.
		"git branch topic git -D", "git tag v1 git -d", "git stash list -p --output=README.md",
	}
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits, ModeAuto, ModeBypass} {
		e := New(mode)
		if err := e.AddAllow(scopes...); err != nil {
			t.Fatal(err)
		}
		for _, command := range destructive {
			res := e.Evaluate("bash", true, args(map[string]string{"command": command}))
			if res.Decision != Ask || res.Step != "destructive" || res.Offer() != "" {
				t.Errorf("%s: %q = %s at %s, scope %q; want a destructive confirmation", mode, command, res.Decision, res.Step, res.Offer())
			}
		}
	}
}

// An ask rule prompts every time, so it offers no scope a person could
// remember, whatever the command, and no Result but a default ask offers one.
func TestAskRuleOffersNoScope(t *testing.T) {
	e := New(ModeDefault)
	if err := e.AddAsk("bash(git tag*)", "web_search"); err != nil {
		t.Fatal(err)
	}
	for tool, a := range map[string]map[string]string{
		"bash":       {"command": "git tag v1"},
		"web_search": {"query": "abhed"},
	} {
		res := e.Evaluate(tool, true, args(a))
		if res.Step != "ask" || res.Scope != "" || res.Offer() != "" {
			t.Errorf("%s: step %s scope %q offer %q, want an ask with no scope", tool, res.Step, res.Scope, res.Offer())
		}
	}
	for _, step := range []string{"ask", "destructive", "screen", "hook", "monitor", "mode", "deny"} {
		if got := (Result{Decision: Ask, Step: step, Scope: "bash(ls *)"}).Offer(); got != "" {
			t.Errorf("step %s offers %q", step, got)
		}
	}
	if got := (Result{Decision: Ask, Step: "default", Scope: "bash(ls *)"}).Offer(); got != "bash(ls *)" {
		t.Errorf("a default ask offers %q", got)
	}
}
