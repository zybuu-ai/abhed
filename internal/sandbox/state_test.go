package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The deny for Abhed's own state must come after the workspace allow, because
// in a Seatbelt profile the later rule wins.
func TestSeatbeltProfileShieldsHarnessState(t *testing.T) {
	ws := workspace(t)
	s := NewProcess(Policy{Workspace: ws})
	profile := s.seatbeltProfile()
	allow := strings.Index(profile, `(allow file-write* (subpath "`+ws+`"))`)
	deny := strings.Index(profile, `(deny file-write* (subpath "`+filepath.Join(ws, stateDir)+`"))`)
	read := strings.Index(profile, `(deny file-read* (subpath "`+filepath.Join(ws, stateDir)+`"))`)
	if allow < 0 || deny < 0 || read < 0 || deny < allow {
		t.Fatalf("state directory is not shielded after the workspace allow:\n%s", profile)
	}
}

// A command in the sandbox can neither read the configuration nor plant a
// users file, while the rest of the workspace stays writable.
func TestProcessSandboxShieldsHarnessState(t *testing.T) {
	requireNetNS(t)
	ws := workspace(t)
	state := filepath.Join(ws, stateDir)
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "config.json"), []byte(`{"secret":"key-123"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := processSandbox(t, ws, false)

	out, _ := runIn(t, s, ws, "cat .abhed/config.json 2>&1; echo ---; echo planted > .abhed/users.json 2>&1; echo ok > fine.txt; echo done")
	if strings.Contains(out, "key-123") {
		t.Fatalf("the configuration was readable from a command:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(state, "users.json")); err == nil {
		t.Fatalf("a command planted a users file:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(ws, "fine.txt")); err != nil {
		t.Fatalf("the workspace itself stopped being writable:\n%s", out)
	}
}

// The state directory's denial must not break what walks the workspace: ls
// -R, find, pytest's collection and git all list it, and none may read it.
func TestProcessSandboxWalksPastHarnessState(t *testing.T) {
	requireNetNS(t)
	ws := workspace(t)
	state := filepath.Join(ws, stateDir)
	if err := os.MkdirAll(filepath.Join(state, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"config.json", "sub/users.json"} {
		if err := os.WriteFile(filepath.Join(state, f), []byte(`{"secret":"key-123"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "test_stats.py"), []byte("def test_one():\n    assert 1 + 1 == 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := processSandbox(t, ws, false)

	for _, walk := range []string{"ls -R", "ls -Ra", "find .", "du -s ."} {
		if out, err := runIn(t, s, ws, walk); err != nil {
			t.Errorf("%s failed in a workspace with %s: %v\n%s", walk, stateDir, err, out)
		}
	}
	for _, f := range []string{"config.json", "sub/users.json"} {
		if out, err := runIn(t, s, ws, "cat "+stateDir+"/"+f); err == nil || strings.Contains(out, "key-123") {
			t.Errorf("%s/%s was readable:\n%s", stateDir, f, out)
		}
	}
	if out, _ := runIn(t, s, ws, "echo x > "+stateDir+"/planted; echo x > "+stateDir+"/config.json"); fileExists(filepath.Join(state, "planted")) {
		t.Errorf("a command wrote into %s:\n%s", stateDir, out)
	}
	if got, _ := os.ReadFile(filepath.Join(state, "config.json")); !strings.Contains(string(got), "key-123") {
		t.Errorf("a command overwrote %s/config.json", stateDir)
	}

	if _, err := exec.LookPath("git"); err == nil {
		out, err := runIn(t, s, ws, "git init -q && git add -A && git -c user.name=t -c user.email=t@t -c commit.gpgsign=false commit -qm x && git ls-files")
		if err != nil || strings.Contains(out, stateDir) || !strings.Contains(out, "test_stats.py") {
			t.Errorf("git add and commit in a workspace with %s: %v\n%s", stateDir, err, out)
		}
	}
	pytest, err := exec.LookPath("pytest")
	home, _ := os.UserHomeDir()
	if err != nil && home != "" && fileExists(filepath.Join(home, ".local/bin/pytest")) {
		pytest, err = filepath.Join(home, ".local/bin/pytest"), nil
	}
	// bubblewrap does not bind the home directory, so a pytest there cannot run inside.
	if err != nil || (runtime.GOOS == "linux" && home != "" && strings.HasPrefix(pytest, home)) {
		t.Log("no pytest the sandbox can run; its collection is not exercised")
		return
	}
	for _, args := range []string{"-q", "-q test_stats.py"} {
		if out, err := runIn(t, s, ws, pytest+" -p no:cacheprovider "+args); err != nil || !strings.Contains(out, "1 passed") {
			t.Errorf("pytest %s failed in a workspace with %s: %v\n%s", args, stateDir, err, out)
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
