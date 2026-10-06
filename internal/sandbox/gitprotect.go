package sandbox

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Bounds on the walk for git folders: a deeper or wider tree is not walked
// further, and what lies past the bound is not protected.
const (
	gitWalkDepth   = 6
	gitWalkEntries = 20000
	// gitModulesDepth bounds how deep a submodule's git folder is looked
	// for under modules: names with slashes, and submodules within submodules.
	gitModulesDepth = 8
)

// gitPointers are the names in a git folder that git reads as configuration,
// runs programs from, or follows to another folder: a commondir moves where
// the configuration and hooks are read from, and alternates where objects are.
var gitPointers = []string{"config", "config.worktree", "commondir", "gitdir", "hooks", "info/attributes", "objects/info/alternates"}

// gitScan is what one walk for git folders found.
type gitScan struct {
	protected []string
	// planted are commondir files where git never writes one: in a
	// repository's own git folder, or a submodule's.
	planted []string
	seen    int
}

// GitProtected are the paths in ws that git runs programs from or follows
// elsewhere: in each git folder, its submodules' and its linked worktrees',
// the gitPointers there, with config and hooks made empty where missing, and
// each .git file (a worktree's or a submodule's link to its git folder), at
// most gitWalkDepth folders down.
func GitProtected(ws string) []string { return scanGit(ws).protected }

func scanGit(ws string) *gitScan {
	g := &gitScan{}
	_ = filepath.WalkDir(ws, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a folder that cannot be read is passed over, and the walk goes on
		}
		if g.seen++; g.seen > gitWalkEntries {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(ws, p)
		depth := strings.Count(rel, string(filepath.Separator))
		if strings.EqualFold(d.Name(), ".git") {
			if d.IsDir() {
				g.folder(p, 0)
				return filepath.SkipDir
			}
			if d.Type().IsRegular() {
				g.protected = append(g.protected, p)
			}
			return nil
		}
		if d.IsDir() && (depth >= gitWalkDepth || d.Name() == "node_modules" || strings.EqualFold(d.Name(), stateDir)) {
			return filepath.SkipDir
		}
		return nil
	})
	return g
}

// folder adds the git folder dir, a repository's or a submodule's, then its
// linked worktrees' and its submodules'. level counts submodules nested.
func (g *gitScan) folder(dir string, level int) {
	// A missing config or hooks is made empty as the person, so it can be
	// bound read-only; so is config.worktree where git would read one.
	for _, f := range []string{"config", "hooks"} {
		if _, err := os.Lstat(filepath.Join(dir, f)); errors.Is(err, fs.ErrNotExist) {
			makeEmpty(filepath.Join(dir, f), f == "hooks")
		}
	}
	perWorktree := worktreeConfigOn(filepath.Join(dir, "config"))
	if perWorktree {
		makeEmpty(filepath.Join(dir, "config.worktree"), false)
	}
	g.existing(dir, "config", "hooks", "config.worktree", "gitdir", "info/attributes", "objects/info/alternates")
	// Git writes a commondir only in a linked worktree's folder; one here
	// was put there, and no empty one can stand in for it, since git
	// refuses an empty commondir.
	if c := filepath.Join(dir, "commondir"); exists(c) {
		g.planted = append(g.planted, c)
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "worktrees")); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			wt := filepath.Join(dir, "worktrees", e.Name())
			if perWorktree {
				makeEmpty(filepath.Join(wt, "config.worktree"), false)
			}
			g.existing(wt, "commondir", "gitdir", "config.worktree")
		}
	}
	if level < gitModulesDepth {
		g.modules(filepath.Join(dir, "modules"), level+1, 0)
	}
}

// modules finds the submodules' git folders under dir: a folder holding a
// HEAD is one, and any other is part of a name with slashes.
func (g *gitScan) modules(dir string, level, depth int) {
	if depth >= gitModulesDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if g.seen++; g.seen > gitWalkEntries {
			return
		}
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		if exists(filepath.Join(sub, "HEAD")) {
			g.folder(sub, level)
		} else {
			g.modules(sub, level, depth+1)
		}
	}
}

// existing adds the named paths in dir that exist.
func (g *gitScan) existing(dir string, names ...string) {
	for _, n := range names {
		if p := filepath.Join(dir, filepath.FromSlash(n)); exists(p) {
			g.protected = append(g.protected, p)
		}
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// worktreeConfigOn reports whether the configuration at p may turn on
// extensions.worktreeConfig, under which git reads config.worktree. A
// mention anywhere counts: an empty config.worktree changes nothing.
func worktreeConfigOn(p string) bool {
	f, err := os.Open(p) // #nosec G304 -- a git folder's config the walk found
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(io.LimitReader(f, 1<<20))
	return strings.Contains(strings.ToLower(string(b)), "worktreeconfig")
}

// makeEmpty makes an empty folder or file at p, never replacing one.
func makeEmpty(p string, dir bool) {
	if dir {
		_ = os.Mkdir(p, 0o755) // #nosec G301 -- git's own mode for hooks
		return
	}
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil { // #nosec G302 G304 -- git's own mode for config, in a git folder the walk found
		_ = f.Close()
	}
}

// takeOutGitPlanted moves each planted commondir to the quarantine, or
// removes it, and says why the command is not run; nil when there was none.
// Bubblewrap and the container can bind only what exists, so a commondir a
// command makes is found here, before the next command runs.
func takeOutGitPlanted(planted []string) error {
	if len(planted) == 0 {
		return nil
	}
	var parts []string
	for _, p := range planted {
		if dest, err := quarantine(p, "commondir", "git-"); err == nil {
			parts = append(parts, p+" was moved to "+dest)
		} else if rerr := os.Remove(p); rerr == nil {
			parts = append(parts, p+" was removed")
		} else {
			parts = append(parts, p+" is still there and could not be taken out; remove it before Abhed runs there again")
		}
	}
	return fmt.Errorf("sandbox: the command was not run: a commondir appeared in a git folder, where git never writes one, "+
		"pointing git at configuration and hooks elsewhere; %s", strings.Join(parts, "; "))
}

// gitPattern is the seatbelt regex for the gitPointers of every git folder
// in ws at any depth, its linked worktrees' and its submodules', in any case.
func gitPattern(ws string) string {
	names := make([]string, len(gitPointers))
	for i, n := range gitPointers {
		names[i] = anyCase(n)
	}
	return fmt.Sprintf(`^%s/(.+/)?%s/((%s/[^/]+|%s/.+)/)?(%s)(/|$)`,
		regexQuote(ws), anyCase(".git"), anyCase("worktrees"), anyCase("modules"), strings.Join(names, "|"))
}
