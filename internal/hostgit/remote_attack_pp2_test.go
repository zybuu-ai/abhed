package hostgit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pp2(t *testing.T, r *remoteRig, planted, what string) (askErr, runErr error) {
	t.Helper()
	b := r.broker(Network{Open: true})
	_, _, _, askErr = b.Ask(r.key("c"), r.ws, "git fetch")
	t.Logf("%s: ask err=%v", what, askErr)
	out, runErr := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch"))
	t.Logf("%s: fetch %v\n%s", what, runErr, out)
	if planted != "" {
		if _, err := os.Stat(planted); err == nil {
			t.Errorf("%s: WORKSPACE PROGRAM RAN ON THE HOST", what)
		}
	}
	return askErr, runErr
}

// E7: the person's ssh config (absolute, outside every area) Includes a workspace file with a ProxyCommand.
func TestPP2SSHConfigInclude(t *testing.T) {
	r := newRemoteRig(t)
	planted := filepath.Join(r.home, "proxycmd-ran")
	ws := filepath.Join(r.ws, "ssh_extra")
	os.WriteFile(ws, []byte("Host git.example\n  ProxyCommand /usr/bin/touch "+planted+"\n"), 0o644)
	cfg := filepath.Join(r.home, "sshcfg")
	os.WriteFile(cfg, []byte("Include "+ws+"\n"), 0o600)
	run(t, r.home, "config", "--global", "core.sshCommand", "/usr/bin/ssh -F "+cfg+" -o BatchMode=yes")
	if err, _ := pp2(t, r, planted, "ssh -F cfg Include ws"); err == nil || !strings.Contains(err.Error(), "ssh configuration file") {
		t.Errorf("an ssh configuration including a workspace file was not refused: %v", err)
	}
}

// E8: a wrapper with an absolute path, then a bare name found on a PATH entry the agent writes.
func TestPP2WrapperBareNameOnWritablePath(t *testing.T) {
	r := newRemoteRig(t)
	planted := filepath.Join(r.home, "wrapped-ran")
	bin := filepath.Join(r.ws, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "myssh"), []byte("#!/bin/sh\ntouch "+planted+"\nexit 1\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	run(t, r.home, "config", "--global", "core.sshCommand", "/usr/bin/nice myssh")
	// ws/bin is left out of host git's PATH: myssh is found nowhere, and runs nowhere.
	pp2(t, r, planted, "nice myssh, PATH has ws/bin")
	run(t, r.home, "config", "--global", "core.sshCommand", "/usr/bin/env -S myssh")
	pp2(t, r, planted, "env -S myssh")
}

// E9/E10: usability: ~/ key path, and an askpass with a space.
func TestPP2TildeAndSpaces(t *testing.T) {
	r := newRemoteRig(t)
	stub := filepath.Join(filepath.Dir(r.ws), "ssh-stub")
	run(t, r.home, "config", "--global", "core.sshCommand", stub+" -i ~/.ssh/id_work")
	// Configure once: a ~/ key and a path with a space are brokered.
	if askErr, runErr := pp2(t, r, "", "ssh -i ~/.ssh/id_work"); askErr != nil || runErr != nil {
		t.Errorf("ssh -i ~/.ssh/id_work: %v %v", askErr, runErr)
	}
	r2 := newRemoteRig(t)
	d := filepath.Join(r2.home, "My Tools")
	os.MkdirAll(d, 0o700)
	os.WriteFile(filepath.Join(d, "askpass"), []byte("#!/bin/sh\necho x\n"), 0o700)
	run(t, r2.home, "config", "--global", "core.askPass", filepath.Join(d, "askpass"))
	if askErr, runErr := pp2(t, r2, "", "askpass with a space"); askErr != nil || runErr != nil {
		t.Errorf("askpass with a space: %v %v", askErr, runErr)
	}
}

// Host git's PATH is the person's, less what is relative or in the workspace
// or a writable area: the same for every brokered call, and the one the
// program checks look on.
func TestHostGitPathIsFiltered(t *testing.T) {
	r := newRemoteRig(t)
	area := filepath.Join(r.home, "writable-bin")
	if err := os.MkdirAll(area, 0o700); err != nil {
		t.Fatal(err)
	}
	old := writableAreas
	writableAreas = func() []string { return []string{area} }
	t.Cleanup(func() { writableAreas = old })
	t.Setenv("PATH", strings.Join([]string{"bin", filepath.Join(r.ws, "bin"), area, "/usr/bin", "/bin"}, string(os.PathListSeparator)))
	var envs [][]string
	seenEnv = func(env []string) { envs = append(envs, env) }
	t.Cleanup(func() { seenEnv = nil })
	b := r.broker(Network{Open: true})
	if out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch")); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}
	if len(envs) != 1 {
		t.Fatalf("saw %d host commands", len(envs))
	}
	for _, kv := range envs[0] {
		if p, ok := strings.CutPrefix(kv, "PATH="); ok && p != "/usr/bin"+string(os.PathListSeparator)+"/bin" {
			t.Fatalf("host git's PATH is %q", p)
		}
	}
	// A program found only in a dropped folder is found nowhere.
	if why := programUntrusted([]string{"myssh"}, []string{r.ws, area}, hostPATH([]string{r.ws, area})); why != "" {
		t.Fatalf("a name found nowhere: %s", why)
	}
}

// ~/.ssh/config, with no -F, is walked as git's configuration is: Includes
// (relative ones from ~/.ssh, globs expanded) into reach, a command it runs
// from the workspace, or a file not the person's alone stop the broker.
func TestSSHConfigIsWalked(t *testing.T) {
	r := newRemoteRig(t)
	sshDir := filepath.Join(r.home, ".ssh")
	cfg := filepath.Join(sshDir, "config")
	extra := filepath.Join(sshDir, "conf.d")
	if err := os.MkdirAll(extra, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(r.ws, filepath.Join(extra, "ws")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.ws, "extra.conf"), []byte("Host *\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The rig's ssh, named plainly so ~/.ssh/config is the one read.
	stub := filepath.Join(filepath.Dir(r.ws), "ssh-stub")
	run(t, r.home, "config", "--global", "core.sshCommand", stub)
	for body, why := range map[string]string{
		"Include conf.d/ws/*.conf\n":                                   "which commands can write",
		"Host git.example\n  ProxyCommand ./proxy %h %p\n":             "relative path",
		"Host *\n  ProxyCommand " + filepath.Join(r.ws, "p") + " %h\n": "which commands can write",
		"Match exec \"" + filepath.Join(r.ws, "m") + " %h\"\n":         "Match exec",
		"Host *\n  KnownHostsCommand sh -c 'cat x'\n":                  "(-c)",
		"Host *\n  LocalCommand ../run.sh\n":                           "relative path",
	} {
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch")
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%q: %v", body, err)
		}
	}
	// The person's own, out of reach, with a ProxyCommand of their own.
	if err := os.WriteFile(cfg, []byte("Host bastion\n  ProxyCommand /usr/bin/nc %h %p\nInclude conf.d/*.none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch"); err != nil {
		t.Fatalf("the person's own ssh configuration: %v", err)
	}
	if err := os.Chmod(cfg, 0o622); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch"); err == nil || !strings.Contains(err.Error(), "writable by other users") {
		t.Fatalf("an ssh configuration others can write: %v", err)
	}
}
