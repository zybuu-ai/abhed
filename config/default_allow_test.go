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
	} {
		args, _ := json.Marshal(map[string]string{"command": command})
		if got := pol.Evaluate("bash", true, args); got.Decision != want {
			t.Errorf("%s: %s (%s), want %s", command, got.Decision, got.Reason, want)
		}
	}
}
