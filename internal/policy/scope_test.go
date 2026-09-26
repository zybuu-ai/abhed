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
