//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// engineContainer is a container tier on the engine this machine has. It
// runs only with ABHED_REQUIRE_CONTAINER set, as the CI job sets it, since it
// pulls an image and starts containers; there a missing engine fails.
func engineContainer(t *testing.T, p Policy) *Container {
	t.Helper()
	if os.Getenv("ABHED_REQUIRE_CONTAINER") == "" {
		t.Skip("set ABHED_REQUIRE_CONTAINER=1 to run the container tier on a real engine")
	}
	c := NewContainer(p)
	if ok, why := c.Available(); !ok {
		t.Fatalf("no container engine: %s", why)
	}
	return c
}

// runInEngine runs command in c and returns its output and error.
func runInEngine(t *testing.T, c *Container, ws, command string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out, err := c.Command(ctx, ws, command).CombinedOutput()
	return string(out), err
}

// The container tier's flags hold on a real engine: the workspace is the
// one writable host path, the network is off, the memory bound includes swap,
// the process bound is the pids limit, and the host's name and PIDs are hidden.
func TestContainerTierOnARealEngine(t *testing.T) {
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	c := engineContainer(t, Policy{Workspace: ws, MaxMemoryMB: 256, MaxProcs: 64})

	if out, err := runInEngine(t, c, ws, "echo inside > made.txt"); err != nil {
		t.Fatalf("a workspace write failed: %v: %s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "made.txt")); strings.TrimSpace(string(b)) != "inside" {
		t.Fatalf("the workspace write did not reach the host: %q", b)
	}
	if out, err := runInEngine(t, c, ws, "echo x > /etc/abhed-test"); err == nil {
		t.Fatalf("the root filesystem was writable: %s", out)
	}
	if out, err := runInEngine(t, c, ws, "cat /proc/net/route | wc -l; ls /sys/class/net"); err != nil || strings.Contains(out, "eth0") {
		t.Fatalf("the network was not off: %v: %s", err, out)
	}
	for file, want := range map[string]string{
		"/sys/fs/cgroup/pids.max":        "64",
		"/sys/fs/cgroup/memory.max":      "268435456",
		"/sys/fs/cgroup/memory.swap.max": "0",
	} {
		out, err := runInEngine(t, c, ws, "cat "+file)
		if err != nil {
			t.Fatalf("%s not readable, so the limit is unchecked (cgroup v1?): %v: %s", file, err, out)
		}
		if got := strings.TrimSpace(out); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}
	if out, err := runInEngine(t, c, ws, "cat /proc/sys/kernel/hostname; echo $$"); err != nil || !strings.HasPrefix(out, "abhed\n1\n") {
		t.Fatalf("hostname and pid 1: %v: %q", err, out)
	}
	// Abhed's state is hidden: its files can't be read, and a write never reaches the host.
	state := filepath.Join(ws, stateDir)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "secret"), []byte("host-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _ := runInEngine(t, c, ws, "cat "+stateDir+"/secret; echo x > "+stateDir+"/secret; echo y > "+stateDir+"/made"); strings.Contains(out, "host-state") {
		t.Fatalf("%s was readable inside: %q", stateDir, out)
	}
	if b, _ := os.ReadFile(filepath.Join(state, "secret")); string(b) != "host-state" {
		t.Errorf("a write inside changed %s/secret on the host: %q", stateDir, b)
	}
	if _, err := os.Stat(filepath.Join(state, "made")); err == nil {
		t.Errorf("a file made in %s inside reached the host", stateDir)
	}
}
