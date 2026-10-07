package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// Bounds on the walk for git folders: a deeper tree, or one with more
// folders, is not walked further, and a repository past the bound is not
// protected. Each workspace's own git folder, its submodules' and its
// linked worktrees' are found apart from the walk, so no bound leaves them
// out. Variables, so a test can lower them.
var (
	gitWalkDepth   = 6
	gitWalkFolders = 20000
	// gitModulesDepth bounds how deep a submodule's git folder is looked
	// for under modules: names with slashes, and submodules within submodules.
	gitModulesDepth = 8
	// gitModulesEntries bounds one scan's modules folders; no real repository
	// nears it, so reaching it refuses rather than leave a submodule exposed.
	gitModulesEntries = 20000
)

// gitPointers are the names in a git folder that git reads as configuration,
// runs programs from, or follows to another folder: a commondir moves where
// the configuration and hooks are read from, and alternates where objects are.
var gitPointers = []string{"config", "config.worktree", "commondir", "gitdir", "hooks", "info/attributes", "objects/info/alternates"}

// gitScan is what one look for git folders found.
type gitScan struct {
	ws        string
	protected []string
	// gits are the .git entries found, which a later scan looks at again.
	gits []string
	// planted are commondir files git would not have written there.
	planted []string
	// linked are protected paths reached through a link, which no bind can hold.
	linked []string
	// walkBounded and modulesBounded are set when a bound stopped the look.
	walkBounded, modulesBounded bool
	folders, entries            int
	// make is whether a missing config and hooks are made empty for binding.
	make bool
	seen map[string]bool
}

// GitProtected are the paths in ws that git runs programs from or follows
// elsewhere: in each git folder, its submodules' and its linked worktrees',
// the gitPointers there, with config and hooks made empty where missing, and
// each .git file (a worktree's or a submodule's link to its git folder), at
// most gitWalkDepth folders down.
func GitProtected(ws string) []string { return scanGit(ws).protected }

// scanGit looks at ws's own git folder, then the .git entries known from
// earlier scans, then walks for others, each folder's .git before its contents.
func scanGit(ws string, known ...string) *gitScan {
	g := &gitScan{ws: ws, make: true, seen: map[string]bool{}}
	g.at(ws)
	for _, p := range known {
		// One whose folder became a link is passed over, as the walk would.
		if info, err := os.Lstat(p); err == nil && !linkBelow(ws, filepath.Dir(p)) {
			g.gitEntry(p, fs.FileInfoToDirEntry(info))
		}
	}
	_ = filepath.WalkDir(ws, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a folder that cannot be read is passed over, and the walk goes on
		}
		if p != ws && strings.EqualFold(d.Name(), ".git") {
			// Found by at on entering the folder above; another spelling is
			// a git folder of its own only where names have case.
			if d.Name() != ".git" && !sameEntry(p, filepath.Join(filepath.Dir(p), ".git")) {
				g.gitEntry(p, d)
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(ws, p)
		depth := strings.Count(rel, string(filepath.Separator))
		if p != ws && (depth >= gitWalkDepth || d.Name() == "node_modules" || strings.EqualFold(d.Name(), stateDir)) {
			return filepath.SkipDir
		}
		if g.folders++; g.folders > gitWalkFolders {
			g.walkBounded = true
			return filepath.SkipAll
		}
		if p != ws {
			g.at(p)
		}
		return nil
	})
	return g
}

// scanOwnGit looks only at ws's own git folder, its submodules' and its
// linked worktrees', making nothing: what seatbelt names beside its patterns.
func scanOwnGit(ws string) *gitScan {
	g := &gitScan{ws: ws, seen: map[string]bool{}}
	g.at(ws)
	return g
}

// at adds dir's .git, a git folder or a file naming one.
func (g *gitScan) at(dir string) {
	p := filepath.Join(dir, ".git")
	if info, err := os.Lstat(p); err == nil {
		g.gitEntry(p, fs.FileInfoToDirEntry(info))
	}
}

func (g *gitScan) gitEntry(p string, d fs.DirEntry) {
	switch {
	case d.IsDir():
		if !g.seen[p] {
			g.gits = append(g.gits, p)
		}
		g.folder(p, 0)
	case d.Type().IsRegular():
		if !g.seen[p] {
			g.gits = append(g.gits, p)
		}
		g.add(p)
	}
}

// linkBelow reports whether p or a folder between it and ws is a symbolic
// link, or cannot be looked at.
func linkBelow(ws, p string) bool {
	ws, p = filepath.Clean(ws), filepath.Clean(p)
	for ; len(p) > len(ws); p = filepath.Dir(p) {
		if info, err := os.Lstat(p); err != nil || info.Mode()&fs.ModeSymlink != 0 {
			return true
		}
	}
	return p != ws
}

func sameEntry(a, b string) bool {
	ia, err1 := os.Lstat(a)
	ib, err2 := os.Lstat(b)
	return err1 == nil && err2 == nil && os.SameFile(ia, ib)
}

func (g *gitScan) add(p string) {
	if !g.seen[p] {
		g.seen[p] = true
		g.protected = append(g.protected, p)
	}
}

// folder adds the git folder dir, a repository's or a submodule's, then its
// linked worktrees' and its submodules'. level counts submodules nested.
func (g *gitScan) folder(dir string, level int) {
	if g.seen[dir] {
		return
	}
	g.seen[dir] = true
	perWorktree := worktreeConfigOn(filepath.Join(dir, "config"))
	if g.make {
		// A missing config or hooks is made empty as the person, so it can
		// be bound read-only; so is config.worktree where git would read one.
		for _, f := range []string{"config", "hooks"} {
			if _, err := os.Lstat(filepath.Join(dir, f)); errors.Is(err, fs.ErrNotExist) {
				makeEmpty(filepath.Join(dir, f), f == "hooks")
			}
		}
		if perWorktree {
			makeEmpty(filepath.Join(dir, "config.worktree"), false)
		}
	}
	g.existing(dir, "config", "hooks", "config.worktree", "gitdir", "info/attributes", "objects/info/alternates")
	// Git writes a commondir only in a linked worktree's folder; one here
	// was put there, and no empty one can stand in for it, since git
	// refuses an empty commondir.
	if c := filepath.Join(dir, "commondir"); exists(c) {
		g.planted = append(g.planted, c)
	}
	// A submodule's work tree holds a .git file naming this folder; found
	// from the protected config, it is held wherever the walk stops.
	if level > 0 {
		if wt := configWorktree(filepath.Join(dir, "config")); wt != "" {
			if !filepath.IsAbs(wt) {
				wt = filepath.Join(dir, wt)
			}
			g.gitFile(filepath.Join(wt, ".git"))
		}
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "worktrees")); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			wt := filepath.Join(dir, "worktrees", e.Name())
			if g.make && perWorktree {
				makeEmpty(filepath.Join(wt, "config.worktree"), false)
			}
			g.existing(wt, "commondir", "gitdir", "config.worktree")
			// Git writes one here pointing back to dir; one pointing elsewhere
			// was changed before the first bind could hold it.
			if c := filepath.Join(wt, "commondir"); exists(c) && !pointsBack(c, wt, dir) {
				g.planted = append(g.planted, c)
			}
			if to := readPointer(filepath.Join(wt, "gitdir")); to != "" {
				if !filepath.IsAbs(to) {
					to = filepath.Join(wt, to)
				}
				g.gitFile(to)
			}
		}
	}
	if level < gitModulesDepth {
		g.modules(filepath.Join(dir, "modules"), level+1, 0)
	}
}

// gitFile adds p, a work tree's .git file, when it is one in the workspace.
func (g *gitScan) gitFile(p string) {
	p = filepath.Clean(p)
	forms := PathForms(g.ws)
	if !insideAny(p, forms) || !insideAny(RealPath(p), forms) {
		return
	}
	if info, err := os.Lstat(p); err == nil && info.Mode().IsRegular() {
		g.add(p)
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
		if g.entries++; g.entries > gitModulesEntries {
			g.modulesBounded = true
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

// existing adds the named paths in dir that exist, noting each reached
// through a link inside dir.
func (g *gitScan) existing(dir string, names ...string) {
	for _, n := range names {
		p := filepath.Join(dir, filepath.FromSlash(n))
		if !exists(p) {
			continue
		}
		g.add(p)
		for q := p; len(q) > len(dir); q = filepath.Dir(q) {
			if info, err := os.Lstat(q); err == nil && info.Mode()&fs.ModeSymlink != 0 {
				g.linked = append(g.linked, q)
				break
			}
		}
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// pointsBack reports whether the commondir c, in the linked worktree's folder
// wt, names dir with no link on the way, which a command could later repoint.
func pointsBack(c, wt, dir string) bool {
	if info, err := os.Lstat(c); err != nil || !info.Mode().IsRegular() {
		return false
	}
	to, ok := readCommondir(c)
	if !ok || to == "" {
		return false
	}
	raw := to
	if filepath.IsAbs(to) {
		if slices.Contains(strings.Split(filepath.ToSlash(to), "/"), "..") {
			return false
		}
	} else {
		// Only climbing out, as git's own "../.." does, through no link.
		for _, e := range strings.Split(filepath.ToSlash(to), "/") {
			if e != ".." && e != "" {
				return false
			}
		}
		if linkBelow(dir, wt) {
			return false
		}
		raw = wt + string(filepath.Separator) + to
	}
	return filepath.Clean(raw) == filepath.Clean(dir) && RealPath(raw) == RealPath(dir)
}

// readCommondir is the commondir file at p as git reads it: all of it, less
// trailing line ends; ok is false for one larger than any git writes.
func readCommondir(p string) (string, bool) {
	f, err := os.Open(p) // #nosec G304 -- a commondir in a git folder the walk found
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", false
	}
	return strings.TrimRight(string(b), "\r\n"), true
}

// readPointer is the first line of a small pointer file, or "".
func readPointer(p string) string {
	f, err := os.Open(p) // #nosec G304 -- a pointer file in a git folder the walk found
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	line, _ := bufio.NewReader(io.LimitReader(f, 4096)).ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

// configWorktree is core.worktree in the configuration at p, or "".
func configWorktree(p string) string {
	f, err := os.Open(p) // #nosec G304 -- a submodule's config the walk found
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	core := false
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			core = strings.EqualFold(strings.Trim(line, "[] \t"), "core")
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if core && ok && strings.EqualFold(strings.TrimSpace(k), "worktree") {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
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

// Events a backend that binds the git folders (bubblewrap, a container, the
// vm tier) records when the command's call carries a record.
const (
	// EvGitPlanted is a commondir taken out of a git folder, and the
	// command not run.
	EvGitPlanted = "sandbox.git_planted"
	// EvGitWalkBounded is the look for git folders stopping at a bound:
	// repositories past it are not protected, or the command is not run.
	EvGitWalkBounded = "sandbox.git_walk_bounded"
)

// gitMemory is what one sandbox keeps between scans: whether the walk's bound
// was recorded, and the .git entries found, so a later flood cannot hide them.
type gitMemory struct {
	noted atomic.Bool
	mu    sync.Mutex
	known map[string][]string // by workspace
}

// gitGuard returns the paths in ws to bind read-only before a command starts,
// or why the command is not run; a planted commondir is taken out first.
func gitGuard(ctx context.Context, ws, backend string, mem *gitMemory) ([]string, error) {
	mem.mu.Lock()
	known := mem.known[ws]
	mem.mu.Unlock()
	g := scanGit(ws, known...)
	mem.mu.Lock()
	if mem.known == nil {
		mem.known = map[string][]string{}
	}
	mem.known[ws] = g.gits
	mem.mu.Unlock()
	launch := LaunchOf(ctx)
	record := func(ev string, pay map[string]any) {
		if launch.Record != nil {
			pay["call_id"], pay["sandbox"], pay["workspace"] = launch.CallID, backend, ws
			_ = launch.Record(ev, pay)
		}
	}
	if g.walkBounded && mem.noted.CompareAndSwap(false, true) {
		record(EvGitWalkBounded, map[string]any{"bound": gitWalkFolders, "refused": false,
			"reason": fmt.Sprintf("the workspace has more than %d folders within %d levels; a repository past them, "+
				"other than the workspace's own, its submodules and linked worktrees and those found earlier, is not protected", gitWalkFolders, gitWalkDepth)})
	}
	if g.modulesBounded {
		why := fmt.Sprintf("sandbox: the command was not run: a git folder's modules hold more than %d entries, "+
			"so not every submodule's configuration and hooks could be found and protected; remove what does not belong there", gitModulesEntries)
		record(EvGitWalkBounded, map[string]any{"bound": gitModulesEntries, "refused": true, "reason": why})
		return nil, errors.New(why)
	}
	if entries, err := takeOutGitPlanted(g.planted); err != nil {
		record(EvGitPlanted, map[string]any{"entries": entries, "reason": err.Error()})
		return nil, err
	}
	if len(g.linked) > 0 {
		return nil, fmt.Errorf("sandbox: the command was not run: %s is a symbolic link, which this sandbox cannot hold read-only, "+
			"and a command could point it at configuration or hooks of its own; replace it with what it points to "+
			"(for hooks, a folder holding them)", strings.Join(g.linked, ", "))
	}
	return g.protected, nil
}

// takeOutGitPlanted quarantines or removes each planted commondir, which no
// bind could have stopped, and says what became of each; nil when none.
func takeOutGitPlanted(planted []string) ([]map[string]any, error) {
	if len(planted) == 0 {
		return nil, nil
	}
	var parts []string
	var entries []map[string]any
	for _, p := range planted {
		m := map[string]any{"path": p}
		if dest, err := quarantine(p, "commondir", "git-"); err == nil {
			m["outcome"], m["moved_to"] = plantMoved, dest
			parts = append(parts, p+" was moved to "+dest)
		} else if rerr := os.Remove(p); rerr == nil {
			m["outcome"] = plantRemoved
			parts = append(parts, p+" was removed")
		} else {
			m["outcome"] = plantRemaining
			parts = append(parts, p+" is still there and could not be taken out; remove it before Abhed runs there again")
		}
		entries = append(entries, m)
	}
	return entries, fmt.Errorf("sandbox: the command was not run: a commondir appeared where git would not write it "+
		"(a repository's or a submodule's git folder, or a linked worktree's pointing elsewhere), "+
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

// gitFoldersPattern matches the folders holding the gitPointers, not their
// contents: seatbelt checks a rename only at its ends, so one moved would escape.
func gitFoldersPattern(ws string) string {
	folders := []string{anyCase("modules"), anyCase("modules") + "/[^/]+", anyCase("worktrees"), anyCase("worktrees") + "/[^/]+",
		anyCase("info"), anyCase("objects"), anyCase("objects/info")}
	return fmt.Sprintf(`^%s/(.+/)?%s/((%s/[^/]+|%s/.+)/)?(%s)$`,
		regexQuote(ws), anyCase(".git"), anyCase("worktrees"), anyCase("modules"), strings.Join(folders, "|"))
}

// anyCaseRegex is the seatbelt regex for exactly the path p in ws, its
// part below ws in any case.
func anyCaseRegex(ws, p string) string {
	rel, err := filepath.Rel(ws, p)
	if err != nil {
		return "^" + regexQuote(p) + "$"
	}
	return "^" + regexQuote(ws) + "/" + anyCase(filepath.ToSlash(rel)) + "$"
}
