//go:build !unix

package server

import "os"

// processAlive reports whether a pid the test's own shell started still runs.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}

// killProcess ends a pid the test's own shell started, when it outlived it.
func killProcess(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
