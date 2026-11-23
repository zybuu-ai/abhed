package hostgit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A11: a token in the person's global insteadOf is printed to the model by a brokered remote -v / failing fetch.
func TestAttackInsteadOfTokenInOutput(t *testing.T) {
	r := newRemoteRig(t)
	gc := filepath.Join(r.home, ".gitconfig")
	f, _ := os.OpenFile(gc, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("[url \"https://oauth2:GLOBAL-TOKEN-99@git.example/\"]\n\tinsteadOf = https://git.example/\n")
	f.Close()
	run(t, r.ws, "remote", "set-url", "origin", "https://git.example/team/app.git")
	b := r.broker(Network{Open: true})
	for _, line := range []string{"git ls-remote origin", "git fetch"} {
		_, _, _, _ = b.Ask(r.key("c"), r.ws, line)
		out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, line))
		t.Logf("%s: %v\n%s", line, err, out)
		if strings.Contains(out, "GLOBAL-TOKEN-99") || strings.Contains(fmt.Sprint(err), "GLOBAL-TOKEN-99") {
			t.Errorf("%s printed the person's token: %s", line, out)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if b, _ := json.Marshal(e); strings.Contains(string(b), "GLOBAL-TOKEN-99") {
			t.Errorf("the record holds the token: %s", b)
		}
	}
}

// A12: a clone into a git folder (.git/hooks, empty as protect_git makes it)
// would put the remote's files where git runs them.
func TestAttackCloneIntoGitFolder(t *testing.T) {
	r := newRemoteRig(t)
	root := filepath.Dir(r.ws)
	seed := filepath.Join(root, "seed")
	if err := os.WriteFile(filepath.Join(seed, "pre-commit"), []byte("#!/bin/sh\necho hooked\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, seed, "add", "pre-commit")
	run(t, seed, "commit", "-q", "-m", "hook")
	run(t, seed, "push", "-q", r.bare, "main")
	hooks := filepath.Join(r.ws, ".git", "hooks")
	_ = os.RemoveAll(hooks)
	if err := os.Mkdir(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	b := r.broker(Network{Open: true})
	for i, dest := range []string{".git/hooks", ".GIT/hooks", "./.git/x", "sub/../.git/hooks"} {
		line := "git clone git@git.example:team/app.git " + dest
		id := fmt.Sprintf("c%d", i)
		if _, _, _, err := b.Ask(r.key(id), r.ws, line); err == nil {
			t.Errorf("%s was not refused", line)
		}
		_, _ = runCmd(t, b.Command(r.ctx(id), r.ws, line))
	}
	if _, err := os.Stat(filepath.Join(hooks, "pre-commit")); err == nil {
		t.Fatal("THE CLONE WROTE INTO .git/hooks")
	}
}
