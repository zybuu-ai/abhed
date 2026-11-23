package toolset

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// offSandbox is a process sandbox whose network is off, holding the proxy
// commands would reach a granted host through.
func offSandbox(t *testing.T) sandbox.Sandbox {
	t.Helper()
	pol, err := egress.Compile(egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return sandbox.NewProcess(sandbox.Policy{Workspace: t.TempDir(), MinTier: sandbox.TierProcess, Egress: pol})
}

func runNetwork(t *testing.T, n *Network, args string) (string, bool) {
	t.Helper()
	var a struct{ Scope, Host string }
	_ = json.Unmarshal([]byte(args), &a)
	out, err := n.Open(a.Scope, a.Host, true)
	if err != nil {
		return err.Error(), true
	}
	return out, false
}

// The person opens the network for the workspace, one host, or this chat;
// the workspace and host grants are kept, the chat's is not.
func TestNetworkToolGrants(t *testing.T) {
	type kept struct {
		all  bool
		host string
	}
	var keeps []kept
	n := NewNetwork(config.Default(), offSandbox(t))
	n.Keep = func(all bool, host string) error { keeps = append(keeps, kept{all, host}); return nil }
	pol := n.policy()
	allowed := func(h string) bool {
		return pol.DecideHost(egress.Request{Host: h, Port: 443, Tunnel: true}).Decision == egress.Allow
	}
	if out, bad := runNetwork(t, n, `{"scope":"host","host":"GitHub.com","reason":"clone"}`); bad || !strings.Contains(out, "github.com") {
		t.Fatalf("host: %s", out)
	}
	if !allowed("github.com") || allowed("pypi.org") {
		t.Fatal("the host grant")
	}
	if out, bad := runNetwork(t, n, `{"scope":"once","host":"proxy.golang.org","reason":"go mod download"}`); bad || !strings.Contains(out, "this chat only") {
		t.Fatalf("once: %s", out)
	}
	if !allowed("proxy.golang.org") || len(keeps) != 1 {
		t.Fatalf("once was kept: %v", keeps)
	}
	if out, bad := runNetwork(t, n, `{"scope":"workspace","reason":"install"}`); bad || !strings.Contains(out, "kept for later sessions") {
		t.Fatalf("workspace: %s", out)
	}
	if !allowed("pypi.org") || len(keeps) != 2 || keeps[0] != (kept{false, "github.com"}) || keeps[1] != (kept{true, ""}) {
		t.Fatalf("keeps %v", keeps)
	}
	for _, bad := range []string{`{"scope":"host","reason":"x"}`, `{"scope":"host","host":"*.example.com","reason":"x"}`, `{"scope":"all","reason":"x"}`} {
		if _, isErr := runNetwork(t, n, bad); !isErr {
			t.Errorf("taken: %s", bad)
		}
	}
	if !strings.Contains(n.State(), "opened by the user") {
		t.Fatalf("state: %s", n.State())
	}
}

// Where the managed file holds the network, nothing is offered, nothing kept
// is used, and the tool refuses.
func TestNetworkNotOfferedUnderManagedNetwork(t *testing.T) {
	for _, key := range []string{"sandbox.allow_network", "sandbox.network", "egress"} {
		cfg := config.Default()
		cfg.ManagedKeys = []string{key}
		n := NewNetwork(cfg, offSandbox(t))
		n.Kept = func() KeptNetwork { return KeptNetwork{All: true} }
		n.ApplyKept()
		if n.policy().AnyGranted() {
			t.Fatalf("%s: a kept grant was used", key)
		}
		if out, bad := runNetwork(t, n, `{"scope":"workspace","reason":"x"}`); !bad || !strings.Contains(out, "managed configuration holds the network") {
			t.Fatalf("%s: %s", key, out)
		}
		if ok, _ := NetworkOfferable(cfg); ok {
			t.Fatalf("%s: offerable", key)
		}
	}
	open := config.Default()
	open.Sandbox.AllowNetwork = true
	if ok, why := NetworkOfferable(open); ok || !strings.Contains(why, "open") {
		t.Fatal("offered where the network is open")
	}
}

// Grants kept for the workspace are in force before the first command.
func TestKeptNetworkApplied(t *testing.T) {
	n := NewNetwork(config.Default(), offSandbox(t))
	n.Kept = func() KeptNetwork { return KeptNetwork{Hosts: []string{"registry.npmjs.org"}} }
	b := withNetwork(stubBash(), n, true)
	_ = b.Sandbox(context.Background(), t.TempDir(), "true")
	if n.policy().DecideHost(egress.Request{Host: "registry.npmjs.org", Port: 443, Tunnel: true}).Decision != egress.Allow {
		t.Fatal("the kept host is not open")
	}
	if !strings.Contains(b.NetworkNote, NetworkOfferText) {
		t.Fatalf("note: %q", b.NetworkNote)
	}
}

// The session note says what the session can do, from its state.
func TestSessionNote(t *testing.T) {
	cfg := config.Default()
	cfg.K8s.Clusters = []config.K8sClusterConfig{{Name: "lab", Server: "https://lab.example"}}
	n := NewNetwork(cfg, offSandbox(t))
	note := SessionNote(cfg, n.sb, n)
	for _, want := range []string{"Sandbox: process", "Network: off, only the user can open it", "not brokered", "Clusters: lab", "dispute"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%s", want, note)
		}
	}
	_ = n.policy().GrantHosts("network", []string{"github.com"})
	if note := SessionNote(cfg, n.sb, n); !strings.Contains(note, "off except") || !strings.Contains(note, "github.com") {
		t.Fatalf("after a grant:\n%s", note)
	}
	// Managed network: said, and how it cannot be opened.
	m := config.Default()
	m.ManagedKeys = []string{"egress"}
	mn := NewNetwork(m, offSandbox(t))
	if note := SessionNote(m, mn.sb, mn); !strings.Contains(note, "cannot be opened: the managed configuration") || strings.Contains(note, "only the user can open it") {
		t.Fatalf("managed:\n%s", note)
	}
	// A container on the default image lacks the basic tools.
	t.Setenv("ABHED_SANDBOX_IMAGE", "")
	sandbox.SetConfiguredImage("")
	c := sandbox.NewContainer(sandbox.Policy{Workspace: t.TempDir()})
	if note := SessionNote(cfg, c, NewNetwork(cfg, c)); !strings.Contains(note, "Sandbox: container") || !strings.Contains(note, "Not in image: git, curl") {
		t.Fatalf("container:\n%s", note)
	}
	open := config.Default()
	open.Sandbox.AllowNetwork = true
	on := NewNetwork(open, offSandbox(t))
	if note := SessionNote(open, on.sb, on); !strings.Contains(note, "Network: on.") {
		t.Fatalf("open:\n%s", note)
	}
}

// stubBash is a bash whose commands are never run.
func stubBash() tools.Bash {
	return tools.Bash{Sandbox: func(context.Context, string, string) *exec.Cmd { return exec.Command("true") }}
}
