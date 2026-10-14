package main

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// A bind whose read-only remount fails is taken down, not left writable.
func TestBindReadOnlyUndoesAFailedRemount(t *testing.T) {
	var bound, unbound []string
	sysMount = func(src, dst, _ string, flags uintptr, _ string) error {
		if flags&unix.MS_REMOUNT != 0 {
			return unix.EPERM
		}
		bound = append(bound, dst)
		return nil
	}
	sysUnmount = func(dst string, _ int) error {
		unbound = append(unbound, dst)
		return nil
	}
	t.Cleanup(func() { sysMount, sysUnmount = unix.Mount, unix.Unmount })
	if err := bindReadOnly("/run/x", "/etc/resolv.conf"); !errors.Is(err, unix.EPERM) {
		t.Fatalf("err = %v", err)
	}
	if len(bound) != 1 || len(unbound) != 1 || unbound[0] != "/etc/resolv.conf" {
		t.Fatalf("bound %v, undone %v", bound, unbound)
	}
}
