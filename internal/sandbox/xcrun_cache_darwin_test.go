package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Apple's /usr/bin shims run through xcrun, which keeps a cache in the
// account's temp folder. Under PrivateTemp that folder is out of reach, so the
// cache goes in the sandbox's own temp, with no error printed for it.
func TestPrivateTempGivesXcrunItsOwnCache(t *testing.T) {
	if err := exec.Command("/usr/bin/git", "--version").Run(); err != nil {
		t.Skipf("no developer tools behind /usr/bin/git: %v", err)
	}
	ws := workspace(t)
	p := DefaultPolicy(ws)
	p.PrivateTemp = true
	s := NewProcess(p)
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	t.Cleanup(func() { _ = s.removeTemp() })
	// xcrun_nocache makes xcrun write its cache rather than find one.
	out, err := runIn(t, s, ws, "xcrun_nocache=1 /usr/bin/git --version; /usr/bin/git --version; xcrun_nocache=1 /usr/bin/python3 --version")
	if err != nil || !strings.Contains(out, "git version") || !strings.Contains(out, "Python 3") {
		t.Fatalf("git under the sandbox: %v\n%s", err, out)
	}
	if strings.Contains(out, "couldn't create cache file") || strings.Contains(out, "xcrun_db") {
		t.Fatalf("xcrun reached for the account's temp folder:\n%s", out)
	}
	own, _ := s.privateTemp()
	if _, err := os.Stat(filepath.Join(own, "xcrun_db")); err != nil {
		t.Fatalf("no cache in the sandbox's own temp: %v", err)
	}
}
