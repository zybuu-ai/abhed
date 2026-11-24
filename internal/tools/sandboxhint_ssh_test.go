package tools

import (
	"strings"
	"testing"
)

// A command refused ~/.ssh is told git's remote commands go through the
// user's own git when run alone, not to reach for an ssh tool or ssh.hosts.
func TestSSHHintNamesTheHostGitBroker(t *testing.T) {
	kind, hint := sandboxHint("ssh: Can't open user config file /Users/x/.ssh/config: Operation not permitted", true)
	if kind != "ssh" || !strings.Contains(hint, "through the user's own git on the host") || !strings.Contains(hint, "not after && or ;") ||
		strings.Contains(hint, "ssh.hosts") {
		t.Fatalf("%s: %s", kind, hint)
	}
}

// Where git's remote commands do not go through the user's own git (serve,
// a container tier), the note does not tell the model to run them alone to
// reach it: it says they cannot authenticate here.
func TestSSHHintWithoutHostGit(t *testing.T) {
	kind, hint := sandboxHint("ssh: Can't open user config file /Users/x/.ssh/config: Operation not permitted", false)
	if kind != "ssh" || strings.Contains(hint, "run them so") || strings.Contains(hint, "on the host") ||
		!strings.Contains(hint, "do not go through the user's own git here") {
		t.Fatalf("%s: %s", kind, hint)
	}
}
