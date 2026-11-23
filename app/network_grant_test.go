package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// What the person opens to a workspace's commands is kept beside its bash
// grants, once each, and forgotten on revoke without touching them.
func TestKeptNetworkStore(t *testing.T) {
	old := grantsFile
	file := filepath.Join(t.TempDir(), "workspace-grants.json")
	grantsFile = func() string { return file }
	t.Cleanup(func() { grantsFile = old })
	ws, other := t.TempDir(), t.TempDir()
	if k := keptNetwork(ws); k.All || len(k.Hosts) != 0 {
		t.Fatalf("kept before any grant: %+v", k)
	}
	for _, h := range []string{"github.com", "github.com", "pypi.org"} {
		if err := keepNetwork(ws, false, h); err != nil {
			t.Fatal(err)
		}
	}
	if err := keepNetwork(ws, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := keepGrant(ws, bashGrant{Command: "go test"}); err != nil {
		t.Fatal(err)
	}
	k := keptNetwork(ws)
	if !k.All || len(k.Hosts) != 2 || networkScope(k) != "network(any host), network(github.com), network(pypi.org)" {
		t.Fatalf("kept %+v (%s)", k, networkScope(k))
	}
	if k := keptNetwork(other); k.All || len(k.Hosts) != 0 {
		t.Fatalf("another workspace got it: %+v", k)
	}
	if err := forgetNetwork(ws); err != nil {
		t.Fatal(err)
	}
	if k := keptNetwork(ws); k.All || len(k.Hosts) != 0 || len(workspaceGrants(ws)) != 1 {
		t.Fatalf("after forgetting: %+v, %d bash grants", k, len(workspaceGrants(ws)))
	}
}

// At the terminal the person opens the network with /network, asked first;
// no answer opens nothing, and a managed network offers nothing.
func TestSlashNetwork(t *testing.T) {
	old := grantsFile
	file := filepath.Join(t.TempDir(), "workspace-grants.json")
	grantsFile = func() string { return file }
	t.Cleanup(func() { grantsFile = old })
	pol, err := egress.Compile(egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	sb := sandbox.NewProcess(sandbox.Policy{Workspace: ws, MinTier: sandbox.TierProcess, Egress: pol})
	cfg := config.Default()
	n := toolset.NewNetwork(cfg, sb)
	n.Keep = func(all bool, host string) error { return keepNetwork(ws, all, host) }
	reach := func(h string) bool {
		return pol.DecideHost(egress.Request{Host: h, Port: 443, Tunnel: true}).Decision == egress.Allow
	}
	e, s := configEnv(cfg, "")
	e.st.set = &toolset.Set{Network: n}
	if _, err := slashNetwork(context.Background(), e, []string{"host", "github.com"}); err == nil || s.dialogs != 1 || reach("github.com") {
		t.Fatalf("opened with no answer: %v", err)
	}
	e, _ = configEnv(cfg, ui.ChoiceYes)
	e.st.set = &toolset.Set{Network: n}
	if _, err := slashNetwork(context.Background(), e, []string{"host"}); err == nil {
		t.Fatal("host with no name was taken")
	}
	if _, err := slashNetwork(context.Background(), e, []string{"host", "github.com"}); err != nil || !reach("github.com") || reach("pypi.org") {
		t.Fatalf("host: %v", err)
	}
	if k := keptNetwork(ws); len(k.Hosts) != 1 {
		t.Fatalf("kept %+v", k)
	}
	m := config.Default()
	m.ManagedKeys = []string{"egress"}
	e, _ = configEnv(m, ui.ChoiceYes)
	e.st.set = &toolset.Set{Network: toolset.NewNetwork(m, sb)}
	if _, err := slashNetwork(context.Background(), e, []string{"workspace"}); err == nil || !strings.Contains(err.Error(), "managed") || reach("pypi.org") {
		t.Fatalf("managed: %v", err)
	}
}
