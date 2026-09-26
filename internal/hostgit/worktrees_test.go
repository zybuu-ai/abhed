package hostgit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ws := t.TempDir()
	if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return ws
}

// The worktrees folder is a real folder of the repository, outside the state,
// and the repository's status does not list it.
func TestWorktreesFolderIsExcludedAndOutsideTheState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := initRepo(t)
	dir, err := New(context.Background(), ws).Worktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir) != ".abhed-worktrees" || filepath.Dir(dir) != ws {
		t.Fatalf("worktrees folder %s", dir)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command("git", "-C", ws, "status", "--porcelain", "--untracked-files=all").Output()
	if strings.Contains(string(out), ".abhed-worktrees") {
		t.Fatalf("the worktrees folder shows in the status:\n%s", out)
	}
	if _, err := New(context.Background(), ws).Worktrees(context.Background()); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude")); strings.Count(string(data), "/.abhed-worktrees/") != 1 {
		t.Fatalf("the exclude line is not there once:\n%s", data)
	}
}

// A link planted where the worktrees folder goes, into the state or out of
// the repository, is refused, and nothing is made through it.
func TestWorktreesFolderRefusesALink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for name, target := range map[string]func(ws, outside string) string{
		"into the state":    func(ws, _ string) string { return filepath.Join(ws, ".abhed") },
		"out of the folder": func(_, outside string) string { return outside },
		// Relative, so it stays inside the repository however its path is spelled.
		"to a folder here": func(string, string) string { return "elsewhere" },
	} {
		t.Run(name, func(t *testing.T) {
			ws, outside := initRepo(t), t.TempDir()
			for _, d := range []string{".abhed", "elsewhere"} {
				if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			to := target(ws, outside)
			if err := os.Symlink(to, filepath.Join(ws, ".abhed-worktrees")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if dir, err := New(context.Background(), ws).Worktrees(context.Background()); err == nil {
				t.Fatalf("a linked worktrees folder was used: %s", dir)
			}
			if !filepath.IsAbs(to) {
				to = filepath.Join(ws, to)
			}
			if ents, _ := os.ReadDir(to); len(ents) != 0 {
				t.Fatalf("something was made through the link: %v", ents)
			}
		})
	}
}

// A worktree path something already holds is refused.
func TestNewWorktreeDirRefusesWhatIsThere(t *testing.T) {
	dir := t.TempDir()
	if err := NewWorktreeDir(filepath.Join(dir, "new")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "planted")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := NewWorktreeDir(filepath.Join(dir, "planted")); err == nil {
		t.Fatal("a planted link was taken for a new worktree")
	}
}

// The worktree exclude line is written as the file tools write: a planted
// info/exclude link into Abhed's state, or an info folder linked out of the
// workspace, is not written through.
func TestExcludeWorktreesWritesNoLink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	for _, name := range []string{"into the state", "out of the workspace"} {
		t.Run(name, func(t *testing.T) {
			ws, outside := t.TempDir(), t.TempDir()
			if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
				t.Fatalf("git init: %v\n%s", err, out)
			}
			if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
				t.Fatal(err)
			}
			users := filepath.Join(ws, ".abhed", "users.json")
			if err := os.WriteFile(users, []byte("secret-hash"), 0o600); err != nil {
				t.Fatal(err)
			}
			info := filepath.Join(ws, ".git", "info")
			if err := os.RemoveAll(info); err != nil {
				t.Fatal(err)
			}
			watched := users
			if name == "into the state" {
				if err := os.MkdirAll(info, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(users, filepath.Join(info, "exclude")); err != nil {
					t.Fatal(err)
				}
			} else {
				watched = filepath.Join(outside, "exclude")
				if err := os.WriteFile(watched, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, info); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(watched)
			_, _ = New(context.Background(), ws).Worktrees(context.Background())
			if after, _ := os.ReadFile(watched); string(after) != string(before) {
				t.Fatalf("the exclude line was written through the link: %q", after)
			}
		})
	}
}

// A worktree that git made somewhere else, because a link was swapped in after
// the checks, is refused; one where it was asked for is not.
func TestPlacedRefusesAWorktreeMadeElsewhere(t *testing.T) {
	ws := initRepo(t)
	r := New(context.Background(), ws)
	if err := os.MkdirAll(filepath.Join(ws, ".abhed-worktrees", "here"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.Placed(context.Background(), filepath.Join(ws, ".abhed-worktrees", "here")); err != nil {
		t.Fatalf("a worktree in place was refused: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(ws, ".abhed-worktrees", "swapped")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := r.Placed(context.Background(), filepath.Join(ws, ".abhed-worktrees", "swapped")); err == nil {
		t.Fatal("a worktree made through a link was accepted")
	}
}
