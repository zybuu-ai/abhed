package hostgit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// D1: the person's global config includes a file the agent can write (one in
// the workspace, as a dotfiles checkout often is); host git takes its ssh command
// as the person's.
func TestR4GlobalIncludeInWorkspace(t *testing.T) {
	r := newRemoteRig(t)
	planted := filepath.Join(r.home, "include-ssh-ran")
	inc := filepath.Join(r.ws, "dotfiles.gitconfig")
	sc := filepath.Join(r.ws, "evil-ssh")
	os.WriteFile(sc, []byte("#!/bin/sh\ntouch "+planted+"\nexit 1\n"), 0o755)
	os.WriteFile(inc, []byte("[core]\n\tsshCommand = "+sc+"\n"), 0o644)
	run(t, r.home, "config", "--global", "include.path", inc)
	b := r.broker(Network{Open: true})
	_, _, _, err := b.Ask(r.key("c"), r.ws, "git fetch")
	t.Logf("ask err=%v", err)
	if err == nil || !strings.Contains(err.Error(), "which commands can write") {
		t.Errorf("not refused: %v", err)
	}
	out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch"))
	t.Logf("fetch: %v\n%s", err, out)
	if _, err := os.Stat(planted); err == nil {
		t.Errorf("AN SSH COMMAND FROM A WORKSPACE FILE THE GLOBAL CONFIG INCLUDES RAN ON THE HOST")
	}
}

// D2: ~/.gitconfig itself is a link into the workspace (stow-managed dotfiles).
func TestR4GlobalConfigLinkedIntoWorkspace(t *testing.T) {
	r := newRemoteRig(t)
	planted := filepath.Join(r.home, "linked-ssh-ran")
	gc := filepath.Join(r.home, ".gitconfig")
	data, _ := os.ReadFile(gc)
	target := filepath.Join(r.ws, "dot-gitconfig")
	sc := filepath.Join(r.ws, "evil-ssh")
	os.WriteFile(sc, []byte("#!/bin/sh\ntouch "+planted+"\nexit 1\n"), 0o755)
	os.WriteFile(target, data, 0o644)
	os.Remove(gc)
	os.Symlink(target, gc)
	b := r.broker(Network{Open: true})
	// the agent edits the workspace file
	f, _ := os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("[core]\n\tsshCommand = " + sc + "\n")
	f.Close()
	_, _, _, err := b.Ask(r.key("c"), r.ws, "git fetch")
	t.Logf("ask err=%v", err)
	if err == nil || !strings.Contains(err.Error(), "which commands can write") {
		t.Errorf("not refused: %v", err)
	}
	out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch"))
	t.Logf("fetch: %v\n%s", err, out)
	if _, err := os.Stat(planted); err == nil {
		t.Errorf("A ~/.gitconfig LINKED INTO THE WORKSPACE STEERED HOST GIT")
	}
}
