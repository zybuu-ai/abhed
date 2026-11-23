package hostgit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// E1: the person's own core.sshCommand names a program by a relative path (or one in a
// writable area); host git runs it from the repository, where the agent writes.
func TestEvRelativeSSHCommand(t *testing.T) {
	r := newRemoteRig(t)
	planted := filepath.Join(r.home, "rel-ssh-ran")
	os.WriteFile(filepath.Join(r.ws, "ssh-wrapper"), []byte("#!/bin/sh\ntouch "+planted+"\nexit 1\n"), 0o755)
	run(t, r.home, "config", "--global", "core.sshCommand", "./ssh-wrapper")
	b := r.broker(Network{Open: true})
	_, _, _, err := b.Ask(r.key("c"), r.ws, "git fetch")
	if err == nil || !strings.Contains(err.Error(), "relative path") {
		t.Errorf("a relative ssh command was not refused: %v", err)
	}
	out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch"))
	t.Logf("fetch: %v\n%s", err, out)
	if _, err := os.Stat(planted); err == nil {
		t.Errorf("A RELATIVE ssh COMMAND FROM THE PERSON'S CONFIG RAN THE WORKSPACE'S PROGRAM ON THE HOST")
	}
}

// E1's variants: a program the person's configuration or environment names
// for host git (the ssh command, an askpass, a git proxy, a credential
// helper, in any form) that the agent could write, or that is not the
// person's alone, stops the broker; one of their own runs.
func TestEvPersonProgramsOutOfReach(t *testing.T) {
	r := newRemoteRig(t)
	area := filepath.Join(r.home, "writable")
	shared := filepath.Join(r.home, "shared")
	for _, d := range []string{area, shared} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	prog := func(dir, name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	inWS, inArea := prog(r.ws, "ws-prog"), prog(area, "area-prog")
	prog(area, "git-credential-planted") // found on PATH for the helper "planted"
	worldW := prog(shared, "world-prog")
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(shared, 0o700) })
	old := writableAreas
	writableAreas = func() []string { return []string{area} }
	t.Cleanup(func() { writableAreas = old })
	refused := func(what string, why string) {
		t.Helper()
		_, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch")
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v", what, err)
		}
	}
	cases := []struct{ key, value, why string }{
		{"core.sshCommand", inWS + " -v", "which commands can write"},
		{"core.sshCommand", inArea, "which commands can write"},
		{"core.sshCommand", worldW, "other users can write"},
		{"core.askPass", inWS, "which commands can write"},
		{"core.gitProxy", "./proxy", "relative path"},
		{"credential.helper", "!" + inArea + " get", "which commands can write"},
		{"credential.helper", "!f() { echo password=x; }; f", "shell command"},
		{"credential.helper", "planted", "PATH"},
	}
	for _, c := range cases {
		run(t, r.home, "config", "--global", "--replace-all", c.key, c.value)
		if c.key == "credential.helper" {
			t.Setenv("PATH", area+string(os.PathListSeparator)+os.Getenv("PATH"))
		}
		refused(c.key+"="+c.value, c.why)
		if c.key == "core.sshCommand" {
			// The rig's own ssh, the person's, back again.
			run(t, r.home, "config", "--global", "core.sshCommand", filepath.Join(filepath.Dir(r.ws), "ssh-stub"))
		} else {
			run(t, r.home, "config", "--global", "--unset-all", c.key)
		}
	}
	t.Setenv("SSH_ASKPASS", inWS)
	refused("SSH_ASKPASS", "which commands can write")
	t.Setenv("SSH_ASKPASS", "")
	// The person's own programs, out of reach, are brokered.
	if _, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch"); err != nil {
		t.Fatalf("the person's own programs: %v", err)
	}
}

// Worktree commands are brokered only where the sandbox holds git's
// configuration, as remote ones are.
func TestWorktreeNeedsGitHeld(t *testing.T) {
	r := newRemoteRig(t)
	b := NewBroker(r.ws, func() sandbox.Tier { return sandbox.TierProcess }, Network{Open: true}, r.command)
	if _, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git worktree prune")); err != nil {
		t.Fatal(err)
	}
	if len(r.inner) != 1 {
		t.Fatalf("a worktree command was brokered with git not held: %v", r.inner)
	}
}
