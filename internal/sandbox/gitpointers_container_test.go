//go:build linux || darwin

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// On the container tier the workspace is the host's, bind-mounted, and only
// the git files that exist are held, as under bubblewrap: a pointer a command
// plants in the workspace's git folder is taken out as it is made (Linux),
// as the command ends and as the sandbox closes, and held folders cannot take
// one at all. Runs on a real engine with ABHED_REQUIRE_CONTAINER=1.
func TestContainerLeavesNoGitPointerOnTheHost(t *testing.T) {
	if os.Getenv("ABHED_REQUIRE_CONTAINER") == "" {
		t.Skip("set ABHED_REQUIRE_CONTAINER=1 to run the container tier on a real engine")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := func() string {
		ws := workspace(t)
		for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "--allow-empty", "-m", "first"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = ws
			cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		return ws
	}
	ws := repo()
	c := NewContainer(Policy{Workspace: ws, ProtectGit: true})
	if ok, why := c.Available(); !ok {
		t.Fatalf("no container engine: %s", why)
	}
	run := func(command string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := c.Command(ctx, ws, command)
		if cmd.Err != nil {
			return "", cmd.Err
		}
		out, err := cmd.CombinedOutput()
		Exited(cmd, err)
		return string(out), err
	}
	dotGit := filepath.Join(ws, ".git")
	commondir := filepath.Join(dotGit, "commondir")
	marker := filepath.Join(t.TempDir(), "PWNED")
	evil := "mkdir -p evil && printf '[core]\\n\\tfsmonitor = touch " + marker + "\\n' > evil/config && "

	for _, attempt := range []string{
		"echo /tmp > .git/objects/info/alternates",
		"echo '* filter=x' > .git/info/attributes",
	} {
		if out, err := run(attempt); err == nil {
			t.Errorf("the container let a command run %q: %s", attempt, out)
		}
	}
	for _, f := range []string{"objects/info/alternates", "info/attributes"} {
		if exists(filepath.Join(dotGit, f)) {
			t.Errorf("%s was made in the host's git folder", f)
		}
	}

	if runtime.GOOS == "linux" {
		// The workspace is this host's: a commondir made while the command
		// still runs is moved aside at once.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := c.Command(ctx, ws, evil+"echo "+filepath.Join(ws, "evil")+" > .git/commondir; sleep 3")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for !exists(filepath.Join(ws, "evil", "config")) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(500 * time.Millisecond)
		if exists(commondir) {
			t.Error("a commondir made by a running command stayed in the host's git folder")
		}
		err := cmd.Wait()
		Exited(cmd, err)
		_, _ = run("true") // refused once, saying why
	}

	// Made by a command that ends: taken out as it ends.
	_, _ = run(evil + "echo " + filepath.Join(ws, "evil") + " > .git/commondir")
	if exists(commondir) {
		t.Error("a commondir stayed after the command ended")
	}
	_, _ = run("true")
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Made by a command whose end is not reported (a caller that never says),
	// and the sandbox closed before any next command: in a fresh workspace,
	// as a share into the engine's machine (macOS) can keep a moved file's
	// old name in its cache.
	ws = repo()
	dotGit, commondir = filepath.Join(ws, ".git"), filepath.Join(ws, ".git", "commondir")
	c = NewContainer(Policy{Workspace: ws, ProtectGit: true})
	if ok, why := c.Available(); !ok {
		t.Fatalf("no container engine: %s", why)
	}
	evil = "mkdir -p evil && printf '[core]\\n\\tfsmonitor = touch " + marker + "\\n' > evil/config && "
	{
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		out, err := c.Command(ctx, ws, evil+"echo "+filepath.Join(ws, "evil")+" > .git/commondir; sleep 1").CombinedOutput()
		cancel()
		if runtime.GOOS != "linux" && !exists(commondir) {
			t.Fatalf("the plant before Close did not land (%v: %s); the test proves nothing", err, out)
		}
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if exists(commondir) {
		t.Error("a commondir stayed after the sandbox closed")
	}
	// The person's own git, where it is safe to run here (Linux, in a
	// throwaway machine), runs nothing the command chose.
	if runtime.GOOS == "linux" {
		st := exec.Command("git", "status", "--short")
		st.Dir = ws
		st.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		_ = st.Run()
		if exists(marker) {
			t.Fatal("the person's git status ran a program the command chose")
		}
	}
}
