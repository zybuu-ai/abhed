package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
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
// -R, pytest's collection and git stat it and pass it by, and none may read
// it. On macOS find and du still report it; bubblewrap shows an empty folder.
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

	for _, walk := range []string{"ls -R", "ls -la", "ls -ld " + stateDir} {
		if out, err := runIn(t, s, ws, walk); err != nil {
			t.Errorf("%s failed in a workspace with %s: %v\n%s", walk, stateDir, err, out)
		}
	}
	for _, f := range []string{"config.json", "sub/users.json"} {
		if out, err := runIn(t, s, ws, "cat "+stateDir+"/"+f); err == nil || strings.Contains(out, "key-123") {
			t.Errorf("%s/%s was readable:\n%s", stateDir, f, out)
		}
	}
	// Nor by another name: a link to a file or to the folder, a hard link, a clone.
	for name, probe := range map[string]string{
		"a symlink to the file":   "ln -s " + stateDir + "/config.json via-link; cat via-link",
		"a symlink to the folder": "ln -s " + stateDir + " via-dir; cat via-dir/config.json",
		"a hard link":             "ln " + stateDir + "/config.json via-hard; cat via-hard",
		"a clone":                 "cp -c " + stateDir + "/config.json via-clone 2>/dev/null || cp " + stateDir + "/config.json via-clone; cat via-clone",
	} {
		if out, _ := runIn(t, s, ws, probe+" 2>&1; rm -rf via-*"); strings.Contains(out, "key-123") {
			t.Errorf("%s read %s:\n%s", name, stateDir, out)
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
		if err != nil || regexp.MustCompile(`(?m)^`+regexp.QuoteMeta(stateDir)+`/`).MatchString(out) || !strings.Contains(out, "test_stats.py") {
			t.Errorf("git add and commit in a workspace with %s: %v\n%s", stateDir, err, out)
		}
	}
	// The sandbox writes nothing of its own into the workspace's state.
	if entries, _ := os.ReadDir(state); len(entries) != 2 {
		t.Errorf("%s holds %d entries, want the 2 the test made", stateDir, len(entries))
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

// A users file the configuration keeps outside .abhed is hidden from commands
// as .abhed is: not readable, and not replaceable.
func TestProcessSandboxShieldsConfiguredStatePaths(t *testing.T) {
	requireNetNS(t)
	ws := workspace(t)
	accounts := filepath.Join(ws, "accounts.json")
	if err := os.WriteFile(accounts, []byte(`{"admin":"hash-123"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy(ws)
	p.StatePaths = []string{accounts}
	s := NewProcess(p)
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	out, _ := runIn(t, s, ws, "cat accounts.json 2>&1; echo ---; echo '{}' > accounts.json 2>&1; echo done")
	if strings.Contains(out, "hash-123") {
		t.Fatalf("the users file was readable from a command:\n%s", out)
	}
	if got, _ := os.ReadFile(accounts); !strings.Contains(string(got), "hash-123") {
		t.Fatalf("a command replaced the users file:\n%s", out)
	}
}

// pinned makes an executable script at path and pins it.
func pinned(t *testing.T, path, body string) ReadableFile {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { // #nosec G306 -- a script the test runs
		t.Fatal(err)
	}
	f, err := PinReadable(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// A file named readable is readable and runnable even inside ~/.abhed,
// which stays hidden around it.
func TestReadableFileInsideHiddenState(t *testing.T) {
	requireNetNS(t)
	home := workspace(t)
	t.Setenv("HOME", home)
	state := filepath.Join(home, stateDir)
	f := pinned(t, filepath.Join(state, "status.sh"), "#!/bin/sh\necho status-ok\n")
	if err := os.WriteFile(filepath.Join(state, "config.json"), []byte(`{"k":"state-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := workspace(t)
	p := DefaultPolicy(ws)
	p.ReadableFiles = []ReadableFile{f}
	s := NewProcess(p)
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	out, _ := runIn(t, s, ws, "~/.abhed/status.sh; cat ~/.abhed/config.json 2>&1; echo done")
	if !strings.Contains(out, "status-ok") || strings.Contains(out, "state-secret") {
		t.Fatalf("%s", out)
	}
	// Without the name, the script is out of reach as before.
	out, _ = runIn(t, processSandbox(t, ws, false), ws, "~/.abhed/status.sh 2>&1; echo done")
	if strings.Contains(out, "status-ok") {
		t.Fatalf("the script ran without being named:\n%s", out)
	}
}

// A pinned file swapped after the check loses its allow: a link to a hidden
// script does not run it, and a link to the configuration does not read it.
func TestReadableFileSwappedLosesItsAllow(t *testing.T) {
	requireNetNS(t)
	home := workspace(t)
	t.Setenv("HOME", home)
	state := filepath.Join(home, stateDir)
	pinned(t, filepath.Join(state, "hidden.sh"), "#!/bin/sh\necho hidden-ran\n")
	if err := os.WriteFile(filepath.Join(state, "config.json"), []byte(`{"k":"state-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(home, "bin", "ok.sh")
	f := pinned(t, script, "#!/bin/sh\necho ok-ran\n")
	ws := workspace(t)
	p := DefaultPolicy(ws)
	p.ReadableFiles = []ReadableFile{f}
	s := NewProcess(p)
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	if out, _ := runIn(t, s, ws, script); !strings.Contains(out, "ok-ran") {
		t.Fatalf("the pinned script did not run: %s", out)
	}
	for _, target := range []string{filepath.Join(state, "hidden.sh"), filepath.Join(state, "config.json")} {
		if err := os.Remove(script); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, script); err != nil {
			t.Fatal(err)
		}
		if f.Same() {
			t.Fatalf("a link to %s passed for the pinned file", target)
		}
		out, _ := runIn(t, s, ws, script+" 2>&1; cat "+script+" 2>&1; echo done")
		if strings.Contains(out, "hidden-ran") || strings.Contains(out, "state-secret") {
			t.Fatalf("after a swap to %s:\n%s", target, out)
		}
	}
}

// Under bubblewrap a pinned file is bound read-only at its resolved path,
// after every mount that could hide it, and not at all once swapped for a
// link, another file or a folder.
func TestBwrapBindsReadableFilesLast(t *testing.T) {
	ws := workspace(t)
	dir := workspace(t)
	f := pinned(t, filepath.Join(dir, "status.sh"), "#!/bin/sh\n")
	p := DefaultPolicy(ws)
	p.ReadableFiles = []ReadableFile{f}
	s := &Process{policy: p, backend: "bwrap"}
	bindArgs := func() string { return strings.Join(s.wrap(t.Context(), ws, nil, "/bin/true").Args, " ") }
	bind := "--ro-bind " + f.Path + " " + f.Path
	if args := bindArgs(); !strings.Contains(args, bind) || strings.Index(args, bind) < strings.LastIndex(args, "--tmpfs") {
		t.Fatalf("%s", args)
	}
	// Each swap makes its replacement before the original goes, so a
	// reused inode number cannot pass for the pinned file.
	swaps := map[string]func(tmp string) error{
		"a folder": func(tmp string) error { return os.Mkdir(tmp, 0o700) },
		"a link":   func(tmp string) error { return os.Symlink(dir, tmp) },
		"another file": func(tmp string) error {
			return os.WriteFile(tmp, []byte("#!/bin/sh\n"), 0o700) // #nosec G306 -- a script the test names
		},
	}
	for name, swap := range swaps {
		tmp := f.Path + ".new"
		if err := swap(tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(f.Path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, f.Path); err != nil {
			t.Fatal(err)
		}
		if args := bindArgs(); strings.Contains(args, "--ro-bind "+f.Path) {
			t.Errorf("swapped for %s, still bound:\n%s", name, args)
		}
	}
}

// A pinned file rewritten in place, or only changed in its metadata, is no
// longer the pinned file: a reused inode number alone does not pass.
func TestReadableFileSameComparesMoreThanTheInode(t *testing.T) {
	path := filepath.Join(workspace(t), "status.sh")
	f := pinned(t, path, "#!/bin/sh\necho one\n")
	if !f.Same() {
		t.Fatal("the pinned file is not itself")
	}
	g := pinned(t, filepath.Join(workspace(t), "other.sh"), "#!/bin/sh\n")
	// A mode change moves the ctime alone: size and mtime stay.
	time.Sleep(20 * time.Millisecond)
	if err := os.Chmod(g.Path, 0o750); err != nil {
		t.Fatal(err)
	}
	if g.Same() {
		t.Error("a changed ctime passed")
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho two, longer\n"), 0o700); err != nil { // #nosec G306 -- the test's script
		t.Fatal(err)
	}
	if f.Same() {
		t.Error("a file rewritten in place passed")
	}
}

// Same size, same mtime put back after a rewrite: only the change time
// tells the file changed, and it is enough.
func TestReadableFileSameCatchesAChangeTimeAlone(t *testing.T) {
	path := filepath.Join(workspace(t), "status.sh")
	f := pinned(t, path, "#!/bin/sh\necho one\n")
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho two\n"), 0o700); err != nil { // #nosec G306 -- the test's script
		t.Fatal(err)
	}
	if err := os.Chtimes(path, f.Info.ModTime(), f.Info.ModTime()); err != nil {
		t.Fatal(err)
	}
	cur, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(cur, f.Info) || cur.Size() != f.Info.Size() || !cur.ModTime().Equal(f.Info.ModTime()) || cur.Mode() != f.Info.Mode() {
		t.Fatal("the setup changed more than the change time")
	}
	if f.Same() {
		t.Fatal("a file differing only in its change time passed")
	}
}
