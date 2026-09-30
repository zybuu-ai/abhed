package local

import (
	"os"
	"path/filepath"
	"strings"
)

// RepoOf is the git repository dir belongs to, named by its common git
// directory, which every worktree of it shares; "" outside a repository.
// It reads the files git keeps rather than running git.
func RepoOf(dir string) string {
	gitDir := gitDirOf(dir)
	if gitDir == "" {
		return ""
	}
	common := gitDir
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil { // #nosec G304 -- git's own file
		c := strings.TrimSpace(string(data))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitDir, c)
		}
		common = c
	}
	return cleanDir(common)
}

// BranchOf is the branch checked out in dir's worktree, a short commit when
// none is, and "" outside a repository.
func BranchOf(dir string) string {
	gitDir := gitDirOf(dir)
	if gitDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD")) // #nosec G304 -- git's own file
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(data))
	if ref, ok := strings.CutPrefix(head, "ref: refs/heads/"); ok {
		return ref
	}
	if len(head) >= 7 && !strings.HasPrefix(head, "ref:") {
		return head[:7]
	}
	return ""
}

// gitDirOf finds the git directory for dir's worktree: a .git directory, or
// the one a .git file points to.
func gitDirOf(dir string) string {
	if dir == "" {
		return ""
	}
	d := cleanDir(dir)
	for {
		p := filepath.Join(d, ".git")
		if info, err := os.Stat(p); err == nil {
			if info.IsDir() {
				return p
			}
			data, err := os.ReadFile(p) // #nosec G304 -- git's own file
			if err != nil {
				return ""
			}
			g, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
			if !ok {
				return ""
			}
			g = strings.TrimSpace(g)
			if !filepath.IsAbs(g) {
				g = filepath.Join(d, g)
			}
			return filepath.Clean(g)
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}
