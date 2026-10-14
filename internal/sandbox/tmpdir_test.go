package sandbox

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Compilers need TMPDIR (per-user /var/folders on macOS); without an allow
// builds fail "operation not permitted", which reads as an agent error.
func TestSandboxAllowsToolchainTempDir(t *testing.T) {
	requireNetNS(t)
	if runtime.GOOS != "darwin" {
		t.Skip("TMPDIR handling is macOS-specific")
	}
	if os.Getenv("TMPDIR") == "" {
		t.Skip("no TMPDIR set")
	}
	dir := workspace(t)
	s := processSandbox(t, dir, false)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	out, _ := s.Command(ctx, dir,
		`touch "$TMPDIR/abhed-probe" && echo WRITE_OK || echo WRITE_BLOCKED`).CombinedOutput()
	if !strings.Contains(string(out), "WRITE_OK") {
		t.Fatalf("toolchains cannot use TMPDIR inside the sandbox:\n%s", out)
	}
	_ = os.Remove(os.Getenv("TMPDIR") + "/abhed-probe")
}

// The real check: a Go build must actually work inside the sandbox.
func TestSandboxAllowsGoBuild(t *testing.T) {
	requireNetNS(t)
	if testing.Short() {
		t.Skip("slow")
	}
	dir := workspace(t)
	_ = os.WriteFile(dir+"/go.mod", []byte("module probe\n\ngo 1.24\n"), 0o644)
	_ = os.WriteFile(dir+"/main.go", []byte("package main\n\nfunc main() {}\n"), 0o644)

	p := DefaultPolicy(dir)
	// On Linux the process tier shows /usr, /bin and /lib; a Go installed
	// elsewhere (a CI runner's /opt) is named as read_only_paths would.
	if runtime.GOOS == "linux" {
		if root, err := exec.Command("go", "env", "GOROOT").Output(); err == nil && !insideAny(strings.TrimSpace(string(root)), []string{"/usr", "/bin", "/lib"}) {
			p.ReadOnlyPaths = append(p.ReadOnlyPaths, strings.TrimSpace(string(root)))
		}
	}
	s := NewProcess(p)
	available(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	out, err := s.Command(ctx, dir, "go build ./... 2>&1").CombinedOutput()
	if err != nil || strings.Contains(string(out), "operation not permitted") {
		t.Fatalf("go build failed inside the sandbox:\n%s", out)
	}
}
