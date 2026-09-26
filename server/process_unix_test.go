//go:build unix

package server

import "syscall"

// processAlive reports whether a pid the test's own shell started still runs.
func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// killProcess ends a pid the test's own shell started, when it outlived it.
func killProcess(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }
