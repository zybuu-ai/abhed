//go:build darwin && cgo

package app

import "testing"

// A build with cgo writes the unified log through os_log, under abhed's own
// subsystem, so the system records abhed as the sender.
func TestCgoBuildWritesThroughOSLog(t *testing.T) {
	if sysLogBackend != "os_log" || sysLogSubsystem != "com.zybuu.abhed" || sysLogCategory != "admin" {
		t.Fatalf("backend %q subsystem %q category %q", sysLogBackend, sysLogSubsystem, sysLogCategory)
	}
}
