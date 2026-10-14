package config

import (
	"encoding/json"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// The built-in allow rules name whole commands: ls and git diff with any
// arguments, not every program whose name starts the same (lsof, git difftool).
func TestDefaultAllowNamesWholeCommands(t *testing.T) {
	ws := t.TempDir()
	pol := policy.New(policy.ModeDefault)
	if err := pol.AddAllow(Default().Permissions.Allow...); err != nil {
		t.Fatal(err)
	}
	pol.Roots = func() []string { return []string{ws} }
	pol.Cwd = func() string { return ws }
	for command, want := range map[string]policy.Decision{
		"ls": policy.Allow, "ls -la": policy.Allow, "git status": policy.Allow, "git diff HEAD~1": policy.Allow, "git log --oneline": policy.Allow,
		"lsof -i": policy.Ask, "lsblk": policy.Ask, "git difftool --tool=x": policy.Ask, "git statusx": policy.Ask, "git logx": policy.Ask,
		// Options that write a file or run a program, in each form.
		"git diff --output=x": policy.Ask, "git diff --output x": policy.Ask, "git diff --outp=x": policy.Ask, "git diff a.go --output=x": policy.Ask,
		"git log --output=x": policy.Ask, "git diff --ext-diff": policy.Ask, "git diff --ext": policy.Ask, "git diff a.go --ext-diff": policy.Ask,
		"git log -p --ext-diff": policy.Ask, "git log -p --textconv": policy.Ask, "git diff --textc": policy.Ask,
		"git diff --no-ext-diff --no-textconv": policy.Allow, "git diff --text": policy.Allow, "git log -O order.txt": policy.Allow,
	} {
		args, _ := json.Marshal(map[string]string{"command": command})
		if got := pol.Evaluate("bash", true, args); got.Decision != want {
			t.Errorf("%s: %s (%s), want %s", command, got.Decision, got.Reason, want)
		}
	}
}
