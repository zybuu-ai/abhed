package hostgit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// shapes are the rig's repository opened three ways whose git folder lies
// outside the workspace: a folder inside it, a linked worktree, and a
// submodule checkout; each named by the workspace Abhed would be given.
func shapes(t *testing.T, r *remoteRig) map[string]string {
	t.Helper()
	deep := filepath.Join(r.ws, "deep")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(filepath.Dir(r.ws), "wt")
	run(t, r.ws, "worktree", "add", "-q", "-b", "wt-branch", wt)
	super := filepath.Join(filepath.Dir(r.ws), "super")
	run(t, filepath.Dir(r.ws), "init", "-q", "-b", "main", super)
	run(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", r.bare, "app")
	app := filepath.Join(super, "app")
	run(t, app, "remote", "set-url", "origin", "git@git.example:team/app.git")
	return map[string]string{"subfolder": deep, "linked worktree": wt, "submodule": app}
}

// A remote command in a workspace whose git folder lies outside it runs
// through host git, as it does at a repository's top: the git folders are
// pinned as the session starts and lie where no command may write.
func TestRemoteCommandsBrokerWhereTheGitFolderIsOutside(t *testing.T) {
	r := newRemoteRig(t)
	for name, ws := range shapes(t, r) {
		t.Run(name, func(t *testing.T) {
			_ = os.Remove(r.sshLog)
			r.mu.Lock()
			r.inner = nil
			r.mu.Unlock()
			b := NewBroker(ws, func() sandbox.Tier { return sandbox.TierProcess }, Network{Open: true}, r.command).
				HoldsGit(func() bool { return true }, func() bool { return true })
			out, err := runCmd(t, b.Command(r.ctx("c-"+name), ws, "git fetch origin"))
			if err != nil {
				t.Fatalf("fetch: %v\n%s", err, out)
			}
			if len(r.inner) != 0 {
				t.Fatalf("the fetch was left to the sandbox: %v", r.inner)
			}
			if log, _ := os.ReadFile(r.sshLog); !strings.Contains(string(log), "git.example agent=set") {
				t.Fatalf("host git did not reach the remote with the person's ssh:\n%s", log)
			}
		})
	}
}

// A git folder outside the workspace that a command may write is never
// pinned, so the line stays in the sandbox.
func TestAWritableOutsideGitFolderIsNotBrokered(t *testing.T) {
	r := newRemoteRig(t)
	wt := shapes(t, r)["linked worktree"]
	writableAreas = func() []string { return []string{filepath.Dir(r.ws)} }
	r.runInner = false
	b := NewBroker(wt, func() sandbox.Tier { return sandbox.TierProcess }, Network{Open: true}, r.command).
		HoldsGit(func() bool { return true }, func() bool { return true })
	if _, err := runCmd(t, b.Command(r.ctx("c-w"), wt, "git fetch origin")); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(r.inner) != 1 {
		t.Fatalf("a fetch whose git folder a command may write was not left to the sandbox: %v", r.inner)
	}
}

// A worktree's .git file pointed at another repository's git folder, which
// does not name the worktree back, reaches nothing.
func TestARepointedGitFileIsNotBrokered(t *testing.T) {
	r := newRemoteRig(t)
	wt := shapes(t, r)["linked worktree"]
	other := filepath.Join(filepath.Dir(r.ws), "other")
	run(t, filepath.Dir(r.ws), "init", "-q", "-b", "main", other)
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(other, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := sandbox.GitReach(wt); len(got) != 0 {
		t.Fatalf("a repointed .git file reached %v", got)
	}
	b := NewBroker(wt, func() sandbox.Tier { return sandbox.TierProcess }, Network{Open: true}, r.command).
		HoldsGit(func() bool { return true }, func() bool { return true })
	_, _ = runCmd(t, b.Command(r.ctx("c-o"), wt, "git fetch origin"))
	if log, _ := os.ReadFile(r.sshLog); strings.Contains(string(log), "agent=set") {
		t.Fatalf("host git ran for a repointed .git file:\n%s", log)
	}
}
