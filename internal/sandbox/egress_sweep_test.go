package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// At start, the egress relay containers a killed Abhed left are removed by
// their owner label; a live owner's, an unlabelled one and another name are
// never touched.
func TestSweepRemovesEgressContainersAKilledAbhedLeft(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	dead := hostID() + ":424242:1"
	live := ownerOf(os.Getpid())
	if live == "" {
		t.Skip("this platform cannot read a process's start")
	}
	script := `#!/bin/sh
echo "$*" >> ` + log + `
case "$1" in
ps)
  case "$*" in
  *label=` + egressOwnerLabel + `*) echo abhed-egr-dead; echo abhed-egr-live; echo someone-elses ;;
  esac ;;
inspect)
  for n in "$@"; do last=$n; done
  case "$last" in
  abhed-egr-dead) echo "` + dead + `" ;;
  abhed-egr-live) echo "` + live + `" ;;
  someone-elses) echo "` + dead + `" ;;
  esac ;;
esac
exit 0
`
	engine := filepath.Join(dir, "engine")
	if err := os.WriteFile(engine, []byte(script), 0o755); err != nil { // #nosec G306 -- a test script that must run
		t.Fatal(err)
	}
	old := ownerExists
	t.Cleanup(func() { ownerExists = old })
	ownerExists = func(pid int) existence {
		if pid == 424242 {
			return existsNo
		}
		return old(pid)
	}
	c := &Container{runtime: engine, available: true}
	sw := c.sweepServers(t.Context())
	if len(sw.Removed) != 1 || sw.Removed[0] != "abhed-egr-dead" {
		t.Fatalf("removed %v", sw.Removed)
	}
	b, _ := os.ReadFile(log)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "rm ") && !strings.HasSuffix(line, " abhed-egr-dead") {
			t.Errorf("removed more than the dead owner's: %q", line)
		}
	}
}
