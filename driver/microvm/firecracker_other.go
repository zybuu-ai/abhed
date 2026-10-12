//go:build !linux

package microvm

import (
	"fmt"
	"runtime"
	"syscall"
)

func probeFirecracker(*Options, Firecracker) error {
	return fmt.Errorf("the firecracker backend needs Linux with KVM; this host runs %s", runtime.GOOS)
}

func killCgroup(string, string) error   { return nil }
func removeCgroup(string, string) error { return nil }

func vmmProcAttr() *syscall.SysProcAttr { return nil }

// startWatchdog has nothing to watch here: the firecracker backend is Linux's.
func startWatchdog(string, string) (func(), error) { return func() {}, nil }
