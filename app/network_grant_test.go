package app

import (
	"path/filepath"
	"testing"
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
