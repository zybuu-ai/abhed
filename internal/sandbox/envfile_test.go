package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The file holds the settings one to a line, readable only by this user, and
// goes with its folder; what a line cannot carry is refused.
func TestEnvFile(t *testing.T) {
	p, remove, err := EnvFile([]string{"HTTPS_PROXY=http://c:t@127.0.0.1:1", "JAVA_TOOL_OPTIONS=-Da=b -Dc=d"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "HTTPS_PROXY=http://c:t@127.0.0.1:1\nJAVA_TOOL_OPTIONS=-Da=b -Dc=d\n" {
		t.Fatalf("%q %v", b, err)
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{p: 0o600, filepath.Dir(p): 0o700} {
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
				t.Errorf("%s: %v %v", path, info.Mode().Perm(), err)
			}
		}
	}
	remove()
	if _, err := os.Stat(filepath.Dir(p)); !os.IsNotExist(err) {
		t.Errorf("the folder outlived remove: %v", err)
	}
	for _, bad := range []string{"A=b\nC=d", "NOEQUALS", "=x", "#A=b", " A=b", "A=b\x00"} {
		if _, _, err := EnvFile([]string{bad}); err == nil {
			t.Errorf("%q was written", bad)
		}
	}
}
