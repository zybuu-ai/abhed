package toolset

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/machinegrant"
)

// machineHome is a throwaway home with a kubeconfig and an ~/.ssh/config.
func machineHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", "")
	for path, text := range map[string]string{
		".kube/config": "current-context: lab\nclusters:\n- name: lab\n  cluster:\n    server: https://lab.example:6443\n" +
			"contexts:\n- name: lab\n  context:\n    cluster: lab\n    user: me\nusers:\n- name: me\n  user:\n    token: t\n",
		".ssh/config": "Host box\n  HostName 10.0.0.9\n",
	} {
		p := filepath.Join(home, path)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	grants := filepath.Join(home, ".abhed", "state", "machine-grants.json")
	old := machinegrant.Path
	machinegrant.Path = func() string { return grants }
	t.Cleanup(func() { machinegrant.Path = old })
}

func infraNames(cfg config.Config, machine bool) []string {
	var out []string
	for _, tl := range InfraToolsFor(cfg, nil, func(string, ...any) {}, machine) {
		out = append(out, tl.Name())
	}
	slices.Sort(out)
	return out
}

func TestMachineSetupIsOfferedOnThePersonsOwnSurfaceOnly(t *testing.T) {
	machineHome(t)
	all := []string{"k8s_apply", "k8s_get", "k8s_login", "ssh", "ssh_connect"}
	if got := infraNames(config.Config{}, true); !slices.Equal(got, all) {
		t.Fatalf("the person's own surface offers %v, want %v", got, all)
	}
	// A server runs other people's sessions: never the operator's own setup.
	if got := infraNames(config.Config{}, false); len(got) != 0 {
		t.Fatalf("a server offers %v", got)
	}
	// A file that turns it off, the workspace's included, wins.
	off := config.Config{SetKeys: []string{"k8s.enabled", "ssh.enabled"}}
	if got := infraNames(off, true); len(got) != 0 {
		t.Fatalf("k8s.enabled and ssh.enabled false still offer %v", got)
	}
	// A file that keeps writes off keeps k8s_apply off.
	readOnly := config.Config{SetKeys: []string{"k8s.allow_writes"}}
	if got := infraNames(readOnly, true); slices.Contains(got, "k8s_apply") {
		t.Fatalf("k8s.allow_writes false still offers k8s_apply: %v", got)
	}
	// An organisation's managed configuration decides for it.
	if got := infraNames(config.Config{Managed: true}, true); len(got) != 0 {
		t.Fatalf("under a managed configuration %v", got)
	}
}

func TestNoMachineSetupOffersNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", "")
	old := machinegrant.Path
	machinegrant.Path = func() string { return filepath.Join(home, "g.json") }
	t.Cleanup(func() { machinegrant.Path = old })
	if got := infraNames(config.Config{}, true); len(got) != 0 {
		t.Fatalf("a machine with no kubeconfig or ssh config offers %v", got)
	}
}
