//go:build unix

package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// checkPrivate resolves dir and checks that no other user can change what it
// holds or swap it for another: the folder itself is exactly 0700 and this
// user's, and every folder above it is root's or this user's and writable by
// no one else, unless it is sticky. It returns the resolved path, which is
// what to use from then on, since a link in dir could be repointed. An ACL
// on a folder (macOS) is not read: TMPDIR's is trusted, as go test trusts it.
func checkPrivate(dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%q is not absolute", dir)
	}
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	me := os.Getuid()
	for d, leaf := p, true; ; d, leaf = filepath.Dir(d), false {
		fi, err := os.Lstat(d) // #nosec G703 -- tests' own temp folder, checked to be private before use
		if err != nil {
			return "", err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || !fi.IsDir() {
			return "", fmt.Errorf("%s is not a folder", d)
		}
		owner := int(st.Uid)
		switch {
		case leaf && (owner != me || fi.Mode().Perm() != 0o700):
			return "", fmt.Errorf("%s is not a 0700 folder of this user's", d)
		case owner != me && owner != 0:
			return "", fmt.Errorf("%s belongs to another user", d)
		case fi.Mode().Perm()&0o022 != 0 && fi.Mode()&os.ModeSticky == 0:
			return "", fmt.Errorf("%s is writable by others", d)
		}
		if d == filepath.Dir(d) {
			return p, nil
		}
	}
}

// ownFolderChecked says the folder Isolate makes is checked too, and a
// failure stops the run.
const ownFolderChecked = true
