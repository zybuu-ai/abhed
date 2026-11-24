package hostgit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The files ssh and git trust for a brokered call (known hosts, revoked
// keys, certificates, CA bundles and folders, client keys) must be the
// person's alone: in the workspace or a writable folder, relative, expanded
// by ssh, or writable by others, they stop the broker. none, the defaults
// and the person's own pass.
func TestTrustFilesOutOfReach(t *testing.T) {
	r := newRemoteRig(t)
	area := filepath.Join(r.home, "writable")
	if err := os.MkdirAll(area, 0o700); err != nil {
		t.Fatal(err)
	}
	old := writableAreas
	writableAreas = func() []string { return []string{area} }
	t.Cleanup(func() { writableAreas = old })
	ask := func() error {
		_, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch")
		return err
	}
	refused := func(what, why string) {
		t.Helper()
		if err := ask(); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v", what, err)
		}
	}
	stub := filepath.Join(filepath.Dir(r.ws), "ssh-stub")
	cfg := filepath.Join(r.home, "sshcfg")
	run(t, r.home, "config", "--global", "core.sshCommand", stub+" -F "+cfg)
	for body, why := range map[string]string{
		"Host *\n  UserKnownHostsFile ~/.ssh/known_hosts " + filepath.Join(r.ws, "kh") + "\n": "vouch for a host",
		"Host *\n  GlobalKnownHostsFile " + filepath.Join(area, "kh") + "\n":                  "vouch for a host",
		"Host *\n  RevokedHostKeys " + filepath.Join(r.ws, "revoked") + "\n":                  "vouch for a host",
		"Host *\n  CertificateFile " + filepath.Join(r.ws, "id-cert.pub") + "\n":              "vouch for a host",
		"Host *\n  UserKnownHostsFile /opt/${KH}/kh\n":                                        "ssh expands",
		"Host *\n  UserKnownHostsFile /opt/%d/kh\n":                                           "ssh expands",
		"Host *\n  UserKnownHostsFile known_hosts\n":                                          "not an absolute path",
	} {
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		refused(body, why)
	}
	// As an -o option too.
	if err := os.WriteFile(cfg, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, r.home, "config", "--global", "core.sshCommand", stub+" -F "+cfg+" -o UserKnownHostsFile="+filepath.Join(r.ws, "kh"))
	refused("-o UserKnownHostsFile", "vouch for a host")
	run(t, r.home, "config", "--global", "core.sshCommand", stub+" -F "+cfg)
	// A known hosts file of the person's own, others can write, is not theirs alone.
	own := filepath.Join(r.home, "kh-own")
	if err := os.WriteFile(own, []byte(""), 0o622); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(own, 0o622); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("Host *\n  UserKnownHostsFile "+own+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	refused("a known hosts file others can write", "writable by other users")
	// The defaults, none and the person's own pass.
	if err := os.WriteFile(cfg, []byte("Host *\n  UserKnownHostsFile ~/.ssh/known_hosts\n  GlobalKnownHostsFile none\n  CertificateFile ~/.ssh/id_ed25519-cert.pub\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ask(); err != nil {
		t.Fatalf("the person's own ssh trust files: %v", err)
	}
	// git's https trust: CA bundle and folder, client cert and key, URL-scoped too.
	for _, kv := range [][2]string{
		{"http.sslCAInfo", filepath.Join(r.ws, "ca.pem")},
		{"http.sslCAPath", area},
		{"http.https://git.example/.sslCert", filepath.Join(area, "c.pem")},
		{"http.sslKey", "key.pem"},
	} {
		run(t, r.home, "config", "--global", kv[0], kv[1])
		if err := ask(); err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(kv[0])) {
			t.Errorf("%s = %s: %v", kv[0], kv[1], err)
		}
		run(t, r.home, "config", "--global", "--unset", kv[0])
	}
	ca := filepath.Join(r.home, "ca.pem")
	if err := os.WriteFile(ca, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, r.home, "config", "--global", "http.sslCAInfo", ca)
	if err := ask(); err != nil {
		t.Fatalf("the person's own CA bundle: %v", err)
	}
}
