package agentdefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
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

// A link loop is refused at the hop limit, and the name held.
func TestManagedLinkLoopRefused(t *testing.T) {
	standIn(t)
	managed, op := t.TempDir(), t.TempDir()
	a, b := filepath.Join(managed, "loop.md"), filepath.Join(managed, "other.lnk")
	if err := os.Symlink("other.lnk", a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop.md", b); err != nil {
		t.Fatal(err)
	}
	writeDef(t, op, "loop.md", def("loop", ""))
	done := make(chan struct{})
	var defs []*agent.Definition
	var msg string
	go func() {
		defer close(done)
		d, errs := Load(Options{ManagedDir: managed, Dirs: []string{op}})
		defs, msg = d, errText(errs)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a link loop was followed without end")
	}
	if len(defs) != 0 || !strings.Contains(msg, "too many links") {
		t.Fatalf("a loop: %+v %s", defs, msg)
	}
}

// A relative target starts from the link's own directory, as stow installs
// them; and ".." after a link steps back from where that link led, as the
// kernel does, not from the text before it.
func TestManagedRelativeLinks(t *testing.T) {
	standIn(t)
	base, op := t.TempDir(), t.TempDir()
	managed := filepath.Join(base, "managed")
	other := filepath.Join(base, "other")
	deep := filepath.Join(other, "sub", "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	for p, name := range map[string]string{
		filepath.Join(other, "real.md"):     "rel",
		filepath.Join(other, "sub", "k.md"): "kernel",
		filepath.Join(other, "k.md"):        "lexical",
	} {
		writeDef(t, filepath.Dir(p), filepath.Base(p), def(name, ""))
		if err := os.Chmod(p, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("sub", "deep"), filepath.Join(other, "lnk")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "other", "real.md"), filepath.Join(managed, "rel.md")); err != nil {
		t.Fatal(err)
	}
	// other/lnk/.. is other/sub to the kernel; lexically it would be other.
	// Written out, not joined: filepath.Join would clean the ".." away first.
	if err := os.Symlink("../other/lnk/../k.md", filepath.Join(managed, "k.md")); err != nil {
		t.Fatal(err)
	}
	defs, errs := Load(Options{ManagedDir: managed, Dirs: []string{op}})
	got := map[string]bool{}
	for _, d := range defs {
		got[d.Name] = d.Source == "managed"
	}
	if !got["rel"] || !got["kernel"] || got["lexical"] {
		t.Fatalf("resolved %v, %s", got, errText(errs))
	}
}

// A managed directory under an ancestor link that leads nowhere fails closed.
func TestDanglingManagedAncestorFailsClosed(t *testing.T) {
	base, op := t.TempDir(), t.TempDir()
	parent := filepath.Join(base, "abhed")
	if err := os.Symlink(filepath.Join(base, "static", "abhed"), parent); err != nil {
		t.Fatal(err)
	}
	writeDef(t, op, "sec.md", def("sec", ""))
	defs, errs := Load(Options{ManagedDir: filepath.Join(parent, "agents"), Dirs: []string{op}})
	if len(defs) != 0 || !strings.Contains(errText(errs), "leads nowhere") {
		t.Fatalf("a dangling ancestor freed the names: %+v %s", defs, errText(errs))
	}
	// Truly absent is not a failure.
	if defs, _ := Load(Options{ManagedDir: filepath.Join(base, "none", "agents"), Dirs: []string{op}}); len(defs) != 1 {
		t.Fatalf("an absent managed directory stopped the rest: %+v", defs)
	}
}
