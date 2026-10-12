package microvm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeCgroup is a VM's cgroup folder under a moved cgroupRoot, with the
// files the kernel gives one: cgroup.kill only where the kernel has it.
func fakeCgroup(t *testing.T, withKill bool) string {
	t.Helper()
	old := cgroupRoot
	t.Cleanup(func() { cgroupRoot = old })
	cgroupRoot = t.TempDir()
	dir := filepath.Join(cgroupRoot, "abhed-microvm", "vm1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if withKill {
		if err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), nil, 0o644); err != nil { // #nosec G306 -- a test's stand-in
			t.Fatal(err)
		}
	}
	return dir
}

// A watchdog whose driver ends without saying so kills the VM's cgroup; one
// told the driver ended the VM leaves it, and odd names are refused.
func TestWatchdogKillsTheCgroupWhenTheDriverDies(t *testing.T) {
	dir := fakeCgroup(t, true)
	kill := filepath.Join(dir, "cgroup.kill")
	if code := runWatchdog("abhed-microvm", "vm1", strings.NewReader("q")); code != 0 {
		t.Fatalf("told: exit %d", code)
	}
	if b, _ := os.ReadFile(kill); len(b) != 0 {
		t.Fatal("a VM the driver ended itself was killed again")
	}
	if code := runWatchdog("abhed-microvm", "vm1", strings.NewReader("")); code != 0 {
		t.Fatalf("driver gone: exit %d", code)
	}
	if b, err := os.ReadFile(kill); err != nil || string(b) != "1" {
		t.Fatalf("the cgroup was not killed: %q %v", b, err)
	}
	if code := runWatchdog("../etc", "vm1", strings.NewReader("")); code != 2 {
		t.Fatalf("a parent with a path in it: exit %d", code)
	}
	if code := runWatchdog("abhed-microvm", "../../x", strings.NewReader("")); code != 2 {
		t.Fatalf("a vm id with a path in it: exit %d", code)
	}
}

// Where the kernel has no cgroup.kill, the watchdog makes no file in its
// place and kills each process the cgroup holds instead.
func TestWatchdogWithoutCgroupKill(t *testing.T) {
	dir := fakeCgroup(t, false)
	vmm := exec.Command("sleep", "60")
	if err := vmm.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vmm.Process.Kill() })
	exited := make(chan error, 1)
	go func() { exited <- vmm.Wait() }()
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(vmm.Process.Pid)+"\n"), 0o644); err != nil { // #nosec G306 -- a test's stand-in
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- runWatchdog("abhed-microvm", "vm1", strings.NewReader("")) }()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the cgroup's process was not killed")
	}
	_ = os.WriteFile(filepath.Join(dir, "cgroup.procs"), nil, 0o644) // #nosec G306 -- as the kernel would after the kill
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "cgroup.kill")); err == nil {
		t.Fatal("the watchdog made a cgroup.kill file of its own")
	}
	// A cgroup already gone is nothing to kill.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if code := runWatchdog("abhed-microvm", "vm1", strings.NewReader("")); code != 0 {
		t.Fatalf("a cgroup already gone: exit %d", code)
	}
}

// In a real delegated cgroup the watchdog kills what the VM's cgroup holds;
// set ABHED_TEST_DELEGATED_CGROUP=1 and run under a delegated scope.
func TestWatchdogKillsARealCgroup(t *testing.T) {
	if os.Getenv("ABHED_TEST_DELEGATED_CGROUP") != "1" {
		t.Skip("set ABHED_TEST_DELEGATED_CGROUP=1 under `systemd-run --user --scope -p Delegate=yes` to kill a real cgroup")
	}
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	own, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "0::")
	if !ok {
		t.Fatalf("not cgroup v2: %q", b)
	}
	old := cgroupRoot
	t.Cleanup(func() { cgroupRoot = old })
	cgroupRoot = filepath.Join("/sys/fs/cgroup", own)
	dir := filepath.Join(cgroupRoot, "abhed-microvm", "vm1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeCgroup("abhed-microvm", "vm1"); _ = os.Remove(filepath.Dir(dir)) })
	vmm := exec.Command("sleep", "60")
	if err := vmm.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vmm.Process.Kill() })
	exited := make(chan error, 1)
	go func() { exited <- vmm.Wait() }()
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(vmm.Process.Pid)), 0); err != nil {
		t.Fatalf("moving the stand-in VM into its cgroup: %v", err)
	}
	if code := runWatchdog("abhed-microvm", "vm1", strings.NewReader("")); code != 0 {
		t.Fatalf("exit %d", code)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the VM's cgroup was not killed")
	}
	if _, err := os.Stat(filepath.Join(dir, "cgroup.kill")); err != nil {
		t.Fatalf("this kernel has no cgroup.kill, so the fallback was what ran: %v", err)
	}
}
