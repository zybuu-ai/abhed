package hostgit

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Each setting in the repository's own files that would steer host git
// stops the broker; the same in the person's own files does not.
func TestPlantedProgramRefuses(t *testing.T) {
	for _, key := range []string{"hook.x.command", "remote.origin.vcs", "core.worktree", "remote.origin.uploadpack",
		"remote.origin.receivepack", "remote.origin.proxy", "remote.origin.proxyauthmethod", "http.proxy",
		"http.sslverify", "http.https://git.example.extraheader", "http.curloptresolve", "http.cookiefile",
		"url.https://evil.example/.insteadof", "url.x.pushinsteadof"} {
		for scope, want := range map[string]bool{"local": true, "worktree": true, "global": false, "system": false} {
			if got := plantedProgram(scope+"\x00"+key+"\nv\x00") != ""; got != want {
				t.Errorf("%s in %s: refused %v, want %v", key, scope, got, want)
			}
		}
	}
	for _, key := range []string{"remote.origin.url", "remote.origin.fetch", "branch.main.remote", "credential.helper"} {
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
