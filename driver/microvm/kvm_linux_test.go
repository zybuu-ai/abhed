//go:build linux

package microvm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The workspace is the image user's, as a real one is the server user's.
	for _, p := range []string{ws, filepath.Join(ws, "hello.txt"), filepath.Join(ws, ".abhed")} {
		if err := os.Chown(p, 65534, 65534); err != nil {
			t.Fatal(err)
		}
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
	// Changes come back to the host as the workspace's user; .abhed never does.
	run("echo vm > made-in-vm.txt; echo bad > .abhed/config.json || true")
	made := filepath.Join(ws, "made-in-vm.txt")
	if b, err := os.ReadFile(made); err != nil || string(b) != "vm\n" {
		t.Fatalf("a change in the VM did not come back: %q %v", b, err)
	}
	if fi, err := os.Stat(made); err != nil || fi.Sys().(*syscall.Stat_t).Uid != 65534 {
		t.Fatalf("the copied-back file is not the workspace user's: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".abhed", "config.json")); err == nil {
		t.Fatal(".abhed was written from the VM")
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
	t.Run("egress", func(t *testing.T) { kvmEgress(ctx, t, o) })
	t.Run("recover", func(t *testing.T) { kvmRecover(ctx, t, o) })
	fc := d.be.(*firecracker)
	if _, err := os.Stat(fc.jailDir("sbx-0123456789abcdef")); !os.IsNotExist(err) {
		t.Fatalf("the chroot outlived the instance: %v", err)
	}
	if _, err := os.Stat(cgroupDir(fc.fc.ParentCgroup, "sbx-0123456789abcdef")); !os.IsNotExist(err) {
		t.Fatalf("the cgroup outlived the instance: %v", err)
	}
}

// kvmEgress runs a VM whose only way out is the session's egress proxy,
// with the proxy URL a command gets on the process tier, unchanged. The
// proxy here is served by root, so root is the user its socket is checked
// against.
func kvmEgress(ctx context.Context, t *testing.T, o Options) {
	port := origin(t)
	p, sock, evs := session(t, port)
	o.ImageUser = &Credential{UID: 0, GID: 0}
	o.StateDir = filepath.Join(filepath.Dir(o.StateDir), "state2")
	fc := *o.Firecracker
	fc.ChrootBase = filepath.Join(filepath.Dir(fc.ChrootBase), "jail2")
	o.Firecracker = &fc
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	const id2 = "sbx_2"
	if _, err := d.Create(ctx, driver.Spec{ID: id2, Egress: &driver.Egress{Socket: sock, Port: p.Addr().Port()}}); err != nil {
		t.Fatal(err)
	}
	call, err := p.Issue("kvm-call")
	if err != nil {
		t.Fatal(err)
	}
	defer call.End()
	get := func(host string) (int, string) {
		var out bytes.Buffer
		res, err := d.Exec(ctx, id2, driver.ExecRequest{Env: call.Env(), Stdout: &out, Stderr: &out,
			Command: fmt.Sprintf("wget -q -T 5 -O - http://%s:%d/", host, port)})
		if err != nil {
			t.Fatal(err)
		}
		return res.ExitCode, out.String()
	}
	if code, out := get("allowed.test"); code != 0 || out != "ok" {
		t.Fatalf("allowed through the proxy: %d %q", code, out)
	}
	if ev, ok := evs.find("allowed.test"); !ok || ev.CallID != "kvm-call" {
		t.Fatalf("not recorded under the call: %+v", ev)
	}
	if code, _ := get("denied.test"); code == 0 {
		t.Fatal("a destination outside the allowlist was reached")
	}
}

// kvmRecover kills a driver's VM the way a restarted driver does after the
// first one was killed: by its marker and cgroup alone.
func kvmRecover(ctx context.Context, t *testing.T, o Options) {
	o.StateDir = filepath.Join(filepath.Dir(o.StateDir), "state3")
	fc := *o.Firecracker
	fc.ChrootBase = filepath.Join(filepath.Dir(fc.ChrootBase), "jail3")
	o.Firecracker = &fc
	first, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	const id3 = "sbx_3"
	if _, err := first.Create(ctx, driver.Spec{ID: id3}); err != nil {
		t.Fatal(err)
	}
	m := first.insts[id3].m
	// Killed: its lock goes with it, and nothing else is cleaned up.
	first.unlock()
	second, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	cleaned, err := second.Recover(ctx)
	if err != nil || !strings.Contains(strings.Join(cleaned, ","), "VM "+id3) {
		t.Fatalf("recover: %q %v", cleaned, err)
	}
	select {
	case <-m.done():
	case <-time.After(10 * time.Second):
		t.Fatal("the left VM is still running")
	}
	if _, err := os.Stat(cgroupDir(fc.ParentCgroup, "sbx-3")); !os.IsNotExist(err) {
		t.Fatalf("the left VM's cgroup remains: %v", err)
	}
}
