package sandboxconfig

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/secrets"
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

// A run started in a subfolder is refused when the enclosing repository's
// state has a second name: a command could rewrite it through that name.
func TestEnclosingRepositoryStateWithASecondNameIsRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	repo := t.TempDir()
	ws := filepath.Join(repo, "services", "ledger")
	cfgFile := filepath.Join(repo, ".abhed", "config.json")
	for _, d := range []string{ws, filepath.Dir(cfgFile)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cfgFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	if err := CheckStatePaths(cfg, ws); err != nil {
		t.Fatalf("an enclosing repository's state with one name was refused: %v", err)
	}
	if err := os.Link(cfgFile, filepath.Join(ws, "cfg-link.json")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	err := CheckStatePaths(cfg, ws)
	if err == nil || !strings.Contains(err.Error(), "2 names") || !strings.Contains(err.Error(), cfgFile) {
		t.Fatalf("a second name for the enclosing repository's config was not refused: %v", err)
	}
	if _, err := Build(cfg, ws); err == nil {
		t.Fatal("the sandbox was built over an enclosing repository's config with a second name")
	}
}

// A workspace reached through a link is judged by where it really is: the
// repository around the real folder holds the config a run there loads.
func TestEnclosingStateThroughALinkedWorkspaceIsRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	repo, other := t.TempDir(), t.TempDir()
	real := filepath.Join(repo, "services", "ledger")
	cfgFile := filepath.Join(repo, ".abhed", "config.json")
	for _, d := range []string{real, filepath.Dir(cfgFile)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cfgFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(other, "ledger")
	if err := os.Symlink(real, ws); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if err := os.Link(cfgFile, filepath.Join(real, "cfg-link.json")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := CheckStatePaths(config.Default(), ws); err == nil || !strings.Contains(err.Error(), "2 names") {
		t.Fatalf("a second name for the real repository's config was not refused: %v", err)
	}
}

// Only the files a run loads count above the workspace, and nothing in a
// folder anyone may write, such as /tmp: another user could otherwise stop
// every workspace under it from starting.
func TestAncestorStateCountsOnlyWhatARunLoads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	top := t.TempDir()
	shared := filepath.Join(top, "shared")
	repo := filepath.Join(shared, "repo")
	ws := filepath.Join(repo, "svc")
	for _, d := range []string{filepath.Join(shared, ".abhed"), filepath.Join(repo, ".abhed"), ws} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Not a file a run loads: a second name for it is not the run's concern.
	notes := filepath.Join(repo, ".abhed", "notes.txt")
	if err := os.WriteFile(notes, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(notes, filepath.Join(ws, "notes-link")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := CheckStatePaths(config.Default(), ws); err != nil {
		t.Fatalf("a linked file an enclosing .abhed holds but no run loads was refused: %v", err)
	}
	// A config in a world-writable sticky folder above the workspace.
	planted := filepath.Join(shared, ".abhed", "config.json")
	if err := os.WriteFile(planted, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(planted, filepath.Join(shared, ".abhed", "config.json.2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(shared); err != nil || info.Mode()&os.ModeSticky == 0 || info.Mode().Perm()&0o002 == 0 {
		t.Skip("a world-writable sticky folder cannot be made here")
	}
	if err := CheckStatePaths(config.Default(), ws); err != nil {
		t.Fatalf("a linked config in a shared sticky folder above the workspace blocked the start: %v", err)
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

// The per-account secrets directory is state, refused to commands like the store.
func TestStatePathsHoldTheAccountStores(t *testing.T) {
	t.Setenv(secrets.EnvAccountsDir, "/srv/abhed/accounts")
	if !slices.Contains(StatePaths(config.Default(), t.TempDir()), "/srv/abhed/accounts") {
		t.Error("the per-account stores are not among the state paths")
	}
}

// skills.dirs reach the policy absolute, ~/ as the home folder and a relative
// folder from the one Abhed runs in, and none when skills are off.
func TestPolicyCarriesSkillDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd, _ := os.Getwd()
	cfg := config.Config{}
	cfg.Skills.Dirs = []string{"~/team-skills", "rel/skills", "/opt/skills"}
	p, err := Policy(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opt, _ := filepath.Abs("/opt/skills")
	want := []string{filepath.Join(home, "team-skills"), filepath.Join(cwd, "rel", "skills"), opt}
	if !slices.Equal(p.SkillDirs, want) {
		t.Errorf("skill dirs %v, want %v", p.SkillDirs, want)
	}
	cfg.Skills.Disabled = true
	if p, err := Policy(cfg, t.TempDir()); err != nil || len(p.SkillDirs) != 0 {
		t.Errorf("skills off: %v %v", p.SkillDirs, err)
	}
}
