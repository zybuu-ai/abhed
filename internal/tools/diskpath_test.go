package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// foldsCase reports whether dir's disk opens a name in another case. The
// default APFS on macOS and NTFS do; most Linux disks do not.
func foldsCase(t *testing.T, dir string) bool {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "probe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(filepath.Join(dir, "probe"))
	_, err := os.Stat(filepath.Join(dir, "PROBE"))
	return err == nil
}

// On a disk that folds case, each existing component is spelled as the disk
// holds it, and what does not exist yet keeps its spelling.
func TestDiskPathSpellsWhatTheDiskHolds(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !foldsCase(t, root) {
		t.Skip("this disk keeps case, so no other spelling opens the folder")
	}
	if err := os.MkdirAll(filepath.Join(root, "core", "vault", "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	for given, want := range map[string]string{
		"CORE/VAULT":             "core/vault",
		"core/Vault/KEYS/New/x":  "core/vault/keys/New/x",
		"Core/vault/keys":        "core/vault/keys",
		"core/vault/Missing/Dir": "core/vault/Missing/Dir",
	} {
		if got := DiskPath(filepath.Join(root, given)); got != filepath.Join(root, want) {
			t.Errorf("%s: got %s, want %s", given, got, filepath.Join(root, want))
		}
	}
}

// A folder renamed to another case is read afresh, not from what was remembered of its parent.
func TestDiskPathFollowsARenameToAnotherCase(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !foldsCase(t, root) {
		t.Skip("this disk keeps case, so no other spelling opens the folder")
	}
	if err := os.MkdirAll(filepath.Join(root, "core", "vault"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := DiskPath(filepath.Join(root, "core", "VAULT")); got != filepath.Join(root, "core", "vault") {
		t.Fatalf("got %s", got)
	}
	if err := os.Rename(filepath.Join(root, "core", "vault"), filepath.Join(root, "core", "Vault")); err != nil {
		t.Fatal(err)
	}
	if got := DiskPath(filepath.Join(root, "core", "VAULT")); got != filepath.Join(root, "core", "Vault") {
		t.Fatalf("after the rename: got %s", got)
	}
}

// A case-only rename is seen even when the folder's time is set back, since the change time moves.
func TestDiskPathSeesARenameBehindAnOldTime(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !foldsCase(t, root) || runtime.GOOS == "windows" {
		t.Skip("needs a disk that folds case and a change time the platform reports")
	}
	core := filepath.Join(root, "core")
	if err := os.MkdirAll(filepath.Join(core, "vault"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(core, old, old); err != nil {
		t.Fatal(err)
	}
	if got := DiskPath(filepath.Join(core, "VAULT")); got != filepath.Join(core, "vault") {
		t.Fatalf("got %s", got)
	}
	if err := os.Rename(filepath.Join(core, "vault"), filepath.Join(core, "Vault")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(core, old, old); err != nil {
		t.Fatal(err)
	}
	if got := DiskPath(filepath.Join(core, "VAULT")); got != filepath.Join(core, "Vault") {
		t.Fatalf("after the rename and touch: got %s", got)
	}
}

// A name given in another Unicode form reads as the disk's own name, not a hard link's.
func TestDiskPathPrefersTheNameOverAHardLink(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	nfc, nfd := "caf\u00e9.txt", "cafe\u0301.txt"
	if err := os.WriteFile(filepath.Join(root, nfc), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, nfc), filepath.Join(root, "aaa.txt")); err != nil {
		t.Skip("hard links unavailable:", err)
	}
	if _, err := os.Lstat(filepath.Join(root, nfd)); err != nil {
		t.Skip("this disk keeps Unicode forms apart")
	}
	if got := DiskPath(filepath.Join(root, nfd)); got != filepath.Join(root, nfc) {
		t.Fatalf("got %q, want the disk's own name %q", got, filepath.Join(root, nfc))
	}
}

// Names a disk folds beyond lower case, ß as SS and ς as σ, read as the disk's own name, not a hard link's.
func TestDiskPathFoldsAsTheDiskDoes(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for disk, given := range map[string]string{"stra\u00dfe.txt": "STRASSE.txt", "\u03c3.txt": "\u03c2.txt"} {
		if err := os.WriteFile(filepath.Join(root, disk), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(root, disk), filepath.Join(root, "a-"+disk)); err != nil {
			t.Skip("hard links unavailable:", err)
		}
		if _, err := os.Lstat(filepath.Join(root, given)); err != nil {
			t.Logf("this disk does not open %s as %s", given, disk)
			continue
		}
		if got := DiskPath(filepath.Join(root, given)); got != filepath.Join(root, disk) {
			t.Errorf("%s: got %q, want the disk's own name %q", given, got, filepath.Join(root, disk))
		}
	}
}

// The identity scan never takes a hard link's name for a file's: with two names, neither is chosen.
func TestIdentityScanPassesOverAHardLinkedFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "real.txt"), filepath.Join(root, "aaa.txt")); err != nil {
		t.Skip("hard links unavailable:", err)
	}
	info, err := os.Lstat(filepath.Join(root, "real.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("the link count is not read on Windows")
	}
	if n, ok := byIdentity(root, []string{"aaa.txt", "real.txt"}, info); ok {
		t.Fatalf("a hard-linked file was named %s by identity", n)
	}
	if err := os.Remove(filepath.Join(root, "aaa.txt")); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Lstat(filepath.Join(root, "real.txt"))
	if n, ok := byIdentity(root, []string{"real.txt"}, info); !ok || n != "real.txt" {
		t.Fatalf("a file with one name was not found by identity: %q %v", n, ok)
	}
}

// What does not exist keeps its spelling on any disk, and so does what exists as named.
func TestDiskPathKeepsAPathThatNeedsNoChange(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "a", "B"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a/B", "a/B/New/File.txt", "Nothing/Here"} {
		if got := DiskPath(filepath.Join(root, p)); got != filepath.Join(root, p) {
			t.Errorf("%s became %s", p, got)
		}
	}
	if got := DiskPath("relative/Path"); got != filepath.Clean("relative/Path") {
		t.Errorf("a relative path became %s", got)
	}
}
