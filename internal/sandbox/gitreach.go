package sandbox

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// GitReach is what git needs to read outside workspace to work in it: the
// repository's top folder when workspace is a folder inside a repository, and
// the repository's git folders outside the workspace (its top's .git, or the
// git folder and common git folder a linked worktree's or submodule
// checkout's .git file names). See gitReachOf.
func GitReach(workspace string) []string {
	r := gitReachOf(workspace)
	if r.top == "" {
		return r.dirs
	}
	return append([]string{r.top}, r.dirs...)
}

// gitReach is GitReach in parts: top and the git folders dirs, read only,
// and own, the one git folder whose work tree is exactly the workspace (a
// submodule checkout's), which commands may write as git does under
// protect_git. A git folder shared with another work tree (a linked
// worktree's common folder, or a repository's .git for a folder inside it,
// whose index covers files outside the workspace) is never written: through
// it a command could move another work tree's branch, index or HEAD, plant
// replace refs or grafts, or stage files outside the workspace.
type gitReach struct {
	top  string
	dirs []string
	own  string
	// shallow is whether own held a shallow file as the session started,
	// which bubblewrap cannot hold when there was none (see outsideGitHeld).
	shallow bool
	// root is the repository's top folder for a workspace inside it, whose
	// own listing git needs (it changes into it) even where top is not read.
	root string
}

// gitReachOf finds a workspace's git reach.
//
// A .git file in the workspace is the command's to rewrite, so the folder it
// names is reached only when that folder names the work tree back (a linked
// worktree's gitdir file, a submodule's core.worktree), which nothing in the
// sandbox can write: a .git file pointed at another repository reaches nothing.
// A repository whose top folder is a home, or holds one (a home kept in git),
// is not reached: its git folder would show what the home holds. A top that
// holds another .abhed than the workspace's own (another workspace's state),
// or more folders than can be looked through, is not read: only its .git.
func gitReachOf(workspace string) gitReach {
	ws := filepath.Clean(workspace)
	root := gitRoot(ws)
	if root == "" || holdsAHome(root) {
		return gitReach{}
	}
	var r gitReach
	add := func(p string) {
		if p == "" || insideAny(p, PathForms(ws)) || slices.Contains(r.dirs, p) {
			return
		}
		r.dirs = append(r.dirs, p)
	}
	dotGit := filepath.Join(root, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		return gitReach{}
	}
	if root != ws {
		r.root = root
		if !holdsOtherState(root, ws) {
			r.top = root
		}
		if info.IsDir() && gitFolder(dotGit) {
			add(dotGit)
		}
	}
	if !info.Mode().IsRegular() {
		return r
	}
	gd := gitFileTarget(root, dotGit)
	common, ok := ownGitFolder(root, gd)
	if !ok {
		return r
	}
	add(gd)
	add(common)
	// A submodule's git folder serves this work tree alone.
	if root == ws && gd == common && gitFolder(gd) && !insideAny(gd, PathForms(ws)) {
		r.own = gd
		r.shallow = exists(filepath.Join(gd, "shallow"))
	}
	return r
}

// stateScanFolders bounds holdsOtherState's walk.
const stateScanFolders = 50_000

// holdsOtherState reports whether top holds a state folder (.abhed, in any
// case) outside ws, or more folders than the walk looks through.
func holdsOtherState(top, ws string) bool {
	seen, found := 0, false
	_ = filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != top {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if seen++; seen > stateScanFolders {
			found = true
			return fs.SkipAll
		}
		if p != top && strings.EqualFold(d.Name(), stateDir) && !insideAny(p, PathForms(ws)) {
			found = true
			return fs.SkipAll
		}
		// A git folder holds no work tree; the workspace's own state is denied with it.
		if p != top && (strings.EqualFold(d.Name(), ".git") || insideAny(p, PathForms(ws))) {
			return fs.SkipDir
		}
		return nil
	})
	return found
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

// stateAnywherePattern is the seatbelt regex for a .abhed at any depth under
// r, in any case.
func stateAnywherePattern(r string) string {
	return fmt.Sprintf(`^%s/(.+/)?%s(/|$)`, regexQuote(r), anyCase(stateDir))
}

// gitFolderPattern is gitPattern for g, a git folder named directly: its
// gitPointers, its linked worktrees' and its submodules', in any case.
func gitFolderPattern(g string) string {
	names := make([]string, len(gitPointers))
	for i, n := range gitPointers {
		names[i] = anyCase(n)
	}
	return fmt.Sprintf(`^%s/((%s/[^/]+|%s/.+)/)?(%s)(/|$)`,
		regexQuote(g), anyCase("worktrees"), anyCase("modules"), strings.Join(names, "|"))
}

// ownGitExtras are what else in a written git folder git reads as history or
// as where to fetch from, held as the gitPointers are: grafts, a shallow
// list, sparse-checkout rules, dumb-http alternates and replace refs.
var ownGitExtras = []string{"info/grafts", "info/sparse-checkout", "objects/info/http-alternates", "shallow", "refs/replace"}

// gitExtrasPattern is the seatbelt regex for ownGitExtras in g and in the
// git folders of its submodules, in any case.
func gitExtrasPattern(g string) string {
	names := make([]string, len(ownGitExtras))
	for i, n := range ownGitExtras {
		names[i] = anyCase(n)
	}
	return fmt.Sprintf(`^%s/(%s/.+/)?(%s)(/|$)`, regexQuote(g), anyCase("modules"), strings.Join(names, "|"))
}

// gitFolderFoldersPattern is gitFoldersPattern for g, a git folder named directly.
func gitFolderFoldersPattern(g string) string {
	folders := []string{anyCase("modules"), anyCase("modules") + "/[^/]+", anyCase("worktrees"), anyCase("worktrees") + "/[^/]+",
		anyCase("info"), anyCase("objects"), anyCase("objects/info")}
	return fmt.Sprintf(`^%s/((%s/[^/]+|%s/.+)/)?(%s)$`,
		regexQuote(g), anyCase("worktrees"), anyCase("modules"), strings.Join(folders, "|"))
}

// outsideGitHeld are the files in own, the submodule's git folder outside
// the workspace ws, that bubblewrap binds read-only when it lets commands
// write it: what git reads as configuration or follows elsewhere (config and
// hooks made empty where missing), and ownGitExtras (refs/replace made an
// empty folder, grafts, sparse-checkout and http-alternates empty files where
// missing). planted are a commondir git would not have written, and a shallow
// list that was not there as the session started (shallow, as the empty one
// would make git take the repository as shallow), to be taken out. ok is
// false, and the folder stays read-only, when one cannot be held: a link, or
// more submodules than are looked through.
func outsideGitHeld(ws, own string, shallow bool) (held, planted []string, ok bool) {
	g := &gitScan{ws: ws, seen: map[string]bool{}, make: true}
	g.folder(own, 0)
	if len(g.linked) > 0 || g.modulesBounded {
		return nil, g.planted, false
	}
	var shallowPlanted []string
	defer func() { planted = append(planted, shallowPlanted...) }()
	for _, n := range ownGitExtras {
		p := filepath.Join(own, filepath.FromSlash(n))
		switch {
		case n == "shallow":
			if !shallow && exists(p) {
				shallowPlanted = append(shallowPlanted, p)
			} else if exists(p) {
				g.add(p)
			}
			continue
		case n == "refs/replace":
			makeEmpty(p, true)
		default:
			_ = os.MkdirAll(filepath.Dir(p), 0o755) // #nosec G301 -- git's own mode for its folders
			makeEmpty(p, false)
		}
		if isLink(p) {
			return nil, g.planted, false
		}
		g.add(p)
	}
	return g.protected, g.planted, true
}
