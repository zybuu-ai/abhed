package hostgit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Configure once: an IPv6 bastion's [%h]:%p and a key named by a token in
// its last path element (~/.ssh/%h.key) are brokered; brackets around
// anything else, a token in a folder, or a folder in reach are not.
func TestSSHTokensConfigureOnce(t *testing.T) {
	r := newRemoteRig(t)
	area := filepath.Join(r.home, "writable")
	shared := filepath.Join(r.home, "shared")
	for _, d := range []string{area, shared, filepath.Join(r.home, ".ssh")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(shared, 0o700) })
	old := writableAreas
	writableAreas = func() []string { return []string{area} }
	t.Cleanup(func() { writableAreas = old })
	stub := filepath.Join(filepath.Dir(r.ws), "ssh-stub")
	cfg := filepath.Join(r.home, "sshcfg")
	run(t, r.home, "config", "--global", "core.sshCommand", stub+" -F "+cfg)
	ask := func(body string) error {
		t.Helper()
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch")
		return err
	}
	for _, body := range []string{
		"Host bastion6\n  ProxyCommand /usr/bin/ssh -W [%h]:%p jump.example\n",
		"Host *\n  ProxyCommand /usr/bin/nc [%h] %p\n",
		"Host *\n  IdentityFile ~/.ssh/%h.key\n  CertificateFile ~/.ssh/%r@%h-cert.pub\n  UserKnownHostsFile ~/.ssh/known_hosts_%h\n",
	} {
		if err := ask(body); err != nil {
			t.Errorf("%q was refused: %v", body, err)
		}
	}
	for body, why := range map[string]string{
		"Host *\n  ProxyCommand /usr/bin/nc [%h%p] 22\n":                   "shell would expand",
		"Host *\n  ProxyCommand /usr/bin/nc [%h]* 22\n":                    "shell would expand",
		"Host *\n  ProxyCommand /usr/bin/nc [ab] 22\n":                     "shell would expand",
		"Host *\n  IdentityFile ~/.ssh/%h/key\n":                           "ssh expands",
		"Host *\n  IdentityFile ~/.ssh/%d/%h.key\n":                        "ssh expands",
		"Host *\n  IdentityFile " + filepath.Join(r.ws, "%h.key") + "\n":   "vouch for a host",
		"Host *\n  IdentityFile " + filepath.Join(area, "%h.key") + "\n":   "vouch for a host",
		"Host *\n  IdentityFile " + filepath.Join(shared, "%h.key") + "\n": "other users can write",
		"Host *\n  IdentityFile ~/.ssh/%-.key\n":                           "not an ssh token",
		"Host *\n  IdentityFile ~/.ssh/$H%h.key\n":                         "ssh expands",
	} {
		if err := ask(body); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%q: %v", body, err)
		}
	}
}
