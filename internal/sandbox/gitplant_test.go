package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What git init and clone write passes; anything that can name a program is dropped.
func TestFilterGitConfig(t *testing.T) {
	clone := "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n" +
		"[remote \"origin\"]\n\turl = https://example.com/r.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n" +
		"[branch \"main\"]\n\tremote = origin\n\tmerge = refs/heads/main\n"
	if kept, bad := filterGitConfig([]byte(clone)); len(bad) != 0 || !strings.Contains(string(kept), "url = https://example.com/r.git") {
		t.Fatalf("a clone's config: bad %v kept %q", bad, kept)
	}
	for _, planted := range []string{
		"[core]\n\tfsmonitor = ./x\n",
		"[core]\n\thooksPath = h\n",
		"[CORE]\n\tFsMonitor = ./x\n",
		"[core] fsmonitor = ./x\n",
		"[filter \"a\"]\n\tclean = ./x\n",
		"[submodule \"s\"]\n\tupdate = !./x\n",
		"[include]\n\tpath = elsewhere\n",
		"[alias]\n\tst = !./x\n",
		"[core.x]\n\tfsmonitor = ./x\n",
	} {
		kept, bad := filterGitConfig([]byte(planted))
		if len(bad) == 0 || strings.Contains(strings.ToLower(string(kept)), "./x") || strings.Contains(string(kept), "elsewhere") {
			t.Errorf("%q: bad %v kept %q", planted, bad, kept)
		}
	}
}

// A repository made after the first look, with an fsmonitor or hook, is
// removed and the next command refused and recorded; earlier ones stay.
func TestPlantedRepositoryIsTakenOut(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := workspace(t)
	writeFiles(t, ws, map[string]string{"old/.git/HEAD": "ref: refs/heads/main\n", "old/.git/config": "[core]\n\tfsmonitor = ./mine\n"})
	var mem gitMemory
	var rec recorded
	if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err != nil {
		t.Fatalf("the first look refused: %v", err)
	}
	writeFiles(t, ws, map[string]string{
		"clone/.git/HEAD": "ref: refs/heads/main\n", "clone/.git/config": "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = x\n",
		"n/.git/HEAD": "ref: refs/heads/main\n", "n/.git/config": "[core]\n\tbare = false\n\tfsmonitor = ./x\n", "n/.git/hooks/post-checkout": "#!/bin/sh\n",
	})
	_, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false)
	if err == nil || !strings.Contains(err.Error(), "core.fsmonitor") {
		t.Fatalf("a planted repository ran: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "n", ".git", "config")); strings.Contains(string(b), "fsmonitor") || !strings.Contains(string(b), "bare = false") {
		t.Errorf("config left as %q", b)
	}
	if _, err := os.Lstat(filepath.Join(ws, "n", ".git", "hooks", "post-checkout")); err == nil {
		t.Error("the hook is still there")
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "old", ".git", "config")); !strings.Contains(string(b), "fsmonitor") {
		t.Error("a repository there from the start was changed")
	}
	if got := rec.of(EvGitPlanted); len(got) != 1 {
		t.Fatalf("recorded %v", rec.events)
	}
	if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err != nil {
		t.Fatalf("the command after was refused: %v", err)
	}
}

// A .git file naming a folder outside the workspace, or under .abhed, is
// refused on every command and nothing where it points is changed.
func TestPlantedPointerOutsideIsNotFollowed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := workspace(t)
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"config": "Host x\n\tProxyCommand nc %h %p\n", "hooks/pre-commit": "#!/bin/sh\n"})
	var mem gitMemory
	var rec recorded
	if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err != nil {
		t.Fatalf("the first look refused: %v", err)
	}
	writeFiles(t, ws, map[string]string{
		"out/.git":        "gitdir: " + outside + "\n",
		".ABHED/x/config": "[core]\n\tfsmonitor = ./x\n",
		"state/.git":      "gitdir: ../.ABHED/x\n",
	})
	for range 2 {
		_, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false)
		if err == nil || !strings.Contains(err.Error(), "out/.git") || !strings.Contains(err.Error(), "state/.git") {
			t.Fatalf("a pointer outside the workspace was not refused: %v", err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "config")); !strings.Contains(string(b), "ProxyCommand") {
		t.Errorf("a config outside the workspace was rewritten: %q", b)
	}
	if _, err := os.Stat(filepath.Join(outside, "hooks", "pre-commit")); err != nil {
		t.Errorf("hooks outside the workspace were moved: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, ".ABHED", "x", "config")); !strings.Contains(string(b), "fsmonitor") {
		t.Errorf("a config under .abhed was rewritten: %q", b)
	}
}

// A planted config that is a link, or a hard link, is never written through.
func TestPlantedConfigLinkIsNotWrittenThrough(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := workspace(t)
	other := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(other, []byte("[core]\n\tfsmonitor = ./other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mem gitMemory
	var rec recorded
	if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err != nil {
		t.Fatalf("the first look refused: %v", err)
	}
	writeFiles(t, ws, map[string]string{"l/.git/HEAD": "ref: refs/heads/main\n", "h/.git/HEAD": "ref: refs/heads/main\n",
		"h/keep": "[core]\n\tfsmonitor = ./x\n"})
	if err := os.Symlink(other, filepath.Join(ws, "l", ".git", "config")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(ws, "h", "keep"), filepath.Join(ws, "h", ".git", "config")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("a linked config was not refused: %v", err)
		}
	}
	if b, _ := os.ReadFile(other); !strings.Contains(string(b), "./other") {
		t.Errorf("the link's target was rewritten: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "h", "keep")); !strings.Contains(string(b), "fsmonitor") {
		t.Errorf("a hard link's other name was rewritten: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "h", ".git", "config")); strings.Contains(string(b), "fsmonitor") {
		t.Errorf("the planted config kept %q", b)
	}
}

// A planted repository that cannot be cleaned is refused on this command and
// every one after, until it is.
func TestPlantedRepositoryUncleanedStaysRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through read-only modes")
	}
	t.Setenv("HOME", t.TempDir())
	ws := workspace(t)
	var mem gitMemory
	var rec recorded
	if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err != nil {
		t.Fatalf("the first look refused: %v", err)
	}
	writeFiles(t, ws, map[string]string{"n/.git/HEAD": "ref: refs/heads/main\n", "n/.git/config": "[core]\n\tfsmonitor = ./x\n",
		"n/.git/hooks/post-checkout": "#!/bin/sh\n"})
	gitDir := filepath.Join(ws, "n", ".git")
	if err := os.Chmod(gitDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) })
	for i := range 2 {
		if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err == nil || !strings.Contains(err.Error(), "could not be") {
			t.Fatalf("command %d ran with the repository uncleaned: %v", i+1, err)
		}
	}
	if err := os.Chmod(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err == nil {
		t.Fatal("the repository was cleaned without a refusal")
	}
	if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err != nil {
		t.Fatalf("a cleaned repository still refuses: %v", err)
	}
}

// A new .git file naming a git folder already there never has it changed: it
// is refused while that names a program, and let through when it names none.
func TestPlantedPointerToAKnownFolderChangesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := workspace(t)
	own := "[core]\n\tbare = false\n\teditor = vim\n[credential]\n\thelper = osxkeychain\n[alias]\n\tst = status\n"
	writeFiles(t, ws, map[string]string{
		".git/HEAD": "ref: refs/heads/main\n", ".git/config": own, ".git/hooks/pre-commit": "#!/bin/sh\n",
		".git/modules/z/HEAD": "ref: refs/heads/main\n", ".git/modules/z/config": "[core]\n\tworktree = ../../../z\n",
	})
	var mem gitMemory
	var rec recorded
	if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err != nil {
		t.Fatalf("the first look refused: %v", err)
	}
	writeFiles(t, ws, map[string]string{"x/.git": "gitdir: ../.git\n"})
	for range 2 {
		if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err == nil || !strings.Contains(err.Error(), "x/.git") ||
			!strings.Contains(err.Error(), "core.editor") {
			t.Fatalf("a pointer to the workspace's git folder was not refused: %v", err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(ws, ".git", "config")); string(b) != own {
		t.Errorf("the workspace's own config was changed: %q", b)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git", "hooks", "pre-commit")); err != nil {
		t.Errorf("the workspace's own hook was moved: %v", err)
	}
	if err := os.Remove(filepath.Join(ws, "x", ".git")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, ws, map[string]string{"z/.git": "gitdir: ../.git/modules/z\n"})
	if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err != nil {
		t.Fatalf("a submodule's pointer naming nothing was refused: %v", err)
	}
}

// A git folder there at the first look and moved to a new .git is not changed.
func TestMovedKnownGitFolderIsNotChanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := workspace(t)
	sub := "[core]\n\tworktree = ../../../z\n[alias]\n\tst = status\n"
	writeFiles(t, ws, map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".git/config": "[core]\n",
		".git/modules/z/HEAD": "ref: refs/heads/main\n", ".git/modules/z/config": sub})
	var mem gitMemory
	var rec recorded
	if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err != nil {
		t.Fatalf("the first look refused: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "n"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(ws, ".git", "modules", "z"), filepath.Join(ws, "n", ".git")); err != nil {
		t.Fatal(err)
	}
	if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err == nil || !strings.Contains(err.Error(), "first look") {
		t.Fatalf("a moved git folder was not refused: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "n", ".git", "config")); string(b) != sub {
		t.Errorf("a git folder there from the start was changed: %q", b)
	}
}

// When the first look stopped at its bound, a repository found later may be
// the person's, so it is refused and left as it is.
func TestRepositoryPastABoundedFirstLookIsNotChanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	old := gitWalkFolders
	gitWalkFolders = 3
	t.Cleanup(func() { gitWalkFolders = old })
	ws := workspace(t)
	for _, d := range []string{"a", "b", "c", "d"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	mine := "[core]\n\tfsmonitor = ./mine\n"
	writeFiles(t, ws, map[string]string{"z/.git/HEAD": "ref: refs/heads/main\n", "z/.git/config": mine})
	var mem gitMemory
	var rec recorded
	if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err != nil {
		t.Fatalf("the first look refused: %v", err)
	}
	for _, d := range []string{"a", "b", "c", "d"} {
		if err := os.Remove(filepath.Join(ws, d)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitGuardFor(rec.ctx(), ws, "bwrap", &mem, false); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("a repository past the bound was not refused: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "z", ".git", "config")); string(b) != mine {
		t.Errorf("a repository past the bound was changed: %q", b)
	}
}

// A .git folder reached through a link is not opened for writing.
func TestNewGitFolderThroughALinkIsNotOpened(t *testing.T) {
	ws := workspace(t)
	writeFiles(t, ws, map[string]string{".git/HEAD": "ref: refs/heads/main\n"})
	if err := os.Symlink(ws, filepath.Join(ws, "x")); err != nil {
		t.Fatal(err)
	}
	if r, err := openNewGitFolder(ws, filepath.Join(ws, "x", ".git"), nil); err == nil {
		_ = r.Close()
		t.Fatal("a folder reached through a link was opened")
	}
	r, err := openNewGitFolder(ws, filepath.Join(ws, ".git"), nil)
	if err != nil {
		t.Fatalf("a real folder was not opened: %v", err)
	}
	_ = r.Close()
}
