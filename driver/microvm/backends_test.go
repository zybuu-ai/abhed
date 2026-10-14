//go:build !windows

package microvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

func fcOptions(t *testing.T) Options {
	t.Helper()
	o := testOptions(t)
	o.Firecracker = &Firecracker{Binary: "/opt/fc/firecracker", Jailer: "/opt/fc/jailer", ChrootBase: filepath.Join(filepath.Dir(o.StateDir), "jail"), UIDBase: 200000, GIDBase: 300000}
	return o
}

func TestJailerArgs(t *testing.T) {
	o := fcOptions(t)
	fc, err := newFirecracker(&o)
	if err != nil {
		t.Fatal(err)
	}
	got := fc.jailerArgs(bootSpec{vmID: "sbx-1", slot: 3, vcpus: 2, memMiB: 512})
	want := []string{
		"--id", "sbx-1",
		"--exec-file", "/opt/fc/firecracker",
		"--uid", "200003",
		"--gid", "300003",
		"--chroot-base-dir", o.Firecracker.ChrootBase,
		"--cgroup-version", "2",
		"--parent-cgroup", "abhed-microvm",
		"--cgroup", "cpu.max=200000 100000",
		"--cgroup", "memory.max=805306368",
		"--cgroup", "memory.swap.max=0",
		"--cgroup", "pids.max=66",
		"--resource-limit", "no-file=2048",
		"--",
		"--config-file", "/vm.json",
		"--no-api",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("jailer args\n got %q\nwant %q", got, want)
	}

	o.Firecracker.SeccompFilter = "/etc/fc/filter.bpf"
	fc, _ = newFirecracker(&o)
	got = fc.jailerArgs(bootSpec{vmID: "sbx-1", vcpus: 1, memMiB: 128})
	if got[len(got)-2] != "--seccomp-filter" || got[len(got)-1] != "/seccomp.bpf" {
		t.Fatalf("custom filter not passed: %q", got)
	}
	for _, a := range got {
		if a == "--no-seccomp" || strings.HasPrefix(a, "--seccomp-level") {
			t.Fatalf("seccomp weakened: %q", got)
		}
	}
}

func TestFirecrackerConfigHasNoNetwork(t *testing.T) {
	b, err := fcConfig(bootSpec{vcpus: 2, memMiB: 300, bootArgs: "console=ttyS0 init=/sbin/abhed-guest", workspace: "/x"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["network-interfaces"]; ok {
		t.Fatal("the VM was given a NIC")
	}
	if len(cfg) != 4 {
		t.Fatalf("unexpected sections: %s", b)
	}
	drives := cfg["drives"].([]any)
	root, ws := drives[0].(map[string]any), drives[1].(map[string]any)
	if root["is_root_device"] != true || root["is_read_only"] != true || root["path_on_host"] != chRootfs {
		t.Fatalf("root drive %v", root)
	}
	if ws["is_root_device"] != false || ws["is_read_only"] != false || ws["path_on_host"] != chWorkspace {
		t.Fatalf("workspace drive %v", ws)
	}
	mc := cfg["machine-config"].(map[string]any)
	if mc["vcpu_count"] != 2.0 || mc["mem_size_mib"] != 300.0 || mc["smt"] != false {
		t.Fatalf("machine config %v", mc)
	}
	if vs := cfg["vsock"].(map[string]any); vs["uds_path"] != chVsock || vs["guest_cid"] != 3.0 {
		t.Fatalf("vsock %v", vs)
	}

	b, _ = fcConfig(bootSpec{vcpus: 1, memMiB: 128})
	_ = json.Unmarshal(b, &cfg)
	if n := len(cfg["drives"].([]any)); n != 1 {
		t.Fatalf("%d drives without a workspace", n)
	}
}

type chownLog struct {
	mu    sync.Mutex
	paths map[string][2]int
}

func (c *chownLog) chown(p string, uid, gid int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths[filepath.Base(p)] = [2]int{uid, gid}
	return nil
}

func TestLayoutGivesOnlyPrivateFilesToTheVMUser(t *testing.T) {
	o := fcOptions(t)
	fc, _ := newFirecracker(&o)
	log := &chownLog{paths: map[string][2]int{}}
	fc.chown = log.chown
	ws := filepath.Join(o.StateDir, "workspace.ext4")
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws, []byte("img"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fc.layout(bootSpec{kernel: o.Kernel, rootfs: o.Rootfs, workspace: ws, vcpus: 1, memMiB: 128}, root, 7, 8); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"vmlinux", "rootfs.ext4", "workspace.ext4", "vm.json"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Fatalf("%s not placed: %v", f, err)
		}
	}
	if log.paths["workspace.ext4"] != [2]int{7, 8} || log.paths["vm.json"] != [2]int{7, 8} {
		t.Fatalf("the VM's own files were not given to it: %v", log.paths)
	}
	if _, ok := log.paths["vmlinux"]; ok {
		t.Fatal("a shared, linked kernel was given to one VM's user")
	}

	// A shared image only root can read cannot be linked in for the VM.
	if err := os.Chmod(o.Kernel, 0o600); err != nil {
		t.Fatal(err)
	}
	root2 := filepath.Join(t.TempDir(), "root")
	_ = os.MkdirAll(root2, 0o700)
	if err := fc.layout(bootSpec{kernel: o.Kernel, rootfs: o.Rootfs, vcpus: 1, memMiB: 128}, root2, 7, 8); err == nil || !strings.Contains(err.Error(), "readable") {
		t.Fatalf("an unreadable shared kernel was linked: %v", err)
	}
}

// fakeVMM points a backend at this test binary acting as the VMM.
func fakeVMM(t *testing.T, role string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ABHED_FAKE_VMM", role)
	return self
}

func roundTrip(t *testing.T, d *Driver) {
	t.Helper()
	ctx := context.Background()
	st, err := d.Create(ctx, driver.Spec{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if st.Tier != Tier || st.Phase != driver.PhaseRunning {
		t.Fatalf("status %+v", st)
	}
	var out bytes.Buffer
	res, err := d.Exec(ctx, id, driver.ExecRequest{Command: "echo $ABHED_SANDBOX; exit 9", Stdout: &out})
	if err != nil || res.ExitCode != 9 || out.String() != "microvm\n" {
		t.Fatalf("exec: %+v %q %v", res, out.String(), err)
	}
	if st, err := d.Stop(ctx, id, 5*time.Second); err != nil || st.Phase != driver.PhaseStopped {
		t.Fatalf("stop: %+v %v", st, err)
	}
	if _, err := d.Start(ctx, id); err != nil {
		t.Fatalf("start again: %v", err)
	}
	if err := d.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
}

// The firecracker backend end to end, with the jailer replaced: chroot
// layout, configuration file, process handling and the vsock socket.
func TestFirecrackerBackendWithFakeJailer(t *testing.T) {
	o := fcOptions(t)
	o.Firecracker.Jailer = fakeVMM(t, "jailer")
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	fc := d.be.(*firecracker)
	fc.probe = func() error { return nil }
	fc.chown = func(string, int, int) error { return nil }
	fc.env = []string{"ABHED_FAKE_VMM=jailer"}
	roundTrip(t, d)
	if _, err := os.Stat(fc.jailDir("sbx-0123456789abcdef")); !os.IsNotExist(err) {
		t.Fatalf("the chroot outlived the instance: %v", err)
	}
}

func TestFirecrackerBootTimeoutKillsTheJailer(t *testing.T) {
	o := fcOptions(t)
	o.Firecracker.Jailer = fakeVMM(t, "jailer")
	o.BootTimeoutSeconds = 1
	d, _ := New(o)
	fc := d.be.(*firecracker)
	fc.probe = func() error { return nil }
	fc.chown = func(string, int, int) error { return nil }
	fc.env = []string{"ABHED_FAKE_VMM=jailer", "ABHED_FAKE_VMM_HANG=1"}
	start := time.Now()
	if _, err := d.Create(context.Background(), driver.Spec{ID: id}); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("create with a hung VMM: %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatal("the hung VMM was not killed promptly")
	}
	if _, err := os.Stat(fc.jailDir("sbx-0123456789abcdef")); !os.IsNotExist(err) {
		t.Fatalf("the chroot outlived the failed boot: %v", err)
	}
}

// signFakeHelper returns the fake helper signed as a real one must be, on
// macOS where the probe checks the signature.
var signFakeHelper = func(_ *testing.T, path string) string { return path }

func TestHelperBackendWithFakeHelper(t *testing.T) {
	o := testOptions(t)
	o.Helper = &Helper{Path: signFakeHelper(t, fakeVMM(t, "helper"))}
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	h := d.be.(*helper)
	h.env = []string{"ABHED_FAKE_VMM=helper"}
	// The real probe checks the signature and runs `check`, which the fake
	// answers; elsewhere than macOS the backend is refused before that.
	if runtime.GOOS != "darwin" {
		h.probe = func() error { return runHelperCheck(context.Background(), h.h.Path) }
	}
	roundTrip(t, d)
}

func TestHelperConfig(t *testing.T) {
	o := testOptions(t)
	o.Helper = &Helper{}
	h, err := newHelper(&o)
	if err != nil {
		t.Fatal(err)
	}
	c := h.config(bootSpec{kernel: "/k", rootfs: "/r", workspace: "/w", vcpus: 2, memMiB: 1024, bootArgs: o.BootArgs}, "/s/v.sock")
	want := helperConfig{Protocol: 1, Kernel: "/k", BootArgs: defaultHelperBootArgs, CPUs: 2, MemoryMiB: 1024,
		Disks: []helperDisk{{Path: "/r", ReadOnly: true}, {Path: "/w"}}, VsockSocket: "/s/v.sock", Network: "none"}
	got, _ := json.Marshal(c)
	exp, _ := json.Marshal(want)
	if !bytes.Equal(got, exp) {
		t.Fatalf("helper config\n got %s\nwant %s", got, exp)
	}
}

func TestHelperCheckAnswers(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		script string
		ok     bool
		msg    string
	}{
		"supported":   {`echo '{"protocol":1,"supported":true}'`, true, ""},
		"unsupported": {`echo '{"protocol":1,"supported":false,"reason":"no entitlement"}'`, false, "no entitlement"},
		"protocol":    {`echo '{"protocol":2,"supported":true}'`, false, "protocol 2"},
		"garbage":     {`echo nope`, false, "unreadable"},
		"fails":       {`exit 3`, false, "exit status 3"},
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := runHelperCheck(context.Background(), p)
		if (err == nil) != tc.ok || (err != nil && !strings.Contains(err.Error(), tc.msg)) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestHelperOptions(t *testing.T) {
	o := testOptions(t)
	o.Helper = &Helper{}
	h, err := newHelper(&o)
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	if want := filepath.Join(filepath.Dir(exe), HelperName); h.h.Path != want {
		t.Fatalf("default helper path %q, want %q", h.h.Path, want)
	}
	for _, team := range []string{"abcdefghij", "ABCDEFGHI", "ABCDEFGHIJK", `ABCDE"FGHI`} {
		o := testOptions(t)
		o.Helper = &Helper{TeamID: team}
		if _, err := newHelper(&o); err == nil || !strings.Contains(err.Error(), "team_id") {
			t.Errorf("team id %q accepted: %v", team, err)
		}
	}
	o = testOptions(t)
	o.Helper = &Helper{TeamID: "AB12CD34EF"}
	if _, err := newHelper(&o); err != nil {
		t.Fatal(err)
	}
}

func TestHelperRefusedWithoutAHelper(t *testing.T) {
	o := testOptions(t)
	o.Helper = &Helper{}
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Create(context.Background(), driver.Spec{ID: id})
	if !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("create without a helper: %v", err)
	}
	if runtime.GOOS == "darwin" {
		// It names what is missing and where it was looked for.
		h := d.be.(*helper)
		if !strings.Contains(err.Error(), "com.apple.security.virtualization") || !strings.Contains(err.Error(), h.h.Path) {
			t.Fatalf("the refusal does not say what is missing: %v", err)
		}
	} else if !strings.Contains(err.Error(), "only on macOS") {
		t.Fatalf("refusal off macOS: %v", err)
	}
}

func TestFirecrackerRefusedOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("covered by the probe on Linux")
	}
	o := fcOptions(t)
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(context.Background(), driver.Spec{ID: id}); !errors.Is(err, driver.ErrUnavailable) || !strings.Contains(err.Error(), "Linux with KVM") {
		t.Fatalf("firecracker off Linux: %v", err)
	}
}

func TestConsoleIsBoundedAndPrintable(t *testing.T) {
	tb := &tailBuffer{max: 8 << 10}
	_, _ = tb.Write([]byte(strings.Repeat("a", 10000) + "\x1b[31mred\x07\u009bend"))
	s := tb.String()
	if len(s) > 2048 || strings.ContainsAny(s, "\x1b\x07\u009b") || !strings.HasSuffix(s, "[31mredend") {
		t.Fatalf("console tail %q", s[max(0, len(s)-40):])
	}
}

// The jailer names a VM's folder after the binary it runs, links resolved.
func TestJailFolderFollowsTheBinarysLink(t *testing.T) {
	o := fcOptions(t)
	dir := t.TempDir()
	real := filepath.Join(dir, "firecracker-v1.17.0-x86_64")
	_ = os.WriteFile(real, nil, 0o755)
	o.Firecracker.Binary = filepath.Join(dir, "firecracker")
	if err := os.Symlink(real, o.Firecracker.Binary); err != nil {
		t.Fatal(err)
	}
	fc, err := newFirecracker(&o)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fc.jailDir("sbx-1"), filepath.Join(o.Firecracker.ChrootBase, "firecracker-v1.17.0-x86_64", "sbx-1"); got != want {
		t.Fatalf("jail %s, want %s", got, want)
	}
}
