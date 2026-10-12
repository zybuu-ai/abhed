//go:build darwin && !cgo

package app

import "testing"

// A build without cgo falls back to /usr/bin/logger.
func TestBuildWithoutCgoFallsBackToLogger(t *testing.T) {
	if sysLogBackend != "logger" {
		t.Fatalf("backend %q", sysLogBackend)
	}
}
