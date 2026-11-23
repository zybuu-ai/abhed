package toolset

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/machinegrant"
	"github.com/zybuu-ai/abhed/internal/tools"
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

func TestWithNothingToReachOnlyK8sLoginIsOffered(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", "")
	old := machinegrant.Path
	machinegrant.Path = func() string { return filepath.Join(home, "g.json") }
	t.Cleanup(func() { machinegrant.Path = old })
	offered := func(machine bool) []string {
		reg := tools.NewRegistry(InfraToolsFor(config.Config{}, nil, func(string, ...any) {}, machine)...)
		var out []string
		for _, d := range reg.Definitions() {
			out = append(out, d.Name)
		}
		slices.Sort(out)
		return out
	}
	// A pasted `oc login` must work on any machine: k8s_login is registered,
	// offered once a paste is held, and the reads and writes wait for a cluster.
	if got := offered(true); len(got) != 0 {
		t.Fatalf("a machine with nothing to reach and nothing pasted offers %v", got)
	}
	reg := tools.NewRegistry(InfraToolsFor(config.Config{}, nil, func(string, ...any) {}, true)...)
	for _, tl := range reg.All() {
		if n, ok := tl.(tools.PasteNoticer); ok {
			n.NotePasted()
		}
	}
	var pasted []string
	for _, d := range reg.Definitions() {
		pasted = append(pasted, d.Name)
	}
	if !slices.Equal(pasted, []string{"k8s_login"}) {
		t.Fatalf("once a login is pasted it offers %v", pasted)
	}
	if got := offered(false); len(got) != 0 {
		t.Fatalf("a server offers %v", got)
	}
	// Turned off in a file, not even k8s_login.
	if got := infraNames(config.Config{SetKeys: []string{"k8s.enabled"}}, true); len(got) != 0 {
		t.Fatalf("k8s.enabled false offers %v", got)
	}
	// Once a login is kept, the cluster tools are offered.
	if err := machinegrant.Keep(machinegrant.Grant{Kind: machinegrant.KindK8sLogin, Name: "lab", Identity: "i",
		Login: &machinegrant.Login{Server: "https://lab.example:6443", Secret: "K8S_LAB_TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	if got := offered(true); !slices.Equal(got, []string{"k8s_apply", "k8s_get", "k8s_login"}) {
		t.Fatalf("with a kept login %v", got)
	}
}

// Under a managed configuration only the organisation's clusters take a
// token: the person's kept logins are not offered.
func TestManagedConfigOffersNoKeptLogin(t *testing.T) {
	machineHome(t)
	if err := machinegrant.Keep(machinegrant.Grant{Kind: machinegrant.KindK8sLogin, Name: "personal", Identity: "i",
		Login: &machinegrant.Login{Server: "https://personal.example:6443", Secret: "K8S_PERSONAL_TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Managed: true}
	cfg.K8s.Enabled = true
	for _, tl := range InfraToolsFor(cfg, nil, func(string, ...any) {}, true) {
		if strings.Contains(tl.Description(), "personal") {
			t.Fatalf("%s offers a kept login under a managed configuration: %s", tl.Name(), tl.Description())
		}
	}
}

// Each session's tool set is built on its own (an ACP session builds its
// agent, and so its tools, when it opens): a login kept in one session is
// offered by the next one built. A registry cloned within a session, for
// its subagents, keeps that session's view.
func TestTheNextSessionSeesAKeptLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", "")
	old := machinegrant.Path
	machinegrant.Path = func() string { return filepath.Join(home, "g.json") }
	t.Cleanup(func() { machinegrant.Path = old })
	build := func() *tools.Registry {
		return tools.NewRegistry(InfraToolsFor(config.Config{}, nil, func(string, ...any) {}, true)...)
	}
	names := func(r *tools.Registry) []string {
		var out []string
		for _, d := range r.Definitions() {
			out = append(out, d.Name)
		}
		slices.Sort(out)
		return out
	}
	first := build()
	sub := first.Clone()
	if err := machinegrant.Keep(machinegrant.Grant{Kind: machinegrant.KindK8sLogin, Name: "lab", Identity: "i",
		Login: &machinegrant.Login{Server: "https://lab.example:6443", Secret: "K8S_LAB_TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	if got := names(first); len(got) != 0 {
		t.Fatalf("the open session's tools changed: %v", got)
	}
	if got := names(sub); len(got) != 0 {
		t.Fatalf("its subagents' copy changed: %v", got)
	}
	next := build()
	if got := names(next); !slices.Equal(got, []string{"k8s_apply", "k8s_get", "k8s_login"}) {
		t.Fatalf("the next session offers %v", got)
	}
	for _, d := range next.Definitions() {
		if d.Name == "k8s_login" && !strings.Contains(d.Description, "lab") {
			t.Fatalf("the next session's k8s_login does not name the kept login: %s", d.Description)
		}
	}
}
