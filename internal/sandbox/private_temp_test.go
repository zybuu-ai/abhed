package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
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

// Removal makes read-only folders writable, but never one outside its own:
// a folder swapped for a link after the walk listed it is not followed out.
func TestRemovalChmodsOnlyInsideItsFolder(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Chmod(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o700) })
	if err := os.Symlink(outside, filepath.Join(dir, "swapped")); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	chmodFolder(r, "swapped")
	_ = r.Close()
	if fi, err := os.Stat(outside); err != nil || fi.Mode().Perm() != 0o500 {
		t.Fatalf("a folder outside was changed through a link: %v %v", fi.Mode(), err)
	}

	mod := filepath.Join(dir, "mod", "x")
	if err := os.MkdirAll(mod, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "f"), nil, 0o400); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(mod, 0o500)
	_ = os.Chmod(filepath.Dir(mod), 0o500)
	makeRemovable(dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("a read-only tree was not made removable: %v", err)
	}
}
