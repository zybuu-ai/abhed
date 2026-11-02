package sandbox

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// tempOwnerFile names, inside a process sandbox's own caches or temp
// folder, the Abhed process that made it, as host:pid:start.
const tempOwnerFile = ".abhed-owner"

// reapedPrefixes are the folders a process sandbox makes in a temp area.
var reapedPrefixes = []string{"abhed-caches-", "abhed-tmp-", "abhed-dap-"}

// markOwned records this process as the owner of a folder it made.
func markOwned(dir string) {
	if v := ownerOf(os.Getpid()); v != "" {
		_ = os.WriteFile(filepath.Join(dir, tempOwnerFile), []byte(v), 0o400) // #nosec G306 -- read by this account's next Abhed
	}
}

var reapTempsOnce sync.Once

// reapParents are the temp areas a process sandbox makes its folders in;
// tests set it.
var reapParents = func() []string {
	parents := []string{os.TempDir()}
	if t := userTemp(); t != "" && t != parents[0] {
		parents = append(parents, t)
	}
	if d := debugRunDir(); d != "" {
		parents = append(parents, d)
	}
	return parents
}

// reapStaleTemps removes, once per process, the caches and temp folders a
// process sandbox of an Abhed process on this host left when it ended
// without closing: only Abhed's own names, and only those whose owner file
// proves the owner gone. One with no owner file, or an owner that cannot be
// read, is kept.
func reapStaleTemps() {
	reapTempsOnce.Do(func() { reapTemps(reapParents()) })
}

func reapTemps(parents []string) {
	for _, parent := range parents {
		entries, err := os.ReadDir(parent)
		if err != nil {
			continue
		}
		removed := 0
		for _, e := range entries {
			if !e.IsDir() || !hasReapedPrefix(e.Name()) {
				continue
			}
			dir := filepath.Join(parent, e.Name())
			if ownerOfTemp(dir) != ownerEnded {
				continue
			}
			makeRemovable(dir)
			if os.RemoveAll(dir) == nil {
				removed++
			}
		}
		if removed > 0 {
			slog.Debug("removed sandbox caches and temp folders left by Abhed processes that ended", "folder", parent, "count", removed)
		}
	}
}

func hasReapedPrefix(name string) bool {
	for _, p := range reapedPrefixes {
		if strings.HasPrefix(name, p) && len(name) > len(p) {
			return true
		}
	}
	return false
}

// ownerOfTemp reads a folder's owner file, a small regular file, never a link.
func ownerOfTemp(dir string) ownerState {
	f := filepath.Join(dir, tempOwnerFile)
	info, err := os.Lstat(f)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256 || info.Mode()&fs.ModeType != 0 {
		return ownerElsewhere
	}
	b, err := os.ReadFile(f) // #nosec G304 -- a small owner file in Abhed's own folder
	if err != nil {
		return ownerElsewhere
	}
	return ownerOfLabel(strings.TrimSpace(string(b)))
}

// debugRunDir is where a debug session's socket folders are made: in the
// home's .abhed, which every command's profile denies, so only the one
// command the folder is opened to (WithPrivateDirs) reaches it.
func debugRunDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, stateDir, "run")
}

// DebugSocketDir makes a folder for one debug session's socket, owned by
// this process for the reaper, under debugRunDir.
func DebugSocketDir() (string, error) {
	parent := debugRunDir()
	if parent == "" {
		return "", errors.New("sandbox: no home folder for a debug session's socket")
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(parent, "abhed-dap-")
	if err != nil {
		return "", err
	}
	markOwned(dir)
	return dir, nil
}
