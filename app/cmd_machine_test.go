package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/internal/machinegrant"
)

func TestMachineListAndForget(t *testing.T) {
	p := filepath.Join(t.TempDir(), "machine-grants.json")
	old := machinegrant.Path
	machinegrant.Path = func() string { return p }
	t.Cleanup(func() { machinegrant.Path = old })
	var out, errw bytes.Buffer
	if code := machineCmd([]string{"list"}, &out, &errw); code != 0 || !strings.Contains(out.String(), "nothing") {
		t.Fatalf("empty list: %d %q %q", code, out.String(), errw.String())
	}
	if err := machinegrant.Keep(machinegrant.Grant{Kind: machinegrant.KindSSHHost, Name: "box", Identity: "i", What: "host box (me@10.0.0.9:22)"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := machineCmd([]string{"list"}, &out, &errw); code != 0 || !strings.Contains(out.String(), "host box") {
		t.Fatalf("list: %d %q", code, out.String())
	}
	if code := machineCmd([]string{"forget", "ssh-host", "nope"}, &out, &errw); code != 1 {
		t.Fatalf("forgetting what is not kept: %d", code)
	}
	if code := machineCmd([]string{"forget", "bogus", "box"}, &out, &errw); code != 2 {
		t.Fatalf("a bad kind: %d", code)
	}
	out.Reset()
	if code := machineCmd([]string{"forget", "ssh-host", "box"}, &out, &errw); code != 0 || !strings.Contains(out.String(), "asks again") {
		t.Fatalf("forget: %d %q", code, out.String())
	}
	if gs, _ := machinegrant.List(); len(gs) != 0 {
		t.Fatalf("still kept: %v", gs)
	}
}

// doctor names the contexts, hosts and kept grants, never a credential.
func TestDoctorShowsTheMachineSetupWithoutSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", "")
	p := filepath.Join(home, "g.json")
	old := machinegrant.Path
	machinegrant.Path = func() string { return p }
	t.Cleanup(func() { machinegrant.Path = old })
	for path, text := range map[string]string{
		".kube/config": "current-context: lab\nclusters:\n- name: lab\n  cluster:\n    server: https://lab.example:6443\n" +
			"contexts:\n- name: lab\n  context:\n    cluster: lab\n    user: me\nusers:\n- name: me\n  user:\n    token: doctor-secret-token-77\n",
		".ssh/config": "Host box\n  HostName 10.0.0.9\n",
	} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(home, path)), 0o700)
		if err := os.WriteFile(filepath.Join(home, path), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = machinegrant.Keep(machinegrant.Grant{Kind: machinegrant.KindSSHHost, Name: "box", Identity: "i", What: "host box (me@10.0.0.9:22)"})
	out := strings.Join(machineDoctor(config.Config{}), "\n")
	for _, want := range []string{"1 contexts", "lab → https://lab.example:6443", "1 hosts", "box", "kept        1"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "doctor-secret-token-77") {
		t.Fatal("doctor printed the token")
	}
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	if out := strings.Join(machineDoctor(config.Config{}), "\n"); !strings.Contains(out, "k8s_login is offered") {
		t.Fatalf("with nothing set up: %s", out)
	}
}

// A guest may not forget a kept grant from the command line.
func TestGuestCannotForgetAMachineGrant(t *testing.T) {
	endGuest()
	if err := usePrincipal(identity.GuestPrincipal()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(endGuest)
	// What a guest approves "always" is not kept anywhere.
	if err := machinegrant.Keep(machinegrant.Grant{Kind: machinegrant.KindSSHHost, Name: "box", Identity: "i"}); err == nil {
		t.Fatal("a guest's grant was kept")
	}
	if err := guestSubcommand([]string{"machine", "forget", "ssh-host", "box"}); err == nil {
		t.Fatal("machine forget was not refused for a guest")
	}
	if err := guestSubcommand([]string{"machine", "list"}); err != nil {
		t.Fatalf("machine list was refused: %v", err)
	}
}
