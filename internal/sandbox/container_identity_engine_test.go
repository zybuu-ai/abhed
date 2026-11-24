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

// On a real engine, a commit in the container carries the person's name and
// email, which the container's own home does not hold. It runs only with
// ABHED_REQUIRE_CONTAINER set and an image with git (ABHED_SANDBOX_IMAGE).
func TestContainerCommitCarriesGitIdentity(t *testing.T) {
	if os.Getenv("ABHED_REQUIRE_CONTAINER") == "" {
		t.Skip("set ABHED_REQUIRE_CONTAINER=1 to run the container tier on a real engine")
	}
	podmanFromRealHome(t)
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	id := []string{"GIT_AUTHOR_NAME=Ana Person", "GIT_COMMITTER_NAME=Ana Person",
		"GIT_AUTHOR_EMAIL=ana@example.test", "GIT_COMMITTER_EMAIL=ana@example.test"}
	c := NewContainer(Policy{Workspace: ws, GitIdentity: id})
	if ok, why := c.Available(); !ok {
		t.Fatalf("no container engine: %s", why)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out, err := c.Command(ctx, ws, "git init -q . && git commit -q --allow-empty -m one && "+
		"git log -1 --format='%an <%ae> | %cn <%ce>'").CombinedOutput()
	if err != nil {
		t.Fatalf("the commit failed in the container: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "Ana Person <ana@example.test> | Ana Person <ana@example.test>" {
		t.Fatalf("the commit carries %q", got)
	}
}

// podmanFromRealHome points podman at its default connection as the real
// home names it, where none is set: the tests' own home has no connections.
func podmanFromRealHome(t *testing.T) {
	t.Helper()
	if os.Getenv("CONTAINER_HOST") != "" || testOrigHome == "" {
		return
	}
	cmd := exec.Command("podman", "system", "connection", "list", "--format", "{{.URI}} {{.Identity}} {{.Default}}")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "XDG_") && !strings.HasPrefix(kv, "HOME=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+testOrigHome)
	out, err := cmd.Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[2] == "true" {
			t.Setenv("CONTAINER_HOST", f[0])
			t.Setenv("CONTAINER_SSHKEY", f[1])
			return
		}
	}
}
