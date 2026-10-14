//go:build unix

package sandbox

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// An engine that never answers is reported unavailable within the bound,
// with why, and the helper it started is ended with it.
func TestHungEngineIsUnavailableInTime(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "child")
	// The helper names itself before it becomes the long sleep, so a helper
	// that is running has always written the marker.
	script := "#!/bin/sh\n/bin/sh -c 'echo $$ > " + marker + "; exec /bin/sleep 60' &\nwait\n"
	for _, name := range []string{"docker", "podman", "nerdctl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil { // #nosec G306 -- a stub engine that must run
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	old := engineProbeWait
	engineProbeWait = 300 * time.Millisecond
	t.Cleanup(func() { engineProbeWait = old })
	start := time.Now()
	ok, why := NewContainer(Policy{Workspace: dir}).Available()
	if ok || !strings.Contains(why, "did not answer") {
		t.Fatalf("available %v: %s", ok, why)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the probe took %s", took)
	}
	// Under load the probe may end the stub before its helper starts or
	// writes; a helper still running writes within the wait below.
	var b []byte
	var err error
	for range 50 {
		if b, err = os.ReadFile(marker); err == nil && len(bytes.TrimSpace(b)) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		t.Log("the probe ended the stub before its helper ran")
		return
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for range 50 {
		if pid <= 0 || syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatal("the stub engine's helper outlived the probe")
}
