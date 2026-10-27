package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// A process-tier stdio server sees none of the home but its own HOME and
// install folder; the workspace stays writable and its .abhed hidden.
func TestServerFilesAreConfined(t *testing.T) {
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	secret := filepath.Join(home, ".ssh", "id_test")
	if err := os.MkdirAll(filepath.Dir(secret), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("PRIVATE-KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(home, "tools", "srv", "bin", "srv")
	if err := os.MkdirAll(filepath.Dir(tool), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tool, []byte("#!/bin/sh\ncat \"$1\" 2>/dev/null; echo planted > \"$2\" 2>/dev/null; echo own > \"$HOME/own\" && cat \"$HOME/own\"; echo ws > \"$3/w\" && cat \"$3/w\"; cat \"$3/.abhed/s\" 2>/dev/null; echo done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := workspace(t)
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(ws, ".abhed", "s"), []byte("STATE"), 0o600)
	s := NewProcess(DefaultPolicy(ws))
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	if s.Backend() == "bwrap" && os.Geteuid() == 0 {
		t.Skip("as root bubblewrap starts no server; TestServerRefusedAsRoot covers it")
	}
	defer func() { _ = s.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	plant := filepath.Join(home, "Library", "LaunchAgents", "evil.plist")
	_ = os.MkdirAll(filepath.Dir(plant), 0o700)
	cmd, err := s.ServerCommand(ctx, "files", []string{tool, secret, plant, ws}, os.Environ(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := cmd.CombinedOutput()
	got := string(out)
	if strings.Contains(got, "PRIVATE-KEY") || strings.Contains(got, "STATE") {
		t.Fatalf("the server read the home or the state:\n%s", got)
	}
	if _, err := os.Stat(plant); err == nil {
		t.Fatal("the server planted a file in the home")
	}
	if !strings.Contains(got, "own") || !strings.Contains(got, "ws") || !strings.Contains(got, "done") {
		t.Fatalf("the server could not use its own home, its install folder or the workspace:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".abhed", "mcp", "files", "home", "own")); err != nil {
		t.Fatalf("the server's own home was not kept: %v", err)
	}
}

func TestInstallPrefix(t *testing.T) {
	for _, c := range []struct{ program, want string }{
		{"/h/.nvm/versions/node/v20/bin/node", "/h/.nvm/versions/node/v20"},
		{"/h/.local/bin/uvx", "/h/.local/bin"},
		{"/h/.cargo/bin/srv", "/h/.cargo/bin"},
		{"/h/bin/srv", "/h/bin"},
		{"/h/srv/server", "/h/srv"},
		{"/usr/local/bin/node", ""},
		{"", ""},
	} {
		if got := installPrefix("/h", c.program); got != c.want {
			t.Errorf("%s: %q, want %q", c.program, got, c.want)
		}
	}
}

// A server's install folder is never Abhed's state, nor a folder holding it
// (the home itself): such a server is refused, naming why.
func TestServerInstallFolderNeverHoldsState(t *testing.T) {
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	for _, rel := range []string{".abhed/bin/srv", ".abhed/tools/srv", "srv"} {
		prog := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(prog), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := serverFilesIn("s", []string{prog}, nil, nil); err == nil || !strings.Contains(err.Error(), "holds Abhed's state") {
			t.Errorf("%s: %v", rel, err)
		}
	}
	ok := filepath.Join(home, "tools", "srv", "bin", "srv")
	_ = os.MkdirAll(filepath.Dir(ok), 0o755)
	_ = os.WriteFile(ok, []byte("#!/bin/sh\n"), 0o755)
	if f, err := serverFilesIn("s", []string{ok}, nil, nil); err != nil || f.prefix != filepath.Join(home, "tools", "srv") {
		t.Fatalf("an ordinary install folder: %+v %v", f, err)
	}
}

// A server's code named by its configured arguments, under the home, is
// readable to it, as its install folder is; never home itself, a broad
// folder, Abhed's state or a folder of credentials.
func TestServerScriptFoldersReadable(t *testing.T) {
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	mk := func(rel string) string {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("echo ok\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	script := mk(".qa/mcp/ticket.sh")
	f, err := serverFilesIn("s", []string{"/bin/sh", script, "--flag", "relative.py"}, nil, nil)
	if err != nil || !slices.Equal(f.scripts, []string{filepath.Dir(script)}) {
		t.Fatalf("scripts %v %v", f.scripts, err)
	}
	if !strings.Contains(f.seatbeltRules(), fmt.Sprintf("(allow file-read-data (subpath %q))", filepath.Dir(script))) ||
		!slices.Contains(f.bwrapArgs(), filepath.Dir(script)) {
		t.Fatalf("not shown: %s %v", f.seatbeltRules(), f.bwrapArgs())
	}
	// Credentials, Abhed's state, and home or a folder holding them:
	// nothing, not even the file.
	for _, rel := range []string{".ssh/x.py", ".aws/cli/x.py", ".abhed/x.py", ".config/gcloud/x.py", "top.py", ".config/x.py", "Library/x.py"} {
		if dirs, files := scriptFolders(home, []string{mk(rel)}); len(dirs)+len(files) != 0 {
			t.Errorf("%s made %v %v readable", rel, dirs, files)
		}
	}
	// A folder directly in home and iCloud Drive's root: the file alone.
	for _, rel := range []string{"Documents/server.py", "Desktop/server.py",
		"Downloads/server.py", "Projects/server.py", "Library/Mobile Documents/com~apple~CloudDocs/server.py"} {
		p := mk(rel)
		if dirs, files := scriptFolders(home, []string{p}); len(dirs) != 0 || !slices.Equal(files, []string{p}) {
			t.Errorf("%s: folders %v files %v", rel, dirs, files)
		}
	}
	// A script's own folder inside one of them is shown, and only it.
	own := mk("Downloads/mcp-tickets/server.py")
	if dirs, _ := scriptFolders(home, []string{own}); !slices.Equal(dirs, []string{filepath.Dir(own)}) {
		t.Errorf("a script's own folder in Downloads: %v", dirs)
	}
	// The server runs its script from there, confined, on macOS.
	if runtime.GOOS != "darwin" {
		return
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec")
	}
	ws := t.TempDir()
	sb := NewProcess(DefaultPolicy(ws))
	cmd, err := sb.ServerCommand(context.Background(), "tickets", []string{"/bin/sh", script}, []string{"PATH=/usr/bin:/bin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("the server could not run its own script: %v %s", err, out)
	}
}

// A program's install folder follows the same rule: one directly in home
// shows the program alone; one in a credential folder is refused.
func TestServerInstallPrefixIsNotATopLevelFolder(t *testing.T) {
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	mk := func(rel string) string {
		p := filepath.Join(home, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	dl := mk("Downloads/mcp-server")
	f, err := serverFilesIn("s", []string{dl}, nil, nil)
	if err != nil || f.prefix != "" || !slices.Equal(f.shown(), []string{dl}) {
		t.Fatalf("a program in Downloads: %+v %v", f, err)
	}
	if _, err := serverFilesIn("s", []string{mk(".ssh/tool")}, nil, nil); err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("a program in ~/.ssh: %v", err)
	}
	own := mk("tools/srv/bin/srv")
	if f, err := serverFilesIn("s", []string{own}, nil, nil); err != nil || f.prefix != filepath.Join(home, "tools", "srv") {
		t.Fatalf("an install folder of its own: %+v %v", f, err)
	}
}
