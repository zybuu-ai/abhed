//go:build unix

package enginecli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// A CLI that hangs on a configuration query is ended with every process it
// started: its helper does not outlive the query.
func TestAskEndsTheCLIsHelpers(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "child")
	stub := filepath.Join(dir, "podman")
	script := "#!/bin/sh\n/bin/sh -c 'echo $$ > " + marker + "; exec /bin/sleep 60' &\nwait\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil { // #nosec G306 -- a stub engine that must run
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := ask(stub, "system", "connection", "list"); err == nil {
		t.Fatal("a hung query answered")
	}
	if took := time.Since(start); took > askWait+2*time.Second {
		t.Fatalf("the query took %s", took)
	}
	b, err := os.ReadFile(marker)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		t.Fatalf("the helper never ran: %v", err)
	}
	pid, _ := strconv.Atoi(string(bytes.TrimSpace(b)))
	for range 50 {
		if pid <= 0 || syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatal("the CLI's helper outlived the query")
}
