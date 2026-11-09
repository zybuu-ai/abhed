package toolset

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/remote"
	"github.com/zybuu-ai/abhed/internal/tools"
)

func sshArgs(host, command string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"host": host, "command": command})
	return b
}

// RemoteReads, which the CLI, the server and the SDK all call, gives the
// engine the operator's reads, the classifier's hosts and the contents opt-in.
func TestRemoteReadsWiresTheClassifier(t *testing.T) {
	cfg := config.Default()
	cfg.SSH.Enabled = true
	cfg.SSH.Hosts = []config.SSHHostConfig{{Name: "app1", Addr: "10.0.0.1", User: "ops"}, {Name: "half", Addr: "10.0.0.2"}}
	cfg.SSH.ReadCommands = []string{"uptime -p"}
	pol := policy.New(policy.ModeDefault)
	if err := RemoteReads(pol, cfg); err != nil {
		t.Fatal(err)
	}
	for cmd, want := range map[string]policy.Decision{"uptime -p": policy.Allow, "ls -lah /var/log": policy.Allow, "cat /etc/os-release": policy.Ask} {
		if res := pol.Evaluate("ssh", true, sshArgs("app1", cmd)); res.Decision != want {
			t.Errorf("%q: %+v", cmd, res)
		}
	}
	if res := pol.Evaluate("ssh", true, sshArgs("half", "ls")); res.Decision != policy.Ask {
		t.Errorf("a host that does not load was trusted: %+v", res)
	}
	cfg.SSH.ReadContents = true
	pol = policy.New(policy.ModeDefault)
	if err := RemoteReads(pol, cfg); err != nil || !pol.RemoteContents {
		t.Fatalf("read_contents not wired: %v", err)
	}
	cfg.SSH.ListedReadsOnly = true
	pol = policy.New(policy.ModeDefault)
	if err := RemoteReads(pol, cfg); err != nil || pol.RemoteHosts != nil || pol.RemoteContents {
		t.Fatalf("listed_reads_only did not turn it off: %v", err)
	}

	// The tools: ssh_connect keeps every declared name, loaded or not.
	cfg.SSH.ListedReadsOnly = false
	var connect remote.ConnectTool
	for _, tl := range InfraTools(cfg, nil, func(string, ...any) {}) {
		if c, ok := tl.(remote.ConnectTool); ok {
			connect = c
		}
		if s, ok := tl.(remote.Tool); ok && (!s.ReadOnly || !s.ReadContents) {
			t.Errorf("the ssh tool is not told of the classifier: %+v", s)
		}
	}
	if !slices.Equal(connect.Declared, []string{"app1", "half"}) {
		t.Errorf("ssh_connect reserves %v", connect.Declared)
	}
	var _ tools.Tool = connect
}
