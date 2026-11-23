package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/machinegrant"
	"github.com/zybuu-ai/abhed/internal/secrets"
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
	// An organisation's managed configuration that sets k8s and ssh decides
	// for it: only a k8s_login that says why it refuses is left.
	org := config.Config{Managed: true, ManagedKeys: []string{"k8s.enabled", "ssh.enabled"}, SetKeys: []string{"k8s.enabled", "ssh.enabled"}}
	if got := infraNames(org, true); !slices.Equal(got, []string{"k8s_login"}) {
		t.Fatalf("under a managed configuration setting k8s and ssh: %v", got)
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
	cfg := config.Config{Managed: true, ManagedKeys: []string{"k8s.enabled"}, SetKeys: []string{"k8s.enabled"}}
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

// A managed file that sets other things, such as web search, leaves the
// person's machine setup alone, kept logins included.
func TestAManagedFileThatSetsNoK8sOrSSHKeepsMachineSetup(t *testing.T) {
	machineHome(t)
	cfg := config.Config{Managed: true, ManagedKeys: []string{"web_search.enabled"}, SetKeys: []string{"web_search.enabled"}}
	cfg.WebSearch.Enabled = true
	// Offered to the model, not only registered: the kubeconfig is in use.
	reg := tools.NewRegistry(InfraToolsFor(cfg, nil, func(string, ...any) {}, true)...)
	var got []string
	for _, d := range reg.Definitions() {
		got = append(got, d.Name)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"k8s_apply", "k8s_get", "k8s_login", "ssh", "ssh_connect"}) {
		t.Fatalf("under a web-search-only managed file the model is offered %v", got)
	}
}

// A managed configuration that sets k8s turns the kubeconfig and pasted
// logins off, and a pasted login is refused with the reason, never as an
// unknown tool; ssh, which it does not set, stays.
func TestAManagedK8sSettingTurnsMachineK8sOffWithAReason(t *testing.T) {
	machineHome(t)
	cfg := config.Config{Managed: true, ManagedKeys: []string{"k8s.enabled"}, SetKeys: []string{"k8s.enabled"}}
	ts := InfraToolsFor(cfg, nil, func(string, ...any) {}, true)
	var names []string
	var login tools.Tool
	for _, tl := range ts {
		names = append(names, tl.Name())
		if tl.Name() == "k8s_login" {
			login = tl
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"k8s_login", "ssh", "ssh_connect"}) || login == nil {
		t.Fatalf("under a managed k8s.enabled false: %v", names)
	}
	err := tools.Precheck(context.Background(), login, nil, json.RawMessage(`{"cluster":"lab","pasted":"P1"}`))
	if err == nil || !strings.Contains(err.Error(), "organisation") || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("the pasted login's refusal: %v", err)
	}
	// machine_setup false turns both off.
	off := false
	cfg = config.Config{Managed: true, ManagedKeys: []string{"machine_setup"}, MachineSetup: &off}
	if got := infraNames(cfg, true); !slices.Equal(got, []string{"k8s_login"}) {
		t.Fatalf("under a managed machine_setup false: %v", got)
	}
}

// k8s_login names only the secrets the model may be offered: never a model
// key or another name Abhed keeps for itself.
func TestK8sLoginListsOnlyOfferedSecrets(t *testing.T) {
	machineHome(t)
	vault := secrets.Open(filepath.Join(t.TempDir(), "s.json"))
	for name, v := range map[string]string{"DB_PASSWORD": "x", "MODEL_KEY_ANTHROPIC_API_KEY": "sk-y"} {
		if strings.HasPrefix(name, "MODEL_KEY_") {
			if err := vault.SetHeld(name, v); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := vault.Set(name, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, tl := range InfraToolsFor(config.Config{}, vault, func(string, ...any) {}, true) {
		if tl.Name() != "k8s_login" {
			continue
		}
		d := tl.Description()
		if strings.Contains(d, "MODEL_KEY") || !strings.Contains(d, "DB_PASSWORD") {
			t.Fatalf("k8s_login's description: %s", d)
		}
	}
}

// personalKube writes a kubeconfig whose only context, personal, points at a
// server that counts the requests it is sent.
func personalKube(t *testing.T) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"kind":"PodList","items":[]}`)
	}))
	t.Cleanup(srv.Close)
	p := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	text := "current-context: personal\nclusters:\n- name: p\n  cluster:\n    server: " + srv.URL + "\n    insecure-skip-tls-verify: true\n" +
		"contexts:\n- name: personal\n  context:\n    cluster: p\n    user: me\nusers:\n- name: me\n  user:\n    token: personal-token\n"
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return &hits
}

// Where the organisation sets k8s with its own clusters, the person's
// kubeconfig is neither listed nor dialled: only the org's clusters are
// offered, and a pasted login is refused with the reason.
func TestAnOrgsClustersAreAllThatIsOffered(t *testing.T) {
	machineHome(t)
	hits := personalKube(t)
	cfg := config.Config{Managed: true, ManagedKeys: []string{"k8s.clusters", "k8s.enabled"}, SetKeys: []string{"k8s.clusters", "k8s.enabled"}}
	cfg.K8s.Enabled = true
	cfg.K8s.Clusters = []config.K8sClusterConfig{{Name: "orgprod", Server: "https://orgprod.example:6443"}}
	reg := tools.NewRegistry(InfraToolsFor(cfg, nil, func(string, ...any) {}, true)...)
	get, _ := reg.Get("k8s_get")
	login, _ := reg.Get("k8s_login")
	if get == nil || login == nil {
		t.Fatalf("the org's cluster tools are missing: %v", reg.Names())
	}
	if strings.Contains(get.Description(), "personal") {
		t.Fatalf("k8s_get lists the personal context: %s", get.Description())
	}
	if !strings.Contains(login.Description(), "orgprod") {
		t.Fatalf("k8s_login does not offer the org's cluster: %s", login.Description())
	}
	sess, _ := tools.NewSession(t.TempDir())
	for _, args := range []string{`{"resource":"pods","context":"personal"}`, `{"resource":"pods"}`} {
		res := get.Run(context.Background(), sess, json.RawMessage(args))
		if !res.IsError || !strings.Contains(res.Content, "no kubeconfig") {
			t.Fatalf("%s: %+v", args, res)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the personal cluster was dialled %d times", hits.Load())
	}
	id := tools.HoldPasted(sess, tools.Pasted{Value: "tok-0123456789abcdef", Server: "https://personal.example"})
	err := tools.Precheck(context.Background(), login, sess, json.RawMessage(`{"cluster":"lab","pasted":"`+id+`"}`))
	if err == nil || !strings.Contains(err.Error(), "ask your administrator") {
		t.Fatalf("a pasted login under the org's clusters: %v", err)
	}
}

// The second route: the org sets only k8s.allow_writes false, and the
// person's own file sets k8s.enabled true. The org has set k8s, so the
// person's true is not taken: no kubeconfig tools, and nothing dialled.
func TestAPersonsK8sEnabledIsNotTakenWhereTheOrgSetsK8s(t *testing.T) {
	machineHome(t)
	hits := personalKube(t)
	cfg := config.Config{Managed: true, ManagedKeys: []string{"k8s.allow_writes"}, SetKeys: []string{"k8s.allow_writes", "k8s.enabled"}}
	cfg.K8s.Enabled = true
	got := infraNames(cfg, true)
	if slices.Contains(got, "k8s_get") || slices.Contains(got, "k8s_apply") {
		t.Fatalf("the person's k8s.enabled was taken under the org's k8s setting: %v", got)
	}
	if hits.Load() != 0 {
		t.Fatal("the personal cluster was dialled")
	}
}
