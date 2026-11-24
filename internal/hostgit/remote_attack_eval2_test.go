package hostgit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The eval's forms of the ssh configuration ssh reads but the walk missed:
// Match !exec, Match exec=cmd, and quoted keywords ("ProxyCommand",
// "LocalCommand", "Include"), each naming the workspace.
func TestEval2SSHConfigForms(t *testing.T) {
	r := newRemoteRig(t)
	stub := filepath.Join(filepath.Dir(r.ws), "ssh-stub")
	cfg := filepath.Join(r.home, "sshcfg")
	run(t, r.home, "config", "--global", "core.sshCommand", stub+" -F "+cfg)
	extra := filepath.Join(r.ws, "extra.conf")
	if err := os.WriteFile(extra, []byte("Host *\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prog := filepath.Join(r.ws, "evil")
	for body, why := range map[string]string{
		"Match !exec \"" + prog + " %h\"\n":     "Match exec",
		"Match exec=" + prog + "\n":             "Match exec",
		"Match host x EXEC " + prog + "\n":      "Match exec",
		"\"ProxyCommand\" " + prog + " %h %p\n": "which commands can write",
		"\"LocalCommand\" " + prog + "\n":       "which commands can write",
		"\"Include\" " + extra + "\n":           "which commands can write",
		"ProxyCommand=" + prog + " %h\n":        "which commands can write",
		"  proxycommand = " + prog + "\n":       "which commands can write",
	} {
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch")
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%q: %v", body, err)
		}
	}
}

// A named credential helper's own words are judged as a ! helper's are:
// store --file or cache --socket into a writable folder stops the broker;
// the person's own file passes.
func TestEval2NamedHelperArguments(t *testing.T) {
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
	for _, h := range []string{
		"store --file " + filepath.Join(area, "creds"),
		"cache --socket " + filepath.Join(area, "sock"),
		"store --file=" + filepath.Join(r.ws, "creds"),
	} {
		run(t, r.home, "config", "--global", "credential.helper", h)
		if err := ask(); err == nil || !strings.Contains(err.Error(), "credential.helper") {
			t.Errorf("%s: %v", h, err)
		}
	}
	run(t, r.home, "config", "--global", "credential.helper", "store --file "+filepath.Join(r.home, "own-creds"))
	if err := ask(); err != nil {
		t.Fatalf("the person's own store file: %v", err)
	}
}
