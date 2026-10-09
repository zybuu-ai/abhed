//go:build !windows

package microvm

import (
	"os"
	"path/filepath"
	"testing"
)

// A helper in a folder others may write could be swapped after its
// signature is checked, so it is refused; a sticky shared folder above an
// owner-only one is fine.
func TestHelperFolderOthersCanWriteIsRefused(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "bin")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, HelperName)
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := helperFolderSafe(helper); err != nil {
		t.Fatalf("an owner-only folder: %v", err)
	}
	for _, mode := range []os.FileMode{0o777, 0o770, 0o1777} {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		if err := helperFolderSafe(helper); err == nil {
			t.Errorf("the helper's folder at mode %v was accepted", mode)
		}
	}
	_ = os.Chmod(dir, 0o700)
	if err := os.Chmod(base, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := helperFolderSafe(helper); err == nil {
		t.Error("a folder above that others may write was accepted")
	}
	_ = os.Chmod(base, 0o700)
}
