package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// maxStateEntries bounds how many files and folders of the state directories
// are remembered for the identity checks, so a large directory cannot slow
// every call. The known files (users, config, secrets and registered paths)
// are gathered first and always count; past the bound, a hardlink to a
// further file in a state directory is not recognised.
const maxStateEntries = 4096

// knownStateFiles are the files a state directory is known to hold.
var knownStateFiles = []string{"users.json", "config.json", "secrets.json"}

// ErrState is the refusal for a path that turned out, once opened, to be
// Abhed's own state.
var ErrState = errors.New("this is Abhed's own state")

var extraState struct {
	mu    sync.Mutex
	paths []string
}

// AddStatePath registers a file or directory that holds Abhed's state outside
// any .abhed directory, such as a configured users or secrets file, so every
// check refuses it as it refuses .abhed. Called by the operator's entry points.
func AddStatePath(path string) {
	if path == "" {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	extraState.mu.Lock()
	defer extraState.mu.Unlock()
	for _, p := range extraState.paths {
		if p == abs {
			return
		}
	}
	extraState.paths = append(extraState.paths, abs)
}

func registeredState() []string {
	extraState.mu.Lock()
	defer extraState.mu.Unlock()
	return append([]string(nil), extraState.paths...)
}

// StateSet is Abhed's own state as it exists on disk: the .abhed directory
// under each root and in the home directory, the files they hold, and any
// registered state path.
//
// It is a boundary and not a rule: a rule can be edited away by whoever can
// write the configuration, and this is what stops the agent being that
// whoever. It is built once per check or walk, so what it read stays current.
type StateSet struct {
	dirs  []os.FileInfo
	files []os.FileInfo
	named []string // registered paths, lexical and resolved
}

// NewStateSet gathers the state under the given roots, in the home directory
// and at every registered path.
func NewStateSet(roots ...string) *StateSet {
	set := &StateSet{}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, home)
	}
	var dirs []string
	for _, r := range roots {
		if r == "" {
			continue
		}
		d := filepath.Join(r, StateDir)
		if set.addDir(d) {
			dirs = append(dirs, d)
			for _, name := range knownStateFiles {
				set.addFile(filepath.Join(d, name))
			}
		}
	}
	for _, p := range registeredState() {
		set.named = append(set.named, p)
		if real, err := filepath.EvalSymlinks(p); err == nil {
			set.named = append(set.named, real)
		}
		if set.addDir(p) {
			dirs = append(dirs, p)
		} else {
			set.addFile(p)
		}
	}
	for _, d := range dirs {
		set.walk(d)
	}
	return set
}

// addDir records dir once, however many roots name it, and reports whether
// it was a directory seen for the first time.
func (set *StateSet) addDir(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || set.seen(set.dirs, info) {
		return false
	}
	set.dirs = append(set.dirs, info)
	return true
}

func (set *StateSet) addFile(path string) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || set.seen(set.files, info) {
		return
	}
	set.files = append(set.files, info)
}

// walk records a state directory's folders and files, up to the bound. The
// worktrees the parallel runs keep there are copies of the workspace, not
// state, and are passed over.
func (set *StateSet) walk(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is not state we can name
		}
		if len(set.dirs)+len(set.files) >= maxStateEntries {
			return filepath.SkipAll
		}
		info := entryInfo(d)
		switch {
		case info == nil || path == dir:
		case d.IsDir() && d.Name() == "worktrees":
			return filepath.SkipDir
		case d.IsDir():
			if !set.seen(set.dirs, info) {
				set.dirs = append(set.dirs, info)
			}
		case d.Type().IsRegular():
			if !set.seen(set.files, info) {
				set.files = append(set.files, info)
			}
		}
		return nil
	})
}

// entryInfo returns an entry's info, or nil when it cannot be read: such
// an entry is not state we can name, so the walk passes over it.
func entryInfo(d fs.DirEntry) os.FileInfo {
	info, err := d.Info()
	if err != nil {
		return nil
	}
	return info
}

func (set *StateSet) seen(pool []os.FileInfo, info os.FileInfo) bool {
	for _, s := range pool {
		if os.SameFile(s, info) {
			return true
		}
	}
	return false
}

// Has reports whether a path names Abhed's state by any spelling. Both the
// path as given and the path with every symlink followed are judged: by name
// without case (a case-insensitive disk opens .ABHED as .abhed), and by asking
// the filesystem whether it, or a folder above it, is state (a symlink, a
// hardlink to a state file, or a name the disk folds or normalises).
func (set *StateSet) Has(path string) bool {
	clean := filepath.Clean(path)
	real := RealPath(clean)
	for _, p := range []string{clean, real} {
		if hasStateName(p) || set.isNamed(p) || set.onDisk(p) {
			return true
		}
	}
	return false
}

// HasEntry reports whether an entry met while walking is state: a folder
// named or linked to one, or a file that is a state file under another name.
func (set *StateSet) HasEntry(d fs.DirEntry) bool {
	if strings.EqualFold(d.Name(), StateDir) {
		return true
	}
	info, err := d.Info()
	if err != nil {
		return false
	}
	return set.same(info)
}

func (set *StateSet) isNamed(p string) bool {
	for _, n := range set.named {
		if strings.EqualFold(p, n) || within(p, []string{n}) {
			return true
		}
	}
	return false
}

func (set *StateSet) onDisk(p string) bool {
	if len(set.dirs) == 0 && len(set.files) == 0 {
		return false
	}
	for ; ; p = filepath.Dir(p) {
		if info, err := os.Stat(p); err == nil && set.same(info) {
			return true
		}
		if filepath.Dir(p) == p {
			return false
		}
	}
}

func (set *StateSet) same(info os.FileInfo) bool {
	if info.IsDir() {
		return set.seen(set.dirs, info)
	}
	return set.seen(set.files, info)
}

// HasFile reports whether an opened file or folder is state, by identity:
// what a path check saw can be swapped before the open, what was opened
// cannot.
func (set *StateSet) HasFile(info os.FileInfo) bool { return set.same(info) }

// IsState reports whether a path names Abhed's state, as seen from the given
// workspace roots. It is the one check every reader of a path shares: the
// agent's tools, and the server's download, viewer and search.
func IsState(path string, roots ...string) bool {
	return NewStateSet(roots...).Has(path)
}

// hasStateName reports whether a cleaned path has StateDir as a component,
// compared without case.
func hasStateName(clean string) bool {
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if strings.EqualFold(part, StateDir) {
			return true
		}
	}
	return false
}

// RealPath follows symlinks in the part of p that exists, so a path to a file
// not yet created still lands under its real parent. A link whose target does
// not exist yet is followed too: writing through it would create the target.
func RealPath(p string) string {
	return realPath(p, 0)
}

func realPath(p string, hops int) string {
	rest := ""
	for cur := p; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
		if info, err := os.Lstat(cur); err == nil && info.Mode()&fs.ModeSymlink != 0 && hops < 40 {
			if target, err := os.Readlink(cur); err == nil {
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(cur), target)
				}
				return realPath(filepath.Join(target, rest), hops+1)
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
