package hostgit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// C4: with a person's pre-push hook, the push's command-line pins are lost
// (a second GIT_CONFIG_COUNT), so the repository's own core.sshCommand and
// credential helper are used by host git.
func TestR3PushWithHookLosesPins(t *testing.T) {
	r := newRemoteRig(t)
	hooks := filepath.Join(r.home, "hooks")
	os.MkdirAll(hooks, 0o755)
	os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	run(t, r.home, "config", "--global", "core.hooksPath", hooks)
	planted := filepath.Join(r.home, "repo-ssh-ran")
	// present before the session (an earlier unprotected run, or any repo the person opens)
	b := r.broker(Network{Open: true})
	r.pushReady(t, b)
	run(t, r.ws, "config", "core.sshCommand", "touch "+planted+"; false")
	b = r.broker(Network{Open: true})
	cmd := b.Command(r.ctx("c-push"), r.ws, "git push origin main")
	if cmd.Err == nil {
		n := 0
		for _, kv := range cmd.Env {
			if strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") {
				n++
				t.Logf("env: %s", kv)
			}
		}
	}
	out, err := runCmd(t, cmd)
	t.Logf("push: %v\n%s", err, out)
	if _, err := os.Stat(planted); err == nil {
		t.Errorf("THE REPOSITORY'S core.sshCommand RAN ON THE HOST FOR A PUSH")
	}
}
