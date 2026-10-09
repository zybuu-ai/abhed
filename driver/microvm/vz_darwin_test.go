package microvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

// entitlements is the file the helper is signed with.
var entitlements, _ = filepath.Abs("../../macos/abhed-vmhelper/abhed-vmhelper.entitlements")

func init() {
	signFakeHelper = func(t *testing.T, path string) string {
		return signedCopy(t, path, "-i", HelperIdentifier, "--entitlements", entitlements, "-o", "runtime")
	}
}

// signedCopy copies a binary and signs the copy ad hoc with args; with no
// args, it removes the signature.
func signedCopy(t *testing.T, src string, args ...string) string {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), HelperName)
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := []string{"--remove-signature", dst}
	if len(args) > 0 {
		cmd = append(append([]string{"-f", "-s", "-"}, args...), dst)
	}
	if out, err := exec.Command(codesign, cmd...).CombinedOutput(); err != nil {
		t.Fatalf("codesign %q: %v: %s", cmd, err, out)
	}
	return dst
}

func verifyFile(path, team string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	return verifyHelper(context.Background(), path, team, fi)
}

func TestHelperSignatureIsChecked(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	good := signedCopy(t, self, "-i", HelperIdentifier, "--entitlements", entitlements, "-o", "runtime")
	if err := verifyFile(good, ""); err != nil {
		t.Fatalf("a correctly signed helper is refused: %v", err)
	}
	for name, tc := range map[string]struct {
		path, team, want string
	}{
		"unsigned":          {signedCopy(t, self), "", "is not signed as " + HelperIdentifier},
		"another program":   {signedCopy(t, self, "-i", "com.example.other", "--entitlements", entitlements, "-o", "runtime"), "", "is not signed as"},
		"no entitlement":    {signedCopy(t, self, "-i", HelperIdentifier, "-o", "runtime"), "", "com.apple.security.virtualization"},
		"no runtime":        {signedCopy(t, self, "-i", HelperIdentifier, "--entitlements", entitlements), "", "hardened runtime"},
		"ad hoc for a team": {good, "AB12CD34EF", "by Apple Developer team AB12CD34EF"},
	} {
		if err := verifyFile(tc.path, tc.team); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A helper changed after it was checked is checked again, and refused.
	f, err := os.OpenFile(good, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte{0})
	_ = f.Close()
	if err := verifyFile(good, ""); err == nil {
		t.Fatal("a helper modified after signing is accepted")
	}
	// And the probe refuses it before running it.
	if err := probeHelper(good, ""); err == nil || !strings.Contains(err.Error(), "is not signed") {
		t.Fatalf("probe of a modified helper: %v", err)
	}
}

func TestHelperRequirement(t *testing.T) {
	if got := helperRequirement(""); got != `identifier "com.zybuu.abhed.vmhelper"` {
		t.Fatal(got)
	}
	want := `identifier "com.zybuu.abhed.vmhelper" and anchor apple generic and certificate leaf[subject.OU] = "AB12CD34EF"`
	if got := helperRequirement("AB12CD34EF"); got != want {
		t.Fatal(got)
	}
}

func TestPlistAndFlags(t *testing.T) {
	head := `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "https://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0">`
	for doc, want := range map[string]bool{
		head + `<dict><key>com.apple.security.virtualization</key><true/></dict></plist>`:                               true,
		head + `<dict><key>com.apple.security.virtualization</key><false/></dict></plist>`:                              false,
		head + `<dict><key>other</key><true/></dict></plist>`:                                                           false,
		head + `<dict><key>x</key><dict><key>com.apple.security.virtualization</key><true/></dict></dict></plist>`:      false,
		head + `<dict><key>x</key><string>com.apple.security.virtualization</string><key>y</key><true/></dict></plist>`: false,
		head + `<dict><key>a</key><false/><key>com.apple.security.virtualization</key><true/></dict></plist>`:           true,
		``: false,
	} {
		if got, _ := plistTrue([]byte(doc), "com.apple.security.virtualization"); got != want {
			t.Errorf("%s: %v", doc, got)
		}
	}
	for info, want := range map[string]bool{
		"CodeDirectory v=20500 size=729 flags=0x10002(adhoc,runtime) hashes=12+7": true,
		"CodeDirectory v=20400 size=729 flags=0x2(adhoc) hashes=12+7":             false,
		"Identifier=x flags=0x10000(runtime)":                                     false,
		"":                                                                        false,
	} {
		if hardenedRuntime(info) != want {
			t.Errorf("%q: %v", info, !want)
		}
	}
}

// vzHelper is the built helper, signed, from ABHED_TEST_VZ_HELPER. The
// tests that use it run when it is set; CI sets it in the macos-vmhelper
// job and checks they passed.
func vzHelper(t *testing.T) string {
	t.Helper()
	p := os.Getenv("ABHED_TEST_VZ_HELPER")
	if p == "" {
		t.Skip("set ABHED_TEST_VZ_HELPER to a built, signed abhed-vmhelper (scripts/macos/build-vmhelper.sh)")
	}
	return p
}

type checkAnswer struct {
	Protocol  int    `json:"protocol"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
}

func runCheckOf(t *testing.T, path string) checkAnswer {
	t.Helper()
	out, err := exec.Command(path, "check").Output()
	if err != nil {
		t.Fatalf("%s check: %v", path, err)
	}
	var c checkAnswer
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || c.Protocol != HelperProtocol {
		t.Fatalf("check answered %q: %v", out, err)
	}
	return c
}

// The real helper, without booting anything: its signature, its check, and
// the configurations it refuses.
func TestVZHelperWithoutBooting(t *testing.T) {
	h := vzHelper(t)
	if err := verifyFile(h, os.Getenv("ABHED_TEST_VZ_TEAM_ID")); err != nil {
		t.Fatal(err)
	}
	c := runCheckOf(t, h)
	if strings.Contains(c.Reason, "entitlement") {
		t.Fatalf("the signed helper does not see its entitlement: %+v", c)
	}
	t.Logf("check: %+v", c)
	unentitled := runCheckOf(t, signedCopy(t, h, "-i", HelperIdentifier, "-o", "runtime"))
	if unentitled.Supported || !strings.Contains(unentitled.Reason, "com.apple.security.virtualization") {
		t.Fatalf("a helper without the entitlement answered %+v", unentitled)
	}

	dir := shortTemp(t)
	kernel, disk := filepath.Join(dir, "kernel"), filepath.Join(dir, "disk.img")
	for _, p := range []string{kernel, disk} {
		if err := os.WriteFile(p, make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := func() map[string]any {
		return map[string]any{
			"protocol": 1, "kernel": kernel, "boot_args": "console=hvc0", "cpus": 1, "memory_mib": 256,
			"disks":        []any{map[string]any{"path": disk, "read_only": true}},
			"vsock_socket": filepath.Join(dir, "v.sock"), "network": "none",
		}
	}
	for name, tc := range map[string]struct {
		edit func(m map[string]any)
		want string
	}{
		"unknown key":      {func(m map[string]any) { m["shared_folders"] = []any{"/"} }, `"shared_folders"`},
		"unknown disk key": {func(m map[string]any) { m["disks"].([]any)[0].(map[string]any)["cache"] = "none" }, `"cache"`},
		"unknown port key": {func(m map[string]any) {
			m["guest_ports"] = []any{map[string]any{"port": 1025, "socket": "/tmp/x", "tcp": 1}}
		}, `"tcp"`},
		"a network":         {func(m map[string]any) { m["network"] = "nat" }, "never gives a VM a network device"},
		"no network key":    {func(m map[string]any) { delete(m, "network") }, "network is missing"},
		"protocol 2":        {func(m map[string]any) { m["protocol"] = 2 }, "protocol 2"},
		"relative kernel":   {func(m map[string]any) { m["kernel"] = "kernel" }, "absolute path"},
		"missing kernel":    {func(m map[string]any) { m["kernel"] = kernel + ".no" }, "No such file"},
		"folder as disk":    {func(m map[string]any) { m["disks"].([]any)[0].(map[string]any)["path"] = dir }, "not a regular file"},
		"no disks":          {func(m map[string]any) { m["disks"] = []any{} }, "disks must be"},
		"no cpus":           {func(m map[string]any) { m["cpus"] = 0 }, "cpus 0"},
		"too many cpus":     {func(m map[string]any) { m["cpus"] = 4096 }, "cpus 4096"},
		"fractional memory": {func(m map[string]any) { m["memory_mib"] = 256.5 }, "whole number"},
		"too little memory": {func(m map[string]any) { m["memory_mib"] = 1 }, "memory_mib 1"},
		"too much memory":   {func(m map[string]any) { m["memory_mib"] = 1 << 30 }, "memory_mib"},
		"long socket":       {func(m map[string]any) { m["vsock_socket"] = "/" + strings.Repeat("s", 120) }, "longer than"},
		"port twice": {func(m map[string]any) {
			m["guest_ports"] = []any{map[string]any{"port": 1025, "socket": "/tmp/a"}, map[string]any{"port": 1025, "socket": "/tmp/b"}}
		}, "listed twice"},
		"bool as number": {func(m map[string]any) { m["cpus"] = true }, "whole number"},
	} {
		m := base()
		tc.edit(m)
		b, _ := json.Marshal(m)
		cfg := filepath.Join(dir, "vm.json")
		if err := os.WriteFile(cfg, b, 0o600); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		cmd := exec.Command(h, "run", "--config", cfg)
		cmd.Stderr = &stderr
		err := cmd.Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%s: %v %q", name, err, stderr.String())
		}
	}
	for _, args := range [][]string{{}, {"run"}, {"run", "--config"}, {"boot"}, {"check", "x"}} {
		if err := exec.Command(h, args...).Run(); err == nil {
			t.Errorf("%q accepted", args)
		} else if ee := (*exec.ExitError)(nil); !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Errorf("%q: %v", args, err)
		}
	}
}

// TestVZHelperBootsAMicroVM boots real VMs on this Mac. It runs only when
// ABHED_TEST_VZ=1, with ABHED_TEST_VZ_HELPER and:
//
//	ABHED_TEST_VZ_KERNEL  an arm64 Image with virtio PCI, block, console and vsock built in
//	ABHED_TEST_VZ_ROOTFS  an arm64 root filesystem from scripts/microvm/build-rootfs.sh
//	ABHED_TEST_VZ_MKFS    mkfs.ext4 from e2fsprogs
//
// scripts/macos/guest-image.sh fetches and builds the first two. Each VM
// has one vCPU and 256 MiB.
func TestVZHelperBootsAMicroVM(t *testing.T) {
	if os.Getenv("ABHED_TEST_VZ") != "1" {
		t.Skip("set ABHED_TEST_VZ=1 on a Mac that can run VMs to boot a real microVM")
	}
	env := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			t.Fatalf("%s is not set", k)
		}
		return v
	}
	base := shortTemp(t)
	o := Options{
		Kernel: env("ABHED_TEST_VZ_KERNEL"), Rootfs: env("ABHED_TEST_VZ_ROOTFS"), MkfsExt4: env("ABHED_TEST_VZ_MKFS"),
		StateDir: filepath.Join(base, "state"), VCPUs: 1, MemoryMiB: 256, MaxMemoryMiB: 512, WorkspaceMiB: 64,
		Helper: &Helper{Path: vzHelper(t), TeamID: os.Getenv("ABHED_TEST_VZ_TEAM_ID")},
	}
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
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
	if st.Tier != Tier || st.Backend != "vz-helper" {
		t.Fatalf("status %+v", st)
	}
	pid := d.insts[id].m.(*procMachine).cmd.Process.Pid
	run := func(cmd string) (int, string) {
		t.Helper()
		var out, errb bytes.Buffer
		res, err := d.Exec(ctx, id, driver.ExecRequest{Command: cmd, Stdout: &out, Stderr: &errb})
		if err != nil {
			t.Fatalf("%s: %v (stderr %q)", cmd, err, errb.String())
		}
		return res.ExitCode, out.String()
	}
	if code, out := run("echo hello from the VM"); code != 0 || out != "hello from the VM\n" {
		t.Fatalf("echo: %d %q", code, out)
	}
	if code, out := run("cat hello.txt"); code != 0 || out != "from the host\n" {
		t.Fatalf("workspace: %d %q", code, out)
	}
	if code, _ := run("exit 42"); code != 42 {
		t.Fatalf("exit code %d", code)
	}
	if _, out := run("uname -m"); strings.TrimSpace(out) != "aarch64" {
		t.Fatalf("not an arm64 guest: %q", out)
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
	// Shared folders would show up as virtiofs mounts or devices.
	if _, out := run("cat /proc/mounts; ls /sys/bus/virtio/drivers/*/ 2>/dev/null"); strings.Contains(out, "virtiofs") || strings.Contains(out, "9p") {
		t.Fatalf("the guest has a shared folder: %q", out)
	}
	run("echo vm > made-in-vm.txt; echo bad > .abhed/config.json || true")
	if b, err := os.ReadFile(filepath.Join(ws, "made-in-vm.txt")); err != nil || string(b) != "vm\n" {
		t.Fatalf("a change in the VM did not come back: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".abhed", "config.json")); err == nil {
		t.Fatal(".abhed was written from the VM")
	}
	if _, err := d.Stop(ctx, id, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	gone(t, pid)
	if _, err := d.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	pid = d.insts[id].m.(*procMachine).cmd.Process.Pid
	if _, out := run("cat made-in-vm.txt"); out != "vm\n" {
		t.Fatal("the workspace did not survive a restart")
	}
	if err := d.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	gone(t, pid)
	if _, err := os.Stat(filepath.Join(o.StateDir, id)); !os.IsNotExist(err) {
		t.Fatalf("the instance's state outlived it: %v", err)
	}
	t.Run("egress", func(t *testing.T) { vzEgress(ctx, t, o) })
	t.Run("stdin", func(t *testing.T) { vzEndsWithStdin(ctx, t, o) })
}

// gone waits for a helper process to have exited.
func gone(t *testing.T, pid int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the helper %d is still running", pid)
}

// vzEgress reaches an allowed destination only through the session's proxy,
// over the helper's guest port.
func vzEgress(ctx context.Context, t *testing.T, o Options) {
	port := origin(t)
	p, sock, evs := session(t, port)
	o.StateDir = filepath.Join(filepath.Dir(o.StateDir), "state2")
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	const id2 = "sbx_2"
	if _, err := d.Create(ctx, driver.Spec{ID: id2, Egress: &driver.Egress{Socket: sock, Port: p.Addr().Port()}}); err != nil {
		t.Fatal(err)
	}
	call, err := p.Issue("vz-call")
	if err != nil {
		t.Fatal(err)
	}
	defer call.End()
	get := func(host string, env []string) (int, string) {
		var out bytes.Buffer
		res, err := d.Exec(ctx, id2, driver.ExecRequest{Env: env, Stdout: &out, Stderr: &out,
			Command: fmt.Sprintf("wget -q -T 5 -O - http://%s:%d/", host, port)})
		if err != nil {
			t.Fatal(err)
		}
		return res.ExitCode, out.String()
	}
	if code, out := get("allowed.test", call.Env()); code != 0 || out != "ok" {
		t.Fatalf("allowed through the proxy: %d %q", code, out)
	}
	if ev, ok := evs.find("allowed.test"); !ok || ev.CallID != "vz-call" {
		t.Fatalf("not recorded under the call: %+v", ev)
	}
	if code, _ := get("denied.test", call.Env()); code == 0 {
		t.Fatal("a destination outside the allowlist was reached")
	}
	if code, _ := get("127.0.0.1", nil); code == 0 {
		t.Fatal("the host was reached without the proxy")
	}
	if err := d.Delete(ctx, id2); err != nil {
		t.Fatal(err)
	}
}

// vzEndsWithStdin is what happens when the driver dies: its end of the
// helper's standard input closes, and the helper stops the VM and exits 0.
func vzEndsWithStdin(ctx context.Context, t *testing.T, o Options) {
	o.StateDir = filepath.Join(filepath.Dir(o.StateDir), "state3")
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	if _, err := d.Create(ctx, driver.Spec{ID: id}); err != nil {
		t.Fatal(err)
	}
	m := d.insts[id].m.(*procMachine)
	if err := m.reap(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.done():
	case <-time.After(10 * time.Second):
		t.Fatal("the helper outlived its driver's end of stdin")
	}
	if code := m.cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("the helper exited %d after its stdin ended; console: %s", code, m.console())
	}
	gone(t, m.cmd.Process.Pid)
	_ = d.Delete(ctx, id)
}
