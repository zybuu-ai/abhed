package sandbox

import (
	"context"
	"os"
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
