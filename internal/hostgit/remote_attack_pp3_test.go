package hostgit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pp3(t *testing.T, r *remoteRig, planted, what string) error {
	t.Helper()
	b := r.broker(Network{Open: true})
	_, _, _, askErr := b.Ask(r.key("c"), r.ws, "git fetch")
	t.Logf("%s: ask err=%v", what, askErr)
	out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch"))
	t.Logf("%s: fetch %v\n%s", what, err, out)
	if _, err := os.Stat(planted); err == nil {
		t.Errorf("%s: WORKSPACE CODE RAN ON THE HOST", what)
	}
	return askErr
}

func refused3(t *testing.T, err error, why string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), why) {
		t.Errorf("not refused for %q: %v", why, err)
	}
}

func sshCfg(t *testing.T, r *remoteRig, body string) {
	cfg := filepath.Join(r.home, "sshcfg")
	os.WriteFile(cfg, []byte(body), 0o600)
	run(t, r.home, "config", "--global", "core.sshCommand", "/usr/bin/ssh -F "+cfg+" -o BatchMode=yes")
}

// E11: Include with an environment variable naming the workspace.
func TestPP3IncludeEnv(t *testing.T) {
	r := newRemoteRig(t)
	planted := filepath.Join(r.home, "inc-env-ran")
	os.WriteFile(filepath.Join(r.ws, "ssh_extra"), []byte("Host git.example\n  ProxyCommand /usr/bin/touch "+planted+"\n"), 0o644)
	t.Setenv("PP3_DIR", r.ws)
	sshCfg(t, r, "Include ${PP3_DIR}/ssh_extra\n")
	refused3(t, pp3(t, r, planted, "Include ${ENV}"), "write the full path")
}

// E12: Include inside a Match block.
func TestPP3IncludeInMatch(t *testing.T) {
	r := newRemoteRig(t)
	planted := filepath.Join(r.home, "inc-match-ran")
	os.WriteFile(filepath.Join(r.ws, "ssh_extra"), []byte("ProxyCommand /usr/bin/touch "+planted+"\n"), 0o644)
	sshCfg(t, r, "Match host git.example\n  Include "+filepath.Join(r.ws, "ssh_extra")+"\n")
	refused3(t, pp3(t, r, planted, "Match + Include"), "which commands can write")
}

// E13: PKCS11Provider and SecurityKeyProvider name a library ssh loads into
// itself; one the agent writes is refused before ssh could load it.
func TestPP3PKCS11Provider(t *testing.T) {
	r := newRemoteRig(t)
	planted := filepath.Join(r.home, "pkcs11-ran")
	lib := filepath.Join(r.ws, "p.dylib")
	if err := os.WriteFile(lib, []byte("not a library"), 0o755); err != nil {
		t.Fatal(err)
	}
	sshCfg(t, r, "Host git.example\n  PKCS11Provider "+lib+"\n")
	refused3(t, pp3(t, r, planted, "PKCS11Provider"), "which commands can write")
	sshCfg(t, r, "Host git.example\n  SecurityKeyProvider "+lib+"\n  IdentityFile "+filepath.Join(r.home, "nokey")+"\n")
	refused3(t, pp3(t, r, planted, "SecurityKeyProvider"), "which commands can write")
}

// E14: ~/.ssh/config path given via -F as a symlink into the workspace.
func TestPP3SymlinkedConfig(t *testing.T) {
	r := newRemoteRig(t)
	planted := filepath.Join(r.home, "sym-ran")
	target := filepath.Join(r.ws, "sshcfg-real")
	os.WriteFile(target, []byte("Host git.example\n  ProxyCommand /usr/bin/touch "+planted+"\n"), 0o600)
	link := filepath.Join(r.home, "sshcfg-link")
	os.Symlink(target, link)
	run(t, r.home, "config", "--global", "core.sshCommand", "/usr/bin/ssh -F "+link)
	refused3(t, pp3(t, r, planted, "symlinked -F"), "which commands can write")
}

// The libraries and programs ssh loads, and its control socket's folder,
// are judged wherever they are set: a configuration file, an -o option, or
// SSH_SK_PROVIDER; none and internal pass, as do paths of the person's own.
func TestPP3SSHLoadsAndSockets(t *testing.T) {
	r := newRemoteRig(t)
	area := filepath.Join(r.home, "writable")
	if err := os.MkdirAll(area, 0o700); err != nil {
		t.Fatal(err)
	}
	old := writableAreas
	writableAreas = func() []string { return []string{area} }
	t.Cleanup(func() { writableAreas = old })
	stub := filepath.Join(filepath.Dir(r.ws), "ssh-stub")
	ask := func() error {
		_, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch")
		return err
	}
	for _, c := range []struct{ cmd, why string }{
		{stub + " -o PKCS11Provider=" + filepath.Join(r.ws, "p.so"), "which commands can write"},
		{stub + " -oXAuthLocation=xauth", "not an absolute path"},
		{stub + " -o ControlPath=" + filepath.Join(area, "cm-%r@%h"), "where the agent could stand in"},
		{stub + " -o ControlPath=%d/cm", "not known until ssh expands it"},
		{stub + " -F $HOME/cfg", "write the full path"},
		{stub + " -F ~other/cfg", "write the full path"},
	} {
		run(t, r.home, "config", "--global", "core.sshCommand", c.cmd)
		refused3(t, ask(), c.why)
	}
	cfg := filepath.Join(r.home, "sshcfg")
	run(t, r.home, "config", "--global", "core.sshCommand", stub+" -F "+cfg)
	for body, why := range map[string]string{
		"Include %d/extra\n":     "write the full path",
		"Include ~other/extra\n": "write the full path",
		"Host *\n  XAuthLocation " + filepath.Join(r.ws, "xauth") + "\n": "which commands can write",
		"Host *\n  ControlPath " + filepath.Join(r.ws, "cm") + "\n":      "where the agent could stand in",
	} {
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		refused3(t, ask(), why)
	}
	t.Setenv("SSH_SK_PROVIDER", filepath.Join(area, "sk.so"))
	if err := os.WriteFile(cfg, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	refused3(t, ask(), "SSH_SK_PROVIDER")
	t.Setenv("SSH_SK_PROVIDER", "internal")
	if err := os.WriteFile(cfg, []byte("Host *\n  SecurityKeyProvider internal\n  PKCS11Provider none\n  ControlMaster auto\n  ControlPath ~/.ssh/cm-%r@%h:%p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ask(); err != nil {
		t.Fatalf("the person's own settings: %v", err)
	}
}
