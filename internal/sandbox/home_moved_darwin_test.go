package sandbox

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

// At run time, with HOME moved: a workspace under the account's real home
// works, a private file beside it there is refused, and a toolchain on PATH
// under that home still runs and reads.
func TestProcessSandboxKeepsTheRealHomeWhenHomeIsMoved(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("this uid has no account home in the user database: %v", err)
	}
	if fi, err := os.Stat(u.HomeDir); err != nil || !fi.IsDir() {
		t.Skipf("the account home %s is not a folder here", u.HomeDir)
	}
	// A folder of the test's own in the real home, removed after.
	root, err := os.MkdirTemp(u.HomeDir, ".abhed-sandbox-test-")
	if err != nil {
		t.Skipf("cannot make a folder in the account home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	ws := filepath.Join(root, "ws")
	writeFiles(t, root, map[string]string{"ws/a.txt": "work", "private.txt": "secret", "tools/bin/tool": "#!/bin/sh\necho tool-ran\n", "tools/lib/x": "lib"})
	if err := os.Chmod(filepath.Join(root, "tools/bin/tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", filepath.Join(root, "tools", "bin")+":"+os.Getenv("PATH"))
	s := NewProcess(DefaultPolicy(ws))
	available(t, s)
	for cmd, want := range map[string]bool{
		"cat " + filepath.Join(ws, "a.txt"):         true,
		"cat " + filepath.Join(root, "private.txt"): false,
		"tool": true,
		"cat " + filepath.Join(root, "tools", "lib/x"): true,
		"ls " + root: false,
	} {
		out, err := runIn(t, s, ws, cmd)
		if got := err == nil && out != ""; got != want {
			t.Errorf("%s: ran %v (%q, %v), want %v", cmd, got, out, err, want)
		}
	}
}
