package microvm

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// watchdogArg makes this binary a VM's watchdog: `abhed watchdogArg PARENT
// VMID`. It waits on its stdin, which only the driver holds open. A 'q'
// there means the driver ended the VM itself; an end of input without it
// means the driver was killed, and the watchdog kills the VM's cgroup, so no
// VM outlives the driver that ran it.
const watchdogArg = "__abhed_microvm_watchdog"

func init() {
	if len(os.Args) == 4 && os.Args[1] == watchdogArg {
		os.Exit(runWatchdog(os.Args[2], os.Args[3], os.Stdin))
	}
}

func runWatchdog(parent, vmID string, in io.Reader) int {
	if !cgroupName.MatchString(parent) || strings.ContainsAny(vmID, "/.") || vmID == "" {
		return 2
	}
	b := make([]byte, 1)
	n, _ := in.Read(b)
	if n == 1 && b[0] == 'q' {
		return 0
	}
	if err := killCgroup(parent, vmID); err != nil {
		return 1
	}
	return 0
}

// startWatchdog starts the watchdog of one VM, as a process of its own that
// outlives this one; stop tells it the driver ended the VM itself.
func startWatchdog(parent, vmID string) (stop func(), err error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if real, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = real
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, watchdogArg, parent, vmID) // #nosec G204 -- this binary, with the watchdog's fixed arguments
	cmd.Stdin = r
	// Its own session: a signal to the driver's group does not reach it.
	cmd.SysProcAttr = watchdogAttr()
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, err
	}
	_ = r.Close()
	go func() { _ = cmd.Wait() }()
	done := false
	return func() {
		if done {
			return
		}
		done = true
		_, werr := w.Write([]byte{'q'})
		_ = w.Close()
		if werr != nil && !errors.Is(werr, os.ErrClosed) {
			_ = cmd.Process.Kill()
		}
	}, nil
}
