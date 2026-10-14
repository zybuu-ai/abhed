package sandbox

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Under PrivateTemp the macOS profile denies the shared temp areas and
// allows the sandbox's own folder, its workspace and what it is given to read.
func TestPrivateTempProfileKeepsTheSharedAreasOut(t *testing.T) {
	ws := workspace(t)
	p := DefaultPolicy(ws)
	p.PrivateTemp = true
	s := &Process{policy: p, backend: "sandbox-exec"}
	t.Cleanup(func() { _ = s.removeTemp() })
	own, err := s.privateTemp()
	if err != nil {
		t.Fatal(err)
	}
	prof := s.seatbeltProfileFor(0, "/opt/ca-folder")
	for _, want := range []string{
		`(deny file-read* (subpath "/private/tmp"))`, `(deny file-write* (subpath "/private/var/tmp"))`,
		fmt.Sprintf("(allow file-write* (subpath %q))", own), fmt.Sprintf("(allow file-read* (subpath %q))", ws),
		`(allow file-read* (subpath "/opt/ca-folder"))`,
	} {
		if !strings.Contains(prof, want) {
			t.Errorf("the profile lacks %s", want)
		}
	}
	if i, j := strings.Index(prof, "Temp: a folder"), strings.Index(prof, "Abhed's own configuration"); i < 0 || j < i {
		t.Error("the temp rules must come before the state denies, which win over them")
	}
	if strings.Contains(NewProcess(DefaultPolicy(ws)).seatbeltProfile(), "Temp: a folder") {
		t.Error("a sandbox without PrivateTemp has its temp rules")
	}
}

// On Linux the sandbox's own folder is its /tmp, kept between its commands,
// and TMPDIR names it.
func TestPrivateTempIsTheBubblewrapTmp(t *testing.T) {
	p := DefaultPolicy(workspace(t))
	p.PrivateTemp = true
	s := &Process{policy: p, backend: "bwrap"}
	t.Cleanup(func() { _ = s.removeTemp() })
	_, env, _, err := s.commandEnv(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	own, _ := s.privateTemp()
	args := s.wrapEgress(t.Context(), p.Workspace, env, nil, "/bin/true").Args
	at := slices.Index(args, own)
	tmpfs := false
	for i := range len(args) - 1 {
		tmpfs = tmpfs || args[i] == "--tmpfs" && args[i+1] == "/tmp"
	}
	if at < 1 || args[at-1] != "--bind" || args[at+1] != "/tmp" || tmpfs {
		t.Fatalf("bwrap args %v", args)
	}
	if !slices.Contains(env, "TMPDIR=/tmp") {
		t.Fatalf("env %v", env)
	}
}
