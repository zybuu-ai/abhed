package sandbox_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// A command cannot grant itself trust: the store, wherever config puts it,
// is neither writable nor replaceable from the process sandbox.
func TestProcessSandboxCannotWriteTheTrustStore(t *testing.T) {
	if runtime.GOOS == "linux" {
		if err := exec.Command("bwrap", "--unshare-net", "--ro-bind", "/", "/", "/bin/true").Run(); err != nil {
			t.Skipf("this environment cannot unshare the network namespace: %v", err)
		}
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	store, err := config.TrustStorePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(store), 0o700); err != nil {
		t.Fatal(err)
	}
	const empty = `{"version":1,"workspaces":{}}`
	if err := os.WriteFile(store, []byte(empty), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := sandbox.NewProcess(sandbox.DefaultPolicy(ws))
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	grant := `{"version":1,"workspaces":{"` + ws + `":{"sha256":"x","decision":"trusted"}}}`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := s.Command(ctx, ws, "echo '"+grant+"' > "+store+" 2>&1; echo '"+grant+"' > "+store+".new 2>&1 && mv "+store+".new "+store+" 2>&1; echo done").CombinedOutput()
	if got, _ := os.ReadFile(store); string(got) != empty {
		t.Fatalf("a command rewrote the trust store:\n%s\n%s", got, out)
	}
	if _, err := os.Stat(store + ".new"); err == nil {
		t.Fatalf("a command planted a file beside the trust store:\n%s", out)
	}
}
