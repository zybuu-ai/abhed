package hostgit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ppFetch(t *testing.T, r *remoteRig, planted, what string) error {
	t.Helper()
	b := r.broker(Network{Open: true})
	_, _, _, askErr := b.Ask(r.key("c"), r.ws, "git fetch")
	t.Logf("%s: ask err=%v", what, askErr)
	out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch"))
	t.Logf("%s: fetch %v\n%s", what, err, out)
	if planted != "" {
		if _, err := os.Stat(planted); err == nil {
			t.Errorf("%s: WORKSPACE PROGRAM RAN ON THE HOST", what)
		}
	}
	return askErr
}

// refusedFor fails t unless err refuses for why.
func refusedFor(t *testing.T, err error, why string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), why) {
		t.Errorf("not refused for %q: %v", why, err)
	}
}

// E2: a trusted interpreter as the first word, a workspace script as its argument.
func TestPPInterpreterWithRelativeScript(t *testing.T) {
	r := newRemoteRig(t)
	planted := filepath.Join(r.home, "interp-ran")
	os.WriteFile(filepath.Join(r.ws, "ssh-wrap.sh"), []byte("touch "+planted+"\nexit 1\n"), 0o644)
	run(t, r.home, "config", "--global", "core.sshCommand", "/bin/sh ./ssh-wrap.sh")
	refusedFor(t, ppFetch(t, r, planted, "sh ./script"), "relative path")
}

// E3: ssh with a relative -F config file (ProxyCommand in the workspace).
func TestPPSSHRelativeConfig(t *testing.T) {
	r := newRemoteRig(t)
	planted := filepath.Join(r.home, "sshF-ran")
	os.WriteFile(filepath.Join(r.ws, "ssh-wrap2"), []byte("#!/bin/sh\ntouch "+planted+"\nexit 1\n"), 0o755)
	run(t, r.home, "config", "--global", "core.sshCommand", "/bin/sh -c ./ssh-wrap2")
	refusedFor(t, ppFetch(t, r, planted, "sh -c ./x"), "(-c)")
}

// E4: the person's insteadOf to ext:: and file://, and protocol.ext.allow.
func TestPPInsteadOfExtFile(t *testing.T) {
	for _, base := range []string{"ext::sh -c touch% /tmp/pp-ext-ran% >&2", "file:///tmp/pp-nonexistent/"} {
		r := newRemoteRig(t)
		run(t, r.home, "config", "--global", "url."+base+".insteadOf", "git@git.example:")
		run(t, r.home, "config", "--global", "protocol.ext.allow", "always")
		ppFetch(t, r, "/tmp/pp-ext-ran", base)
	}
	os.Remove("/tmp/pp-ext-ran")
}

// E5: a common shell-function helper (usability: is brokering refused?).
func TestPPShellFunctionHelper(t *testing.T) {
	r := newRemoteRig(t)
	run(t, r.home, "config", "--global", "credential.helper", "!f() { echo username=x; echo password=y; }; f")
	// Brokering is off for it; a helper script is what guide 04 suggests.
	refusedFor(t, ppFetch(t, r, "", "shell-function helper"), "shell command")
}

// E6: credential.helper reset ordering: "" then a helper.
func TestPPHelperReset(t *testing.T) {
	r := newRemoteRig(t)
	run(t, r.home, "config", "--global", "--add", "credential.helper", "")
	run(t, r.home, "config", "--global", "--add", "credential.helper", "/usr/bin/true")
	var got []string
	for _, kv := range remoteConfig(context.Background(), r.ws, []string{"origin"}) {
		if kv[0] == "credential.helper" {
			got = append(got, kv[1])
		}
	}
	if strings.Join(got, "|") != "||/usr/bin/true" {
		t.Fatalf("credential.helper in order: %q", got)
	}
}

// More of E2's shape: a trusted program with a workspace script, a relative
// ssh config or proxy command, or an absolute path into the workspace, in
// any word; and a ! helper with shell syntax past its first word.
func TestPPEveryWordIsJudged(t *testing.T) {
	r := newRemoteRig(t)
	for _, c := range []struct{ key, value, why string }{
		{"core.sshCommand", "/bin/sh " + filepath.Join(r.ws, "x"), "which commands can write"},
		{"core.sshCommand", "ssh -F cfg/ssh_config", "relative path"},
		{"core.sshCommand", "ssh -oProxyCommand=./proxy", "relative path"},
		{"core.sshCommand", "ssh -o ProxyCommand=" + filepath.Join(r.ws, "p"), "which commands can write"},
		{"credential.helper", "!/bin/sh ./helper", "relative path"},
		{"credential.helper", "!/usr/bin/env x; touch /tmp/y", "shell command"},
		{"core.sshCommand", "/usr/bin/env ./ask", "relative path"},
	} {
		run(t, r.home, "config", "--global", "--replace-all", c.key, c.value)
		_, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch")
		if err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s = %s: %v", c.key, c.value, err)
		}
		if c.key == "core.sshCommand" {
			run(t, r.home, "config", "--global", "core.sshCommand", filepath.Join(filepath.Dir(r.ws), "ssh-stub"))
		} else {
			run(t, r.home, "config", "--global", "--unset-all", c.key)
		}
	}
	// A program of the person's own with words of its own passes.
	run(t, r.home, "config", "--global", "credential.helper", "!/usr/bin/true get --url=https://git.example/x")
	if _, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch"); err != nil {
		t.Fatalf("the person's own helper: %v", err)
	}
}

// Host git runs a remote command from a folder of the person's own, the
// repository given as GIT_DIR, so a relative path resolves there.
func TestRemoteCommandsRunOutsideTheRepository(t *testing.T) {
	r := newRemoteRig(t)
	var envs [][]string
	seenEnv = func(env []string) { envs = append(envs, env) }
	t.Cleanup(func() { seenEnv = nil })
	b := r.broker(Network{Open: true})
	cmd := b.Command(r.ctx("c"), r.ws, "git fetch")
	if cmd.Err != nil {
		t.Fatal(cmd.Err)
	}
	if within(cmd.Dir, r.ws) || strings.Contains(strings.Join(cmd.Args, " "), "-C '"+r.ws) {
		t.Fatalf("host git runs in the repository: %s %v", cmd.Dir, cmd.Args)
	}
	if len(envs) != 1 || !slicesContainsPrefix(envs[0], "GIT_DIR="+filepath.Join(r.ws, ".git")) {
		t.Fatalf("no GIT_DIR: %v", envs)
	}
	_, _ = runCmd(t, cmd)
}

func slicesContainsPrefix(env []string, want string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, want) {
			return true
		}
	}
	return false
}
