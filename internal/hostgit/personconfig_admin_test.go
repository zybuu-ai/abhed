//go:build unix

package hostgit

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A folder macOS's admin group may write (Homebrew's, drwxrwxr-x
// person:admin) is the person's, on macOS alone; one everyone may write,
// or another group, is not.
func TestWritableByOthersAdminGroup(t *testing.T) {
	for _, c := range []struct {
		perm os.FileMode
		gid  uint32
		goos string
		want bool
	}{
		{0o775, adminGID, "darwin", false},
		{0o775, 20, "darwin", true},
		{0o775, adminGID, "linux", true},
		{0o777, adminGID, "darwin", true},
		{0o755, 20, "linux", false},
	} {
		if got := writableByOthers(c.perm, c.gid, c.goos); got != c.want {
			t.Errorf("%o gid %d on %s: %v, want %v", c.perm, c.gid, c.goos, got, c.want)
		}
	}
}

// A credential helper in a 775 person:admin folder, as Homebrew's bin is,
// is brokered on macOS; made world-writable, it is not.
func TestHomebrewShapedHelperFolder(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS's admin group")
	}
	r := newRemoteRig(t)
	bin := filepath.Join(r.home, "brew", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("chgrp", "admin", bin).CombinedOutput(); err != nil {
		t.Skipf("this account cannot give a folder to the admin group: %v %s", err, out)
	}
	if err := os.Chmod(bin, 0o775); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(bin, "git-credential-brewed")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, r.home, "config", "--global", "credential.helper", helper)
	ask := func() error {
		_, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch")
		return err
	}
	if err := ask(); err != nil {
		t.Fatalf("a helper in a person:admin 775 folder: %v", err)
	}
	if err := os.Chmod(bin, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bin, 0o700) })
	if err := ask(); err == nil || !strings.Contains(err.Error(), "other users can write") {
		t.Fatalf("a world-writable folder: %v", err)
	}
}
