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

// A repository made after the first look, with an fsmonitor or a hook, has
// them taken out before the next command, which is refused and recorded;
// one there from the start, and a plain clone, are left alone.
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
