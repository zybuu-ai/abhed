//go:build linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Under bubblewrap, a command cannot leave in the workspace's git folder a
// pointer the person's own git would follow on the host: alternates,
// attributes and a linked worktree's pointers are held with their folders,
// and a commondir (which cannot be held before it exists) is moved aside as
// it is made, again as the command ends, and as the sandbox closes. The
// person's git status on the host then runs nothing the command chose.
func TestBubblewrapLeavesNoGitPointerOnTheHost(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ws := workspace(t)
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run(ws, "init", "-q", "-b", "main")
	run(ws, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "--allow-empty", "-m", "first")
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	if ok, why := s.Available(); !ok || s.backend != "bwrap" {
		t.Skipf("bubblewrap unavailable: %s", why)
	}
	marker := filepath.Join(t.TempDir(), "PWNED")
	dotGit := filepath.Join(ws, ".git")
	commondir := filepath.Join(dotGit, "commondir")

	// Held whole: none of these can be made.
	for _, attempt := range []string{
		"echo /tmp > .git/objects/info/alternates",
		"echo http://evil.example > .git/objects/info/http-alternates",
		"echo '* filter=x' > .git/info/attributes",
		"echo x > .git/info/grafts",
	} {
		if out, err := runIn(t, s, ws, attempt); err == nil {
			t.Errorf("the sandbox let a command run %q: %s", attempt, out)
		}
	}
	for _, f := range []string{"objects/info/alternates", "objects/info/http-alternates", "info/attributes", "info/grafts"} {
		if exists(filepath.Join(dotGit, f)) {
			t.Errorf("%s was made in the host's git folder", f)
		}
	}

	// A commondir made while the command still runs is gone at once.
	plant := "git init -q --bare evil && git -C evil config core.fsmonitor 'touch " + marker + "' && " +
		"echo " + filepath.Join(ws, "evil") + " > .git/commondir; sleep 2"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := s.Command(ctx, ws, plant)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if exists(commondir) {
		t.Error("a commondir made by a running command stayed in the host's git folder")
	}
	err := cmd.Wait()
	Exited(cmd, err)
	if exists(commondir) {
		t.Error("a commondir stayed after the command ended")
	}
	hostStatus := func() {
		c := exec.Command("git", "status", "--short")
		c.Dir = ws
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		_ = c.Run()
	}
	hostStatus()
	if exists(marker) {
		t.Fatal("the person's git status on the host ran a program the command chose")
	}
	// The next command is refused, and says why.
	if out, err := runIn(t, s, ws, "true"); err == nil || !strings.Contains(out+errString(err), "commondir") {
		t.Errorf("the command after a plant was not refused: %v %s", err, out)
	}

	// Planted and left by a command that ends before any next one: the
	// sandbox closing takes it out.
	_, _ = runIn(t, s, ws, "echo "+filepath.Join(ws, "evil")+" > .git/commondir")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if exists(commondir) {
		t.Error("a commondir stayed after the sandbox closed")
	}
	hostStatus()
	if exists(marker) {
		t.Fatal("the person's git status on the host ran a program the command chose")
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
