package app

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// The editor's own files are matched in any case, as APFS and NTFS open
// them, and through links the agent can make in the workspace.
func TestEditorFileByCaseAndLink(t *testing.T) {
	ws := t.TempDir() // on macOS a path through the /var link
	real, _ := filepath.EvalSymlinks(ws)
	for _, d := range []string{".vscode", ".git/hooks", ".git2/hooks", "sub"} {
		if err := os.MkdirAll(filepath.Join(real, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{"cfg": ".vscode", "g": ".git", "dc": ".devcontainer"} {
		if err := os.Symlink(target, filepath.Join(real, link)); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]bool{
		".vscode/settings.json":       true,
		".VSCode/launch.json":         true,
		".DevContainer/x.json":        true,
		".GIT/config":                 true,
		".Git/HOOKS/pre-commit":       true,
		".git":                        true,
		"cfg/tasks.json":              true,
		"g/hooks/pre-commit":          true,
		"g/config":                    true,
		"dc/devcontainer.json":        true, // a link to a folder not made yet
		"team.Code-Workspace":         true,
		"sub/x.CODE-WORKSPACE":        true,
		"src/main.go":                 false,
		".github/workflows/ci.yml":    false,
		".gitignore":                  false,
		".git/HEAD":                   false,
		".git2/hooks/pre-push":        false, // no .git file names it
		"sub/.vscode-notes/readme.md": false,
	} {
		for _, root := range []string{ws, real} {
			for _, base := range []string{ws, real} {
				p := filepath.Join(base, filepath.FromSlash(path))
				if got := editorFile(p, []string{root}); got != want {
					t.Errorf("editorFile(%s, root %s) = %v, want %v", p, root, got, want)
				}
			}
		}
	}
}

// A root spelled in another case is the same folder where the disk ignores case.
func TestEditorFileUnderARootInAnotherCase(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("names differing in case are different folders here")
	}
	real, _ := filepath.EvalSymlinks(t.TempDir())
	parent := filepath.Dir(real)
	other := filepath.Join(filepath.Dir(parent), strings.ToUpper(filepath.Base(parent)), filepath.Base(real))
	if other == real {
		t.Skip("no letters to change")
	}
	if !editorFile(filepath.Join(real, ".vscode", "tasks.json"), []string{other}) {
		t.Fatal("a root in another case let .vscode through")
	}
}

// A .git file points at the git folder that matters; its config and hooks
// are the editor's, for the agent's tools and for the sandbox.
func TestEditorFileFollowsAGitFile(t *testing.T) {
	ws := t.TempDir()
	real, _ := filepath.EvalSymlinks(ws)
	common := filepath.Join(real, "main.git")
	wt := filepath.Join(common, "worktrees", "w")
	if err := os.MkdirAll(filepath.Join(wt, "hooks"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, ".git"), []byte("gitdir: main.git/worktrees/w\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		"main.git/worktrees/w/hooks/pre-commit": true,
		"main.git/worktrees/w/config":           true,
		"main.git/config":                       true,
		"main.git/HOOKS/post-checkout":          true,
		"main.git/worktrees/w":                  true,
		"main.git/description":                  false,
	} {
		if got := editorFile(filepath.Join(ws, path), []string{ws}); got != want {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
	got := protectedPaths(ws)
	for _, want := range []string{filepath.Join(wt, "hooks"), filepath.Join(wt, "config"), filepath.Join(common, "hooks"), filepath.Join(common, "config"), filepath.Join(ws, ".git", "hooks"), filepath.Join(real, ".vscode")} {
		if !slices.Contains(got, want) {
			t.Errorf("protectedPaths lacks %s: %v", want, got)
		}
	}
}

// The sandbox is given the editor's files in both forms of the workspace,
// and *.code-workspace files in any case.
func TestProtectedPathsBothForms(t *testing.T) {
	ws := t.TempDir()
	real, _ := filepath.EvalSymlinks(ws)
	if err := os.WriteFile(filepath.Join(real, "Team.Code-Workspace"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := protectedPaths(ws)
	for _, want := range []string{filepath.Join(ws, ".git", "hooks"), filepath.Join(real, ".git", "hooks"), filepath.Join(real, "Team.Code-Workspace")} {
		if !slices.Contains(got, want) {
			t.Errorf("protectedPaths lacks %s: %v", want, got)
		}
	}
}

// An unsaved buffer guards the file however the agent spells its name, where
// the filesystem ignores case.
func TestDirtyBufferIgnoresCaseWhereTheDiskDoes(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("names differing in case are different files here")
	}
	ws := t.TempDir()
	draft := filepath.Join(ws, "Draft.md")
	if err := os.WriteFile(draft, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &acpSession{cwd: ws, dirty: map[string]bool{bufferKey(tools.RealPath(draft)): true}}
	if err := s.dirtyGuard(filepath.Join(ws, "draft.md"), nil); !errors.Is(err, errDirtyBuffer) {
		t.Fatalf("draft.md past an unsaved Draft.md: %v", err)
	}
	if err := s.dirtyGuard(filepath.Join(ws, "other.md"), nil); err != nil {
		t.Fatalf("another file: %v", err)
	}
}

// The guard holds in the roots the tool session gives it, such as a
// subagent's worktree, as well as the Studio session's own.
func TestEditorGuardHoldsInTheToolSessionsRoots(t *testing.T) {
	ws, wt := t.TempDir(), t.TempDir()
	s := &acpSession{cwd: ws}
	if err := s.dirtyGuard(filepath.Join(wt, ".vscode", "tasks.json"), []string{wt}); !errors.Is(err, errEditorFile) {
		t.Fatalf("a worktree's .vscode: %v", err)
	}
}
