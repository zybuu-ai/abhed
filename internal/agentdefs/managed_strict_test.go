package agentdefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// standIn makes the owner check pass for everything but what refuse names,
// since the tests do not run as root: a file of mode 0644 is someone else's,
// as is a file or directory named in refuse.
func standIn(t *testing.T, refuse ...string) {
	t.Helper()
	old := managedOwnerOK
	managedOwnerOK = func(fi os.FileInfo) bool {
		if fi.Mode().IsRegular() && fi.Mode().Perm() == 0o644 {
			return false
		}
		for _, r := range refuse {
			if fi.Name() == r {
				return false
			}
		}
		return true
	}
	t.Cleanup(func() { managedOwnerOK = old })
}

// The file checked is the file read: a target swapped after the path was
// checked, which the open finds instead, is refused, and the name held.
func TestManagedLinkCheckedOnTheOpenFile(t *testing.T) {
	standIn(t)
	managed, other, op := t.TempDir(), t.TempDir(), t.TempDir()
	real := writeDef(t, other, "real.md", def("sec", ""))
	if err := os.Chmod(real, 0o444); err != nil {
		t.Fatal(err)
	}
	evil := writeDef(t, other, "evil.md", def("sec", "tools: bash\n"))
	if err := os.Chmod(evil, 0o644); err != nil { // 0644 stands for "not root's"
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(managed, "sec.md")); err != nil {
		t.Fatal(err)
	}
	writeDef(t, op, "sec.md", def("sec", ""))
	defs, _ := Load(Options{ManagedDir: managed, Dirs: []string{op}})
	if len(defs) != 1 || defs[0].Source != "managed" {
		t.Fatalf("the organisation's link did not load: %+v", defs)
	}

	old := openManaged
	openManaged = func(string) (*os.File, error) { return os.Open(evil) } // the swap, after the checks on the path
	defer func() { openManaged = old }()
	defs, errs := Load(Options{ManagedDir: managed, Dirs: []string{op}})
	if len(defs) != 0 || !strings.Contains(errText(errs), "owned by root") {
		t.Fatalf("a file swapped in after the check loaded, or gave the name up: %+v %s", defs, errText(errs))
	}
}

// Every directory and link on the way to the target must be the
// organisation's: one someone else may change is refused, name held.
func TestManagedLinkPathStrict(t *testing.T) {
	managed, base, op := t.TempDir(), t.TempDir(), t.TempDir()
	other := filepath.Join(base, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	real := writeDef(t, other, "real.md", def("chain", ""))
	if err := os.Chmod(real, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(other, "hop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "hop"), filepath.Join(managed, "chain.md")); err != nil {
		t.Fatal(err)
	}
	writeDef(t, op, "chain.md", def("chain", ""))

	standIn(t)
	if defs, errs := Load(Options{ManagedDir: managed, Dirs: []string{op}}); len(defs) != 1 || defs[0].Source != "managed" {
		t.Fatalf("a chain of root's did not load: %+v %s", defs, errText(errs))
	}
	for _, step := range []string{"other", "hop"} {
		standIn(t, step)
		defs, errs := Load(Options{ManagedDir: managed, Dirs: []string{op}})
		if len(defs) != 0 || !strings.Contains(errText(errs), "nobody else may change") {
			t.Fatalf("a %s others may change was followed: %+v %s", step, defs, errText(errs))
		}
	}
}

// A managed directory that is a link leading nowhere fails closed.
func TestDanglingManagedDirFailsClosed(t *testing.T) {
	base, op := t.TempDir(), t.TempDir()
	managed := filepath.Join(base, "agents")
	if err := os.Symlink(filepath.Join(base, "gone"), managed); err != nil {
		t.Fatal(err)
	}
	writeDef(t, op, "sec.md", def("sec", ""))
	defs, errs := Load(Options{ManagedDir: managed, Dirs: []string{op}})
	if len(defs) != 0 || !strings.Contains(errText(errs), "leads nowhere") {
		t.Fatalf("a dangling managed directory freed its names: %+v %s", defs, errText(errs))
	}
}

// fakeInfo is a FileInfo whose owner and mode the test chooses.
type fakeInfo struct {
	mode os.FileMode
	sys  any
}

func (f fakeInfo) Name() string       { return "f" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() os.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return f.sys }

// A managed .MD file is not read, and doctor is told so.
func TestManagedCaseWarnings(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "sec.MD", def("sec", ""))
	writeDef(t, dir, "ok.md", def("ok", ""))
	w := ManagedCaseWarnings(dir)
	if len(w) != 1 || !strings.Contains(w[0], "sec.MD is not read") {
		t.Fatalf("warnings: %v", w)
	}
}
