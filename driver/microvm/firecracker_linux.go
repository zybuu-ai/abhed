package microvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// cgroupRoot is where cgroup v2 is mounted; tests move it.
var cgroupRoot = "/sys/fs/cgroup"

// probeFirecracker reports the first thing missing for the jailer to run a
// VM here. Each is required; none is worked around.
func probeFirecracker(o *Options, fc Firecracker) error {
	if os.Geteuid() != 0 {
		return errors.New("the firecracker backend must run as root, because the jailer needs root to chroot and drop to each VM's user; run cmd/abhed-microvm as root and register it as an out-of-process driver")
	}
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("KVM is not available: %w; the host needs hardware virtualization and the kvm module", err)
	}
	_ = kvm.Close()
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		return errors.New("the jailer's limits need cgroup v2 mounted at /sys/fs/cgroup")
	}
	for _, p := range []string{fc.Binary, fc.Jailer} {
		if err := trustedBinary(p); err != nil {
			return err
		}
	}
	for _, p := range []string{o.Kernel, o.Rootfs} {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a readable file", p)
		}
	}
	return firecrackerVersion(fc.Binary)
}

// trustedBinary refuses a binary root would run that someone else could
// have replaced.
func trustedBinary(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("%s is not installed: %w", p, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", p)
	}
	if !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must be owned by root and writable by no one else", p)
	}
	return nil
}

var versionRE = regexp.MustCompile(`Firecracker v(\d+)\.(\d+)`)

// firecrackerVersion refuses a Firecracker older than 1.5, the oldest the
// configuration and flags here are written for.
func firecrackerVersion(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return fmt.Errorf("%s --version: %w", bin, err)
	}
	m := versionRE.FindSubmatch(out)
	if m == nil {
		return fmt.Errorf("%s did not report a Firecracker version", bin)
	}
	major, _ := strconv.Atoi(string(m[1]))
	minor, _ := strconv.Atoi(string(m[2]))
	if major < 1 || (major == 1 && minor < 5) {
		return fmt.Errorf("Firecracker %d.%d is older than the 1.5 this driver needs", major, minor)
	}
	return nil
}

func cgroupDir(parent, vmID string) string { return filepath.Join(cgroupRoot, parent, vmID) }

// killCgroup kills every process in the VM's cgroup.
func killCgroup(parent, vmID string) error {
	dir := cgroupDir(parent, vmID)
	// Never created: a cgroup's files are the kernel's, and one that is
	// missing means the cgroup is gone or the kernel predates cgroup.kill.
	f, err := os.OpenFile(filepath.Join(dir, "cgroup.kill"), os.O_WRONLY, 0)
	if err == nil {
		_, err = f.Write([]byte("1"))
		return errors.Join(err, f.Close())
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, serr := os.Stat(dir); errors.Is(serr, os.ErrNotExist) {
		return nil
	}
	return killProcs(dir)
}

// killProcs kills every process in the cgroup dir, for a kernel without
// cgroup.kill (before 5.14), until none is left or a second passes.
func killProcs(dir string) error {
	// Frozen first, so none forks a child the loop has not read; a fatal
	// signal still ends a frozen process.
	if f, err := os.OpenFile(filepath.Join(dir, "cgroup.freeze"), os.O_WRONLY, 0); err == nil {
		_, _ = f.Write([]byte("1"))
		_ = f.Close()
	}
	for range 50 {
		b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs")) // #nosec G304 -- the VM's cgroup
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		pids := strings.Fields(string(b))
		if len(pids) == 0 {
			return nil
		}
		for _, p := range pids {
			if pid, err := strconv.Atoi(p); err == nil && pid > 1 {
				_ = unix.Kill(pid, unix.SIGKILL)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("processes are still in %s", dir)
}

// removeCgroup removes the VM's cgroup, waiting a little for processes
// just killed to leave it.
func removeCgroup(parent, vmID string) error {
	var err error
	for range 50 {
		err = unix.Rmdir(cgroupDir(parent, vmID))
		if err == nil || errors.Is(err, unix.ENOENT) {
			return nil
		}
		if !errors.Is(err, unix.EBUSY) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("removing the cgroup of %s: %w", vmID, err)
}

// vmmProcAttr kills the jailer if the driver dies before it has dropped
// privileges; after that, Delete and cgroup.kill end it.
func vmmProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
}

// watchdogAttr starts the watchdog in a session of its own.
func watchdogAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
