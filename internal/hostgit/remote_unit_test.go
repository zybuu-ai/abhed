package hostgit

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// Each setting in the repository's own files that would steer host git
// stops the broker; the same in the person's own files does not.
func TestPlantedProgramRefuses(t *testing.T) {
	for _, key := range []string{"hook.x.command", "remote.origin.vcs", "core.worktree", "remote.origin.uploadpack",
		"remote.origin.receivepack", "remote.origin.proxy", "remote.origin.proxyauthmethod", "http.proxy",
		"http.sslverify", "http.https://git.example.extraheader", "http.curloptresolve", "http.cookiefile",
		"url.https://evil.example/.insteadof", "url.x.pushinsteadof", "core.sshcommand", "core.askpass", "core.gitproxy",
		"credential.helper", "credential.https://git.example.helper"} {
		for scope, want := range map[string]bool{"local": true, "worktree": true, "global": false, "system": false} {
			if got := plantedProgram(scope+"\x00"+key+"\nv\x00") != ""; got != want {
				t.Errorf("%s in %s: refused %v, want %v", key, scope, got, want)
			}
		}
	}
	for _, key := range []string{"remote.origin.url", "remote.origin.fetch", "branch.main.remote", "credential.username", "credential.https://git.example.username"} {
		if why := plantedProgram("local\x00" + key + "\nv\x00"); why != "" {
			t.Errorf("%s refused: %s", key, why)
		}
	}
}

// A clone is moved in folder by folder with no link followed: a folder on
// the way that is a link, or a name already there, is refused and nothing
// is written through it.
func TestMoveIntoFollowsNoLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no clone is moved on this system")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, outside := filepath.Join(root, "ws"), filepath.Join(root, "outside")
	for _, d := range []string{filepath.Join(ws, "sub", "empty"), outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "sub", "taken")); err != nil {
		t.Fatal(err)
	}
	stage := func() string {
		d, err := os.MkdirTemp(root, "stage-")
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	for _, parts := range [][]string{{"link", "dest"}, {"sub", "taken"}, {"sub", "empty"}, {".git", "dest"}, {"sub", ".GIT"}} {
		if err := moveInto(ws, parts, stage()); err == nil {
			t.Errorf("%v was not refused", parts)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("written outside the workspace: %v", entries)
	}
	if err := moveInto(ws, []string{"sub", "dest"}, stage()); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(ws, "sub", "dest")); err != nil || !info.IsDir() {
		t.Fatalf("not moved in: %v", err)
	}
}

// A folder on the way checked as a real folder, then swapped for a link to
// a folder outside before the move, as a running command could: the move
// does not follow it.
func TestMoveIntoRefusesAFolderSwappedForALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no clone is moved on this system")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, outside := filepath.Join(root, "ws"), filepath.Join(root, "outside")
	for _, d := range []string{filepath.Join(ws, "sub"), outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	b := &HostBroker{workspace: ws, known: map[string]repoState{}}
	parts, err := b.cloneTarget(t.Context(), ws, "sub/dest")
	if err != nil {
		t.Fatal(err)
	}
	// After the check, before the move.
	if err := os.Rename(filepath.Join(ws, "sub"), filepath.Join(root, "sub-moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "sub")); err != nil {
		t.Fatal(err)
	}
	staged, err := os.MkdirTemp(root, "stage-")
	if err != nil {
		t.Fatal(err)
	}
	if err := moveInto(ws, parts, staged); err == nil {
		t.Fatal("the move followed a folder swapped for a link")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("written outside the workspace: %v", entries)
	}
}

// The bash tool knows a brokered line from this package's own test.
func TestToolsKnowsBrokeredLines(t *testing.T) {
	if tools.HostGitLine == nil || !tools.HostGitLine("git fetch origin") || tools.HostGitLine("make") {
		t.Fatal("tools.HostGitLine is not the broker's")
	}
}
