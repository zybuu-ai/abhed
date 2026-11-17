package container

import (
	"os"
	"path/filepath"
	"testing"
)

// No container mount may be, hold or sit inside an account's or a guest's
// folder, or the accounts store: they are all under ~/.abhed.
func TestMountsRefuseAccountsAndGuests(t *testing.T) {
	h, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", h)
	for _, rel := range []string{".abhed/accounts/0123456789abcdef01234567", ".abhed/guest/g1", ".abhed/accounts"} {
		p := filepath.Join(h, filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := admitHost(p, nil); err == nil {
			t.Errorf("%s was admitted as a mount", rel)
		}
	}
}
