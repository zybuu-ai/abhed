//go:build !windows

package microvm

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/driver/microvm/guest"
)

// sandboxPair is a workspace and a folder beside it that nothing may reach.
func sandboxPair(t *testing.T) (ws, outside string) {
	t.Helper()
	base := shortTemp(t)
	ws, outside = filepath.Join(base, "ws"), filepath.Join(base, "outside")
	for _, d := range []string{ws, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	return ws, outside
}

func applierFor(t *testing.T, ws string, protected ...string) *applier {
	t.Helper()
	a, err := openApplier(ws, nil, protected, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.close)
	return a
}

func fileChange(name, body string, mode int64) guest.Change {
	return guest.Change{Path: name, Header: &tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(body)), Mode: mode}, Body: strings.NewReader(body)}
}

func linkChange(name, target string) guest.Change {
	return guest.Change{Path: name, Header: &tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target}}
}

func dirChange(name string) guest.Change {
	return guest.Change{Path: name, Header: &tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755}}
}

func mustApply(t *testing.T, a *applier, ch guest.Change) {
	t.Helper()
	if err := a.apply(ch); err != nil {
		t.Fatalf("%s: %v", ch.Path, err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading %s: %v", p, err)
	}
	return string(b)
}

func refusedOnly(t *testing.T, a *applier, n int, want string) {
	t.Helper()
	if len(a.refused) != n {
		t.Fatalf("refused %d, want %d: %q", len(a.refused), n, a.refused)
	}
	for _, r := range a.refused {
		if !strings.Contains(r, want) {
			t.Fatalf("refusal %q does not say %q", r, want)
		}
	}
}

func TestApplyWritesFoldersFilesAndLinks(t *testing.T) {
	ws, _ := sandboxPair(t)
	a := applierFor(t, ws)
	mustApply(t, a, dirChange("src"))
	mustApply(t, a, fileChange("src/main.go", "package main\n", 0o4755))
	mustApply(t, a, fileChange("deep/new/file.txt", "x", 0o644))
	mustApply(t, a, linkChange("src/link", "main.go"))
	mustApply(t, a, fileChange(".git/objects/ab/cdef", "obj", 0o444))
	if len(a.refused) != 0 {
		t.Fatalf("refused %q", a.refused)
	}
	if read(t, filepath.Join(ws, "src/main.go")) != "package main\n" || read(t, filepath.Join(ws, "deep/new/file.txt")) != "x" {
		t.Fatal("content not written")
	}
	fi, _ := os.Stat(filepath.Join(ws, "src/main.go"))
	if fi.Mode()&os.ModeSetuid != 0 || fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v: the setuid bit came back", fi.Mode())
	}
	if target, _ := os.Readlink(filepath.Join(ws, "src/link")); target != "main.go" {
		t.Fatalf("link %q", target)
	}
	if read(t, filepath.Join(ws, ".git/objects/ab/cdef")) != "obj" {
		t.Fatal("git objects are not protected and should come back")
	}
	// No temporary file is left behind.
	entries, _ := os.ReadDir(filepath.Join(ws, "src"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".abhed-sync-") {
			t.Fatalf("left %s", e.Name())
		}
	}
}

func TestApplyRefusesPathsLeavingTheWorkspace(t *testing.T) {
	ws, outside := sandboxPair(t)
	a := applierFor(t, ws)
	for _, name := range []string{"../outside/secret", "/etc/abhed-test", "a/../../outside/secret", "a/./b", "a//b", "", ".", "a\x00b"} {
		mustApply(t, a, fileChange(name, "pwned", 0o644))
		mustApply(t, a, guest.Change{Deleted: true, Path: name})
	}
	if read(t, filepath.Join(outside, "secret")) != "untouched" {
		t.Fatal("wrote outside the workspace")
	}
	if len(a.refused) != 16 {
		t.Fatalf("refused %d of 16: %q", len(a.refused), a.refused)
	}
	if _, err := os.Stat("/etc/abhed-test"); err == nil {
		t.Fatal("wrote an absolute path")
	}
}

func TestApplyNeverWritesAbhedState(t *testing.T) {
	ws, _ := sandboxPair(t)
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := applierFor(t, ws)
	mustApply(t, a, fileChange(".abhed/config.json", `{"permissions":{"mode":"bypass"}}`, 0o644))
	mustApply(t, a, fileChange(".ABHED/config.json", "x", 0o644))
	mustApply(t, a, fileChange("sub/.abhed/users.json", "x", 0o644))
	mustApply(t, a, dirChange(".abhed"))
	mustApply(t, a, guest.Change{Deleted: true, Path: ".abhed"})
	mustApply(t, a, linkChange("cfg", ".abhed/config.json"))
	refusedOnly(t, a, 6, "")
	if read(t, filepath.Join(ws, ".abhed", "config.json")) != "{}" {
		t.Fatal(".abhed was written")
	}
	if _, err := os.Lstat(filepath.Join(ws, "cfg")); err == nil {
		t.Fatal("a link into .abhed was made")
	}
	if _, err := os.Stat(filepath.Join(ws, "sub")); err == nil {
		t.Fatal("a folder was made on the way to a refused path")
	}
}

func TestApplyKeepsGitConfigurationAndHooks(t *testing.T) {
	ws, _ := sandboxPair(t)
	if err := os.MkdirAll(filepath.Join(ws, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := applierFor(t, ws)
	for _, name := range []string{".git/hooks/pre-commit", ".git/config", ".GIT/Config", ".git/config.worktree",
		"sub/.git/hooks/post-checkout", ".git/modules/m/config", ".git/worktrees/w/commondir", ".git/worktrees/w/gitdir",
		".git/info/attributes", ".git/objects/info/alternates", "sub/.git"} {
		mustApply(t, a, fileChange(name, "evil", 0o755))
	}
	mustApply(t, a, linkChange("vendor/.git", "../.git"))
	mustApply(t, a, guest.Change{Deleted: true, Path: ".git/config"})
	refusedOnly(t, a, 13, "protected")
	if read(t, filepath.Join(ws, ".git", "config")) != "[core]\n" {
		t.Fatal(".git/config was changed")
	}
	if _, err := os.Stat(filepath.Join(ws, ".git", "hooks", "pre-commit")); err == nil {
		t.Fatal("a hook was written")
	}
}

func TestApplyKeepsWriteProtectedPaths(t *testing.T) {
	ws, _ := sandboxPair(t)
	set := filepath.Join(ws, ".vscode", "settings.json")
	if err := os.MkdirAll(filepath.Dir(set), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(set, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := applierFor(t, ws, set, "docs/locked")
	mustApply(t, a, fileChange(".vscode/settings.json", "evil", 0o644))
	mustApply(t, a, fileChange(".VSCODE/Settings.json", "evil", 0o644))
	mustApply(t, a, fileChange("docs/locked/a.md", "evil", 0o644))
	refusedOnly(t, a, 3, "write-protected")
	// Deleting the folder that holds it removes the rest and keeps it.
	if err := os.WriteFile(filepath.Join(ws, ".vscode", "other.json"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	a = applierFor(t, ws, set)
	mustApply(t, a, guest.Change{Deleted: true, Path: ".vscode"})
	if read(t, set) != "{}" {
		t.Fatal("a write-protected file was deleted")
	}
	if _, err := os.Stat(filepath.Join(ws, ".vscode", "other.json")); err == nil {
		t.Fatal("the unprotected file beside it was kept")
	}
	// Nor can the folder be replaced by a file.
	mustApply(t, a, fileChange(".vscode", "file now", 0o644))
	if read(t, set) != "{}" {
		t.Fatal("a folder holding a write-protected file was replaced")
	}
}

func TestApplyNeverFollowsHostLinks(t *testing.T) {
	ws, outside := sandboxPair(t)
	if err := os.Symlink(outside, filepath.Join(ws, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(ws, "filelink")); err != nil {
		t.Fatal(err)
	}
	a := applierFor(t, ws)
	// Through a link to a folder: refused.
	mustApply(t, a, fileChange("dirlink/secret", "pwned", 0o644))
	mustApply(t, a, guest.Change{Deleted: true, Path: "dirlink/secret"})
	mustApply(t, a, dirChange("dirlink/sub"))
	// Onto a link to a file: the link is replaced, its target is not written.
	mustApply(t, a, fileChange("filelink", "replaced", 0o644))
	if read(t, filepath.Join(outside, "secret")) != "untouched" {
		t.Fatal("wrote through a host link")
	}
	if fi, err := os.Lstat(filepath.Join(ws, "filelink")); err != nil || !fi.Mode().IsRegular() || read(t, filepath.Join(ws, "filelink")) != "replaced" {
		t.Fatalf("the link was not replaced by the file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "sub")); err == nil {
		t.Fatal("made a folder through a host link")
	}
	refusedOnly(t, a, 3, "link")
	// Deleting the link removes the link, not what it points to.
	mustApply(t, a, guest.Change{Deleted: true, Path: "dirlink"})
	if _, err := os.Lstat(filepath.Join(ws, "dirlink")); err == nil || read(t, filepath.Join(outside, "secret")) != "untouched" {
		t.Fatal("deleting a link reached its target")
	}
}

func TestApplyNeverWritesThroughHardLinks(t *testing.T) {
	ws, outside := sandboxPair(t)
	if err := os.Link(filepath.Join(outside, "secret"), filepath.Join(ws, "hard")); err != nil {
		t.Fatal(err)
	}
	a := applierFor(t, ws)
	mustApply(t, a, fileChange("hard", "pwned", 0o644))
	mustApply(t, a, guest.Change{Path: "h2", Header: &tar.Header{Name: "h2", Typeflag: tar.TypeLink, Linkname: "../outside/secret"}})
	if read(t, filepath.Join(outside, "secret")) != "untouched" {
		t.Fatal("wrote through a hard link")
	}
	if read(t, filepath.Join(ws, "hard")) != "pwned" {
		t.Fatal("the workspace's name was not given the new content")
	}
	refusedOnly(t, a, 1, "hard links")
	if _, err := os.Lstat(filepath.Join(ws, "h2")); err == nil {
		t.Fatal("a hard link entry was made")
	}
}

func TestApplyRefusesLinksOut(t *testing.T) {
	ws, _ := sandboxPair(t)
	a := applierFor(t, ws)
	for _, target := range []string{"/etc/passwd", "../../outside/secret", "a/../../../x", ".git/hooks", "../.abhed"} {
		mustApply(t, a, linkChange("sub/l", target))
	}
	refusedOnly(t, a, 5, "points")
	mustApply(t, a, linkChange("sub/ok", "../README"))
	if len(a.refused) != 5 {
		t.Fatalf("an inside link was refused: %q", a.refused)
	}
}

// A ".." after another link climbs from where that link leads: the
// chained links below would reach outside, or into .abhed.
func TestApplyRefusesChainedLinks(t *testing.T) {
	ws, _ := sandboxPair(t)
	a := applierFor(t, ws)
	mustApply(t, a, linkChange("d1/d2/L", ".."))
	mustApply(t, a, linkChange("d1/d2/d3/L", "../.."))
	if len(a.refused) != 0 {
		t.Fatalf("an inside link was refused: %q", a.refused)
	}
	mustApply(t, a, linkChange("d1/d2/E", "L/../../../outside/secret"))
	mustApply(t, a, linkChange("d1/d2/F", "L/../../secret"))
	mustApply(t, a, linkChange("d1/d2/d3/G", "../../../../outside"))
	mustApply(t, a, linkChange("d1/H", "d2/L/../.abhed"))
	refusedOnly(t, a, 4, "points")
	for _, n := range []string{"d1/d2/E", "d1/d2/F", "d1/d2/d3/G", "d1/H"} {
		if _, err := os.Lstat(filepath.Join(ws, n)); err == nil {
			t.Fatalf("%s was made", n)
		}
	}
}

func TestApplyRefusesSpecialFilesAndBounds(t *testing.T) {
	ws, _ := sandboxPair(t)
	a := applierFor(t, ws)
	for _, typ := range []byte{tar.TypeFifo, tar.TypeChar, tar.TypeBlock} {
		mustApply(t, a, guest.Change{Path: "dev", Header: &tar.Header{Name: "dev", Typeflag: typ}})
	}
	refusedOnly(t, a, 3, "only files")
	big := fileChange("big", "", 0o644)
	big.Header.Size = 2 << 20
	if err := a.apply(big); err == nil {
		t.Fatal("more than a sync may carry was accepted")
	}
}

func TestApplyReplacesKinds(t *testing.T) {
	ws, _ := sandboxPair(t)
	if err := os.MkdirAll(filepath.Join(ws, "d", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "f"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := applierFor(t, ws)
	mustApply(t, a, fileChange("d", "was a folder", 0o644))
	mustApply(t, a, dirChange("f"))
	if read(t, filepath.Join(ws, "d")) != "was a folder" {
		t.Fatal("folder not replaced by a file")
	}
	if fi, err := os.Stat(filepath.Join(ws, "f")); err != nil || !fi.IsDir() {
		t.Fatal("file not replaced by a folder")
	}
}

func TestApplyOwner(t *testing.T) {
	ws, _ := sandboxPair(t)
	other := &Credential{UID: uint32(os.Getuid()) + 1, GID: uint32(os.Getgid())}
	if _, err := openApplier(ws, other, nil, 1<<20); err == nil {
		t.Fatal("a workspace another user owns was opened for writing")
	}
	me := &Credential{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	a, err := openApplier(ws, me, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	mustApply(t, a, fileChange("n/new", "x", 0o644))
	if len(a.refused) != 0 {
		t.Fatalf("refused %q", a.refused)
	}
	// A folder not the owner's is not written into: as root, a different
	// owner stands in for it.
	a.owner = other
	mustApply(t, a, fileChange("n/again", "x", 0o644))
	refusedOnly(t, a, 1, "not owned")
}
