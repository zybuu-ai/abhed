package hostgit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func evilSSH(t *testing.T, r *remoteRig, name string) (script, planted string) {
	planted = filepath.Join(r.home, name+"-ran")
	script = filepath.Join(r.ws, name+"-ssh")
	os.WriteFile(script, []byte("#!/bin/sh\ntouch "+planted+"\nexit 1\n"), 0o755)
	return
}

func tryFetch(t *testing.T, r *remoteRig, planted string, what string) {
	b := r.broker(Network{Open: true})
	_, _, _, err := b.Ask(r.key("c"), r.ws, "git fetch")
	t.Logf("%s ask err=%v", what, err)
	out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch"))
	t.Logf("%s fetch: %v\n%s", what, err, out)
	if _, err := os.Stat(planted); err == nil {
		t.Errorf("%s: AGENT-WRITABLE CONFIG STEERED HOST GIT", what)
	}
}

// D3: ~/.gitconfig is a hard link to a workspace file.
func TestR5HardlinkedGlobal(t *testing.T) {
	r := newRemoteRig(t)
	sc, planted := evilSSH(t, r, "hl")
	gc := filepath.Join(r.home, ".gitconfig")
	ws := filepath.Join(r.ws, "dot-gitconfig")
	if err := os.Link(gc, ws); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(ws, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("[core]\n\tsshCommand = " + sc + "\n")
	f.Close()
	tryFetch(t, r, planted, "hardlink")
}

// D4: includeIf onbranch: activates a workspace include only after the check.
func TestR5OnBranchAfterCheck(t *testing.T) {
	r := newRemoteRig(t)
	sc, planted := evilSSH(t, r, "ob")
	inc := filepath.Join(r.ws, "branch.gitconfig")
	os.WriteFile(inc, []byte("[core]\n\tsshCommand = "+sc+"\n"), 0o644)
	run(t, r.home, "config", "--global", "includeIf.onbranch:evil.path", inc)
	b := r.broker(Network{Open: true})
	// Every include is checked, whatever its condition: refused before the branch can change.
	if _, _, _, err := b.Ask(r.key("c"), r.ws, "git fetch"); err == nil || !strings.Contains(err.Error(), "which commands can write") {
		t.Errorf("an inactive include in the workspace was not refused: %v", err)
	}
	cmd := b.Command(r.ctx("c"), r.ws, "git fetch")
	if cmd.Err != nil {
		return
	}
	run(t, r.ws, "checkout", "-q", "-b", "evil") // the agent's background job switches branch
	out, err := runCmd(t, cmd)
	t.Logf("fetch: %v\n%s", err, out)
	if _, err := os.Stat(planted); err == nil {
		t.Errorf("onbranch: A WORKSPACE INCLUDE ACTIVATED AFTER THE CHECK STEERED HOST GIT")
	}
}

// D5: XDG_CONFIG_HOME points into the workspace.
func TestR5XDGInWorkspace(t *testing.T) {
	r := newRemoteRig(t)
	sc, planted := evilSSH(t, r, "xdg")
	x := filepath.Join(r.ws, "xdg")
	os.MkdirAll(filepath.Join(x, "git"), 0o755)
	os.WriteFile(filepath.Join(x, "git", "config"), []byte("[core]\n\tsshCommand = "+sc+"\n"), 0o644)
	// ~/.gitconfig sets sshCommand too and wins over XDG; remove it from there
	run(t, r.home, "config", "--global", "--unset", "core.sshCommand")
	t.Setenv("XDG_CONFIG_HOME", x)
	tryFetch(t, r, planted, "xdg")
}

// D6: GIT_CONFIG_GLOBAL in the person's environment names a workspace file.
func TestR5GitConfigGlobalEnv(t *testing.T) {
	r := newRemoteRig(t)
	sc, planted := evilSSH(t, r, "gcg")
	g := filepath.Join(r.ws, "g.gitconfig")
	os.WriteFile(g, []byte("[core]\n\tsshCommand = "+sc+"\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", g)
	tryFetch(t, r, planted, "GIT_CONFIG_GLOBAL")
}

// D7: includeIf gitdir: in the global config names a workspace file.
func TestR5IncludeIfGitdir(t *testing.T) {
	r := newRemoteRig(t)
	sc, planted := evilSSH(t, r, "gd")
	inc := filepath.Join(r.ws, "gd.gitconfig")
	os.WriteFile(inc, []byte("[core]\n\tsshCommand = "+sc+"\n"), 0o644)
	run(t, r.home, "config", "--global", "includeIf.gitdir:"+r.ws+"/.path", inc)
	tryFetch(t, r, planted, "includeIf gitdir")
}
