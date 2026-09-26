package sandboxconfig

import (
	"context"
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

// A state file with a second name is refused at start: the sandbox guards the
// state by path, so a command could rewrite the file through the other name.
func TestStateFileWithASecondNameIsRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	for name, file := range map[string]string{
		"workspace config": "ws/.abhed/config.json",
		"users":            "ws/.abhed/users.json",
		"any state file":   "ws/.abhed/skills/tidy/run.sh",
		"home secrets":     "home/.abhed/secrets.json",
	} {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			path := filepath.Join(ws, strings.TrimPrefix(file, "ws/"))
			if strings.HasPrefix(file, "home/") {
				path = filepath.Join(home, strings.TrimPrefix(file, "home/"))
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)
			cfg := config.Default()
			if err := CheckStatePaths(cfg, ws); err != nil {
				t.Fatalf("a state file with one name was refused: %v", err)
			}
			if err := os.Link(path, filepath.Join(ws, "notes.json")); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			err := CheckStatePaths(cfg, ws)
			if err == nil || !strings.Contains(err.Error(), "refusing to start") || !strings.Contains(err.Error(), "2 names") {
				t.Fatalf("a state file with a second name was not refused: %v", err)
			}
			if _, err := Build(cfg, ws); err == nil {
				t.Fatal("the sandbox was built over a state file with a second name")
			}
		})
	}
}

// A run whose workspace is a worktree inside the repository cannot read the
// repository's .abhed: it is state for the run as the worktree's own is.
func TestStateOfTheRepositoryAroundAWorktreeIsHidden(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(repo, tools.WorktreesDir, "issue-1")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(repo, ".abhed", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfgFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgFile, []byte(`{"secret":"repo-config"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	visible := filepath.Join(repo, "README.md")
	if err := os.WriteFile(visible, []byte("repo-readme"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Sandbox.MinTier = "process"
	sb, err := Build(cfg, worktree, repo)
	if err != nil {
		t.Skipf("no process sandbox here: %v", err)
	}
	// Commands must run at all, or the refusal below proves nothing.
	if out, err := sb.Command(context.Background(), worktree, "cat "+visible).CombinedOutput(); err != nil || !strings.Contains(string(out), "repo-readme") {
		t.Skipf("the process sandbox cannot run a command here: %v %s", err, out)
	}
	out, _ := sb.Command(context.Background(), worktree, "cat "+cfgFile+"; echo x >> "+cfgFile).CombinedOutput()
	if strings.Contains(string(out), "repo-config") {
		t.Fatalf("the run read the repository's configuration: %s", out)
	}
	if got, _ := os.ReadFile(cfgFile); string(got) != `{"secret":"repo-config"}` {
		t.Fatalf("the run changed the repository's configuration: %q", got)
	}
}
