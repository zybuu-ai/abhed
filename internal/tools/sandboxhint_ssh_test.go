package tools

import (
	"strings"
	"testing"
)

// A command refused ~/.ssh is told git's remote commands go through the
// user's own git when run alone, not to reach for an ssh tool or ssh.hosts.
func TestSSHHintNamesTheHostGitBroker(t *testing.T) {
	kind, hint := sandboxHint("ssh: Can't open user config file /Users/x/.ssh/config: Operation not permitted")
	if kind != "ssh" || !strings.Contains(hint, "through the user's own git on the host") || !strings.Contains(hint, "not after && or ;") ||
		strings.Contains(hint, "ssh.hosts") {
		t.Fatalf("%s: %s", kind, hint)
	}
}
