package sandbox

import (
	"os"
	"path/filepath"
	"strings"
)

// GitReach is what git needs to read outside workspace to work in it: the
// repository's top folder when workspace is a folder inside a repository, and
// the git folder and common git folder a work tree's .git file names (a linked
// worktree's, a submodule checkout's). Commands may read these, never write
// them: writes stay confined as before.
//
// A .git file in the workspace is the command's to rewrite, so the folder it
// names is read only when that folder names the work tree back (a linked
// worktree's gitdir file, a submodule's core.worktree), which nothing in the
// sandbox can write: a .git file pointed at another repository reaches nothing.
// A repository whose top folder is a home, or holds one (a home kept in git),
// is not reached: its git folder would show what the home holds.
func GitReach(workspace string) []string {
	ws := filepath.Clean(workspace)
	root := gitRoot(ws)
	if root == "" {
		return nil
	}
	var out []string
	add := func(p string) {
		if p == "" || insideAny(p, PathForms(ws)) {
			return
		}
		for _, have := range out {
			if have == p {
				return
			}
		}
		out = append(out, p)
	}
	if holdsAHome(root) {
		return nil
	}
	dotGit := filepath.Join(root, ".git")
	if root != ws {
		add(root)
	}
	info, err := os.Lstat(dotGit)
	if err != nil || !info.Mode().IsRegular() {
		return out
	}
	gd := gitFileTarget(root, dotGit)
	common, ok := ownGitFolder(root, gd)
	if !ok {
		return out
	}
	add(gd)
	add(common)
	return out
}

// ownGitFolder is the common git folder of gd, the git folder root's .git file
// names, when gd names root back as its work tree; ok is false otherwise.
func ownGitFolder(root, gd string) (common string, ok bool) {
	if gd == "" || !gitFolder(gd) && !worktreeFolder(gd) {
		return "", false
	}
	same := func(a, b string) bool { return a != "" && RealPath(filepath.Clean(a)) == RealPath(filepath.Clean(b)) }
	// A linked worktree's folder: its gitdir file names the work tree's .git,
	// and its commondir the repository's git folder.
	if back := readPointer(filepath.Join(gd, "gitdir")); back != "" && worktreeFolder(gd) {
		if !filepath.IsAbs(back) {
			back = filepath.Join(gd, back)
		}
		if !same(back, filepath.Join(root, ".git")) {
			return "", false
		}
		c, ok := readCommondir(filepath.Join(gd, "commondir"))
		if !ok || c == "" {
			return "", false
		}
		if !filepath.IsAbs(c) {
			c = filepath.Join(gd, c)
		}
		c = filepath.Clean(c)
		// git makes a worktree's folder in the common one's worktrees folder.
		if !gitFolder(c) || !same(filepath.Dir(filepath.Dir(gd)), c) || !strings.EqualFold(filepath.Base(filepath.Dir(gd)), "worktrees") {
			return "", false
		}
		return c, true
	}
	// A submodule's (or a separate git dir's) folder: its core.worktree names
	// the work tree.
	if gitFolder(gd) {
		wt := configWorktree(filepath.Join(gd, "config"))
		if wt == "" {
			return "", false
		}
		if !filepath.IsAbs(wt) {
			wt = filepath.Join(gd, wt)
		}
		if !same(wt, root) {
			return "", false
		}
		return gd, true
	}
	return "", false
}

// gitFolder reports whether d holds what every repository's git folder does.
func gitFolder(d string) bool {
	head, err := os.Lstat(filepath.Join(d, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	for _, sub := range []string{"objects", "refs"} {
		if info, err := os.Stat(filepath.Join(d, sub)); err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

// worktreeFolder reports whether d looks like a linked worktree's git folder.
func worktreeFolder(d string) bool {
	head, err := os.Lstat(filepath.Join(d, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	c, err := os.Lstat(filepath.Join(d, "commondir"))
	return err == nil && c.Mode().IsRegular()
}

// holdsAHome reports whether p is a home folder, holds one, or is the root.
func holdsAHome(p string) bool {
	if filepath.Dir(p) == p {
		return true
	}
	hs := homes()
	if h, err := os.UserHomeDir(); err == nil {
		hs = append(hs, h)
	}
	for _, h := range hs {
		for _, f := range PathForms(h) {
			for _, q := range PathForms(p) {
				if _, in := Within(f, q); in {
					return true
				}
			}
		}
	}
	return false
}
