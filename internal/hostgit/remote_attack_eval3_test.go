package hostgit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The eval's forms: keywords quoted or escaped mid-word, OpenSSH's aliases
// and deprecated names of judged keywords, and shell globs past a command's
// first word, each naming the workspace or a writable folder.
func TestEval3SSHForms(t *testing.T) {
	r := newRemoteRig(t)
	area := filepath.Join(r.home, "writable")
	if err := os.MkdirAll(area, 0o700); err != nil {
		t.Fatal(err)
	}
	old := writableAreas
	writableAreas = func() []string { return []string{area} }
	t.Cleanup(func() { writableAreas = old })
	stub := filepath.Join(filepath.Dir(r.ws), "ssh-stub")
	cfg := filepath.Join(r.home, "sshcfg")
	run(t, r.home, "config", "--global", "core.sshCommand", stub+" -F "+cfg)
	prog, lib := filepath.Join(r.ws, "evil"), filepath.Join(area, "p.dylib")
	ask := func() error {
		_, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch")
		return err
	}
	for body, why := range map[string]string{
		"Proxy\"Command\" " + prog + " %h\n":                          "which commands can write",
		"KnownHosts\"Command\" " + prog + "\n":                        "which commands can write",
		"Local'Command'=" + prog + "\n":                               "which commands can write",
		"Proxy\\Command " + prog + "\n":                               "which commands can write",
		"In\"clude\" " + filepath.Join(r.ws, "x.conf") + "\n":         "which commands can write",
		"SmartcardDevice " + lib + "\n":                               "which commands can write",
		"Smartcard\"Device\" " + lib + "\n":                           "which commands can write",
		"UserKnownHostsFile2 " + filepath.Join(r.ws, "kh") + "\n":     "vouch for a host",
		"GlobalKnownHostsFile2 " + filepath.Join(area, "kh") + "\n":   "vouch for a host",
		"IdentityFile2 " + filepath.Join(r.ws, "id") + "\n":           "vouch for a host",
		"IdentityFile " + filepath.Join(area, "id") + "\n":            "vouch for a host",
		"ProxyCommand /usr/bin/nc /tm?/x %h\n":                        "shell would expand",
		"ProxyCommand /usr/bin/nc " + filepath.Join(r.ws, "*") + "\n": "shell would expand",
	} {
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ask(); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%q: %v", body, err)
		}
	}
	// As an -o option, and in a helper's words.
	if err := os.WriteFile(cfg, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, r.home, "config", "--global", "core.sshCommand", stub+" -F "+cfg+" -oSmartcardDevice="+lib)
	if err := ask(); err == nil || !strings.Contains(err.Error(), "which commands can write") {
		t.Errorf("-oSmartcardDevice: %v", err)
	}
	run(t, r.home, "config", "--global", "core.sshCommand", stub+" -F "+cfg)
	for _, h := range []string{"store --file /tm?/creds", "store --file " + filepath.Join(r.home, "c*")} {
		run(t, r.home, "config", "--global", "credential.helper", h)
		if err := ask(); err == nil || !strings.Contains(err.Error(), "shell would expand") {
			t.Errorf("%s: %v", h, err)
		}
	}
	// The person's own, written in full, pass.
	run(t, r.home, "config", "--global", "credential.helper", "store --file "+filepath.Join(r.home, "creds"))
	if err := os.WriteFile(cfg, []byte("Host *\n  IdentityFile ~/.ssh/id_ed25519\n  ProxyCommand /usr/bin/nc %h %p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ask(); err != nil {
		t.Fatalf("the person's own: %v", err)
	}
}
