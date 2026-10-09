//go:build linux

package microvm

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

// TestFirecrackerOnKVM boots a real microVM. It needs a KVM host and root,
// and runs only when ABHED_TEST_FIRECRACKER=1 with:
//
//	ABHED_TEST_FC_BINARY  firecracker release binary (root-owned)
//	ABHED_TEST_FC_JAILER  jailer release binary (root-owned)
//	ABHED_TEST_FC_KERNEL  uncompressed guest kernel with virtio-blk and vsock
//	ABHED_TEST_FC_ROOTFS  ext4 root filesystem from scripts/microvm/build-rootfs.sh
//
// docs/guide/22-microvm.md describes the CI job that sets these.
func TestFirecrackerOnKVM(t *testing.T) {
	if os.Getenv("ABHED_TEST_FIRECRACKER") != "1" {
		t.Skip("set ABHED_TEST_FIRECRACKER=1 on a KVM host to boot a real microVM")
	}
	env := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			t.Fatalf("%s is not set", k)
		}
		return v
	}
	// Every folder above the workspace must be passable by the image user.
	base := shortTemp(t)
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	o := Options{
		Kernel: env("ABHED_TEST_FC_KERNEL"), Rootfs: env("ABHED_TEST_FC_ROOTFS"),
		StateDir: filepath.Join(base, "state"), WorkspaceMiB: 64,
		ImageUser: &Credential{UID: 65534, GID: 65534},
		Firecracker: &Firecracker{
			Binary: env("ABHED_TEST_FC_BINARY"), Jailer: env("ABHED_TEST_FC_JAILER"),
			ChrootBase: filepath.Join(base, "jail"), UIDBase: 910000, GIDBase: 910000, Slots: 4,
			ParentCgroup: "abhed-microvm-test",
		},
	}
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("from the host\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	st, err := d.Create(ctx, driver.Spec{ID: id, Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if st.Tier != Tier || st.Backend != "firecracker" {
		t.Fatalf("status %+v", st)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })

	run := func(cmd string) (int, string) {
		t.Helper()
		var out, errb bytes.Buffer
		res, err := d.Exec(ctx, id, driver.ExecRequest{Command: cmd, Stdout: &out, Stderr: &errb})
		if err != nil {
			t.Fatalf("%s: %v (stderr %q)", cmd, err, errb.String())
		}
		return res.ExitCode, out.String()
	}
	if code, out := run("cat hello.txt"); code != 0 || out != "from the host\n" {
		t.Fatalf("workspace: %d %q", code, out)
	}
	if code, _ := run("exit 42"); code != 42 {
		t.Fatalf("exit code %d", code)
	}
	if _, out := run("id -u"); strings.TrimSpace(out) != "1000" {
		t.Fatalf("commands run as %q, not the unprivileged guest user", out)
	}
	if _, out := run("ls /sys/class/net"); strings.TrimSpace(out) != "lo" {
		t.Fatalf("the guest has network interfaces: %q", out)
	}
	if code, _ := run("echo x > /etc/abhed-test"); code == 0 {
		t.Fatal("the root filesystem is writable")
	}
	if code, _ := run("kill -9 1"); code == 0 {
		t.Fatal("a command could signal the guest's init")
	}

	if _, err := d.Stop(ctx, id, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, out := run("cat hello.txt"); out != "from the host\n" {
		t.Fatal("the workspace did not survive a restart")
	}
	if err := d.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	fc := d.be.(*firecracker)
	if _, err := os.Stat(fc.jailDir("sbx-0123456789abcdef")); !os.IsNotExist(err) {
		t.Fatalf("the chroot outlived the instance: %v", err)
	}
	if _, err := os.Stat(cgroupDir(fc.fc.ParentCgroup, "sbx-0123456789abcdef")); !os.IsNotExist(err) {
		t.Fatalf("the cgroup outlived the instance: %v", err)
	}
}
