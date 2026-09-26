package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/hostgit"
)

// A memory file in the workspace is the agent's to change. A link planted as
// ABHED.md or ABHED.local.md, to Abhed's state or out of the workspace, puts
// nothing in the system prompt; an ordinary one still does.
func TestMemoryFilesAreReadAsTheFileToolsRead(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	users := filepath.Join(ws, ".abhed", "users.json")
	if err := os.WriteFile(users, []byte("secret-hash"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(outside, "id_rsa")
	if err := os.WriteFile(key, []byte("secret-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(users, filepath.Join(ws, "ABHED.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(key, filepath.Join(ws, "ABHED.local.md")); err != nil {
		t.Fatal(err)
	}
	prompt := BuildSystemPrompt(BuildOptions{Profile: "main", Workspace: ws, MemoryFiles: DiscoverMemoryFiles(ws)})
	if strings.Contains(prompt, "secret-hash") || strings.Contains(prompt, "secret-key") {
		t.Fatalf("a planted link put a file in the system prompt:\n%s", prompt)
	}
	for _, f := range []string{"ABHED.md", "ABHED.local.md"} {
		if data, err := ReadMemoryFile(ws, filepath.Join(ws, f)); err == nil {
			t.Errorf("/memory read %s through a link: %q", f, data)
		}
	}

	if err := os.Remove(filepath.Join(ws, "ABHED.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "ABHED.md"), []byte("use tabs"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt = BuildSystemPrompt(BuildOptions{Profile: "main", Workspace: ws, MemoryFiles: DiscoverMemoryFiles(ws)})
	if !strings.Contains(prompt, "use tabs") {
		t.Fatalf("an ordinary memory file was left out:\n%s", prompt)
	}
}

// The worktree exclude line is written as the file tools write: a planted
// info/exclude link into Abhed's state, or an info folder linked out of the
// workspace, is not written through.
func TestExcludeWorktreesWritesNoLink(t *testing.T) {
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
			excludeWorktrees(context.Background(), hostgit.New(context.Background(), ws))
			if after, _ := os.ReadFile(watched); string(after) != string(before) {
				t.Fatalf("the exclude line was written through the link: %q", after)
			}
		})
	}
}
