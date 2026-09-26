package sandboxconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A configured state file that commands could reach is refused at start: a
// deny rule on the file does not hold where a folder above it can be moved.
func TestStatePathInAWritableAreaIsRefused(t *testing.T) {
	base := outsideWritableAreas(t)
	t.Setenv("TMPDIR", filepath.Join(base, "tmp"))
	ws := filepath.Join(base, "ws")
	home := filepath.Join(base, "home")
	t.Setenv("HOME", home)
	t.Setenv("ABHED_SECRETS_FILE", "")
	elsewhere := filepath.Join(base, "state")
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	ws2 := filepath.Join(base, "ws2")
	if err := os.MkdirAll(filepath.Join(base, "tmp", "fake-state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ws2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "tmp", "fake-state"), filepath.Join(ws2, ".abhed")); err != nil {
		t.Fatal(err)
	}
	// A folder outside that is really a link into the workspace.
	link := filepath.Join(elsewhere, "linked")
	if err := os.Symlink(filepath.Join(ws, "sub"), link); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		users   string
		refused bool
	}{
		{filepath.Join(ws, "sub", "users.json"), true},
		{filepath.Join(link, "new", "users.json"), true},
		{filepath.Join(base, "tmp", "abhed-users.json"), true},
		{filepath.Join(elsewhere, "users.json"), false},
		{filepath.Join(ws, ".abhed", "users.json"), false},
		{filepath.Join(home, ".abhed", "users.json"), false},
		// A .abhed that is a link to a temp folder shields nothing.
		{filepath.Join(ws2, ".abhed", "users.json"), true},
		{"", false},
	}
	for _, c := range cases {
		cfg := config.Default()
		cfg.Auth.UsersFile = c.users
		w := ws
		if strings.HasPrefix(c.users, ws2) {
			w = ws2
		}
		err := CheckStatePaths(cfg, w)
		if c.refused != (err != nil) {
			t.Errorf("users_file %q: refused=%v, want %v (%v)", c.users, err != nil, c.refused, err)
		}
		if err != nil && !strings.Contains(err.Error(), "refusing to start") {
			t.Errorf("the refusal should say so: %v", err)
		}
	}
}

// outsideWritableAreas makes a folder that no writable area holds, so the
// test runs where temp folders live under /tmp too: under the home directory,
// or else under this package's folder, removed when the test ends.
func outsideWritableAreas(t *testing.T) string {
	t.Helper()
	var parents []string
	if home, err := os.UserHomeDir(); err == nil {
		parents = append(parents, home)
	}
	if wd, err := os.Getwd(); err == nil {
		parents = append(parents, wd)
	}
	for _, p := range parents {
		if under([]string{tools.RealPath(p)}, sandbox.WritableAreas()) {
			continue
		}
		dir, err := os.MkdirTemp(p, ".abhed-statepaths-*")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	t.Fatal("no folder outside the writable areas to test in")
	return ""
}
