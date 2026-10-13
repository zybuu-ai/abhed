package sandbox

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// rootSSHSessions counts the podman machine's privileged sshd sessions;
// `podman machine ssh` itself leaves none behind.
func rootSSHSessions(t *testing.T, podman string) int {
	t.Helper()
	out, err := exec.Command(podman, "machine", "ssh", "--", "ps", "-eo", "args").Output() // #nosec G204 -- the host's podman, fixed arguments
	if err != nil {
		t.Fatalf("podman machine ssh: %v", err)
	}
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "sshd: root [priv]") {
			n++
		}
	}
	return n
}

// Choosing a tier and running a command on a podman machine leaves no root
// ssh session behind in its VM, as Podman 5.2's CLI over ssh does. Opt in
// with ABHED_TEST_SSHD_LEAK=1 on a Mac with a running machine;
// ABHED_TEST_SSHD_IMAGE names an image already pulled.
func TestPodmanMachineLeavesNoSSHSessions(t *testing.T) {
	if os.Getenv("ABHED_TEST_SSHD_LEAK") != "1" {
		t.Skip("set ABHED_TEST_SSHD_LEAK=1 to run against the podman machine")
	}
	podman, err := exec.LookPath("podman")
	if err != nil {
		t.Skip("no podman")
	}
	if img := os.Getenv("ABHED_TEST_SSHD_IMAGE"); img != "" {
		old := Image
		Image = img
		t.Cleanup(func() { Image = old })
	}
	before := rootSSHSessions(t, podman)
	ws := t.TempDir()
	var sb Sandbox
	for range 10 {
		resetEngineProbes()
		if sb, err = Select(Policy{Workspace: ws, MinTier: TierContainer}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if out, err := sb.Command(ctx, ws, "true").CombinedOutput(); err != nil {
		t.Fatalf("a command on the container tier: %v: %s", err, out)
	}
	after := rootSSHSessions(t, podman)
	t.Logf("root sshd sessions in the VM: %d before, %d after", before, after)
	if after != before {
		t.Fatalf("%d root ssh session(s) left in the VM", after-before)
	}
}
