//go:build darwin && cgo

package app

import (
	"sync"
	"testing"
)

// A build with cgo writes the unified log through os_log, under abhed's own
// subsystem, so the system records abhed as the sender.
func TestCgoBuildWritesThroughOSLog(t *testing.T) {
	if sysLogBackend != "os_log" || sysLogSubsystem != "com.zybuu.abhed" || sysLogCategory != "admin" {
		t.Fatalf("backend %q subsystem %q category %q", sysLogBackend, sysLogSubsystem, sysLogCategory)
	}
}

// Goroutines logging at once share one os_log handle, made once.
func TestOSLogHandleMadeOnce(t *testing.T) {
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = writeSysLog("abhed test: concurrent os_log write")
		}()
	}
	wg.Wait()
	if n := sysLogHandles(); n != 1 {
		t.Fatalf("%d os_log handles made", n)
	}
}
