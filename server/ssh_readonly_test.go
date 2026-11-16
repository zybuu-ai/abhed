package server

import (
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// A server session's policy runs the read-only classifier on the hosts the
// operator declared, and asks for file contents unless read_contents is set.
func TestServerPolicyClassifiesRemoteReads(t *testing.T) {
	cfg := config.Default()
	cfg.SSH.Enabled = true
	cfg.SSH.Hosts = []config.SSHHostConfig{{Name: "app1", Addr: "10.0.0.1", User: "ops"}}
	s := &Server{opts: Options{Config: cfg, Workspace: t.TempDir()}}
	call := func(cmd string) []byte { return []byte(`{"host":"app1","command":"` + cmd + `"}`) }
	if got := s.newPolicy(policy.ModePlan).Evaluate("ssh", true, call("ls -lah /var/log")); got.Decision != policy.Allow || got.Rule != policy.ReadOnlyRule {
		t.Fatalf("metadata read: %+v", got)
	}
	if got := s.newPolicy(policy.ModeDefault).Evaluate("ssh", true, call("cat /etc/os-release")); got.Decision != policy.Ask {
		t.Fatalf("content read without read_contents: %+v", got)
	}
	s.opts.Config.SSH.ReadContents = true
	if got := s.newPolicy(policy.ModeDefault).Evaluate("ssh", true, call("cat /etc/os-release")); got.Decision != policy.Allow {
		t.Fatalf("content read with read_contents: %+v", got)
	}
}
