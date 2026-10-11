package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Bounds on what one fingerprint reads, so a flooded hooks folder cannot stall an ask.
const (
	printFileBytes  = 1 << 20
	printDirEntries = 512
	printDepth      = 3
)

// GitFingerprint sums what git reads as configuration or runs programs from
// for the repository holding each of dirs: the git folder's config, hooks,
// commondir and attributes (a linked worktree's and its common folder's too),
// the .git entry itself, and the person's global git configuration. It
// changes whenever any of them does, so an "always allow" for a git command
// granted before a hook or fsmonitor was planted can be asked about again.
func GitFingerprint(dirs ...string) string {
	h := sha256.New()
	seen := map[string]bool{}
	for _, d := range dirs {
		root := gitRoot(d)
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		fmt.Fprintf(h, "repo %s\n", root)
		dotGit := filepath.Join(root, ".git")
		// The entry alone: a folder's own parts are summed below, never its objects.
		printPath(h, dotGit, printDepth)
		gd := dotGit
		if info, err := os.Lstat(dotGit); err == nil && info.Mode().IsRegular() {
			if to := gitFileTarget(root, dotGit); to != "" {
				gd = to
			}
		}
		printGitFolder(h, gd)
		// A linked worktree's folder names the common one, whose config and hooks git reads.
		if c, ok := readCommondir(filepath.Join(gd, "commondir")); ok && c != "" {
			if !filepath.IsAbs(c) {
				c = filepath.Join(gd, c)
			}
			printGitFolder(h, filepath.Clean(c))
		}
	}
	for _, p := range globalGitConfigs() {
		printPath(h, p, printDepth)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// printGitFolder sums the gitPointers of the git folder gd.
func printGitFolder(h hash.Hash, gd string) {
	fmt.Fprintf(h, "gitdir %s\n", gd)
	for _, n := range gitPointers {
		printPath(h, filepath.Join(gd, filepath.FromSlash(n)), 0)
	}
}

// printPath writes p's kind and contents to h: a link's target, a file's
// bytes (to a bound), a folder's entries in order, to a depth.
func printPath(h hash.Hash, p string, depth int) {
	info, err := os.Lstat(p)
	if err != nil {
		fmt.Fprintf(h, "%s absent\n", p)
		return
	}
	fmt.Fprintf(h, "%s %v\n", p, info.Mode())
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		to, _ := os.Readlink(p)
		fmt.Fprintf(h, "-> %s\n", to)
	case info.Mode().IsRegular():
		fmt.Fprintf(h, "size %d\n", info.Size())
		f, err := os.Open(p) // #nosec G304 -- a git folder part or git's global configuration
		if err != nil {
			fmt.Fprintf(h, "unreadable %v\n", info.ModTime().UnixNano())
			return
		}
		_, _ = io.Copy(h, io.LimitReader(f, printFileBytes))
		_ = f.Close()
		if info.Size() > printFileBytes {
			fmt.Fprintf(h, "mtime %d\n", info.ModTime().UnixNano())
		}
	case info.IsDir() && depth < printDepth:
		entries, err := os.ReadDir(p)
		if err != nil {
			fmt.Fprintf(h, "unlistable\n")
			return
		}
		names := make([]string, 0, len(entries))
		for i, e := range entries {
			if i >= printDirEntries {
				fmt.Fprintf(h, "more %d\n", len(entries))
				break
			}
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, n := range names {
			printPath(h, filepath.Join(p, n), depth+1)
		}
	}
}

// gitRoot is the nearest folder at or above dir that holds a .git, or "".
func gitRoot(dir string) string {
	if dir == "" {
		return ""
	}
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// gitFileTarget is the git folder a work tree's .git file names, or "".
func gitFileTarget(root, f string) string {
	line := readPointer(f)
	to, ok := strings.CutPrefix(line, "gitdir: ")
	if !ok || to == "" {
		return ""
	}
	if !filepath.IsAbs(to) {
		to = filepath.Join(root, to)
	}
	return filepath.Clean(to)
}

// globalGitConfigs are the person's own git configuration files, which git
// reads for every repository.
func globalGitConfigs() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".gitconfig"))
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" {
			xdg = filepath.Join(home, ".config")
		}
		out = append(out, filepath.Join(xdg, "git", "config"), filepath.Join(xdg, "git", "attributes"))
	}
	return out
}
