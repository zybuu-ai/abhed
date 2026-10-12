package sandbox

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// rootHiddenEtc are files under /etc hidden from a command run as root
// whatever their mode: password hashes, host keys and sudo's rules.
var rootHiddenEtc = []string{"shadow", "shadow-", "gshadow", "gshadow-", "sudoers", "sudoers.d", "ssh/ssh_host_*_key", "ssh/ssh_host_key"}

// Bounds on the walk of /etc; past them a folder is hidden, which only narrows.
var (
	etcWalkDepth   = 8
	etcWalkEntries = 20000
)

// rootEtcGrants are the paths under etc a command run as root may read: what
// any user may read, less rootHiddenEtc. A folder holding nothing hidden is
// granted whole, one holding some entry by entry; links are judged at their targets.
func rootEtcGrants(etc string) []string {
	seen := 0
	var walk func(dir, rel string, depth int) ([]string, bool)
	walk = func(dir, rel string, depth int) ([]string, bool) {
		if depth > etcWalkDepth {
			return nil, false
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, false
		}
		whole := true
		var grants []string
		for _, e := range entries {
			if seen++; seen > etcWalkEntries {
				return grants, false
			}
			p, r := filepath.Join(dir, e.Name()), path.Join(rel, e.Name())
			info, err := os.Lstat(p)
			switch {
			case err != nil:
				whole = false
			case info.Mode()&fs.ModeSymlink != 0:
			case etcHidden(r, info):
				whole = false
			case info.IsDir():
				sub, all := walk(p, r, depth+1)
				if all {
					grants = append(grants, p)
				} else {
					whole = false
					grants = append(grants, sub...)
				}
			case info.Mode().IsRegular():
				grants = append(grants, p)
			default:
				whole = false
			}
		}
		return grants, whole
	}
	grants, whole := walk(etc, "", 0)
	if whole {
		return []string{etc}
	}
	return grants
}

// etcHidden reports whether the entry at rel under /etc is kept from root:
// named in rootHiddenEtc, or not readable (a folder: not searchable) by every user.
func etcHidden(rel string, info fs.FileInfo) bool {
	for _, pat := range rootHiddenEtc {
		if ok, _ := path.Match(pat, rel); ok {
			return true
		}
	}
	if info.IsDir() {
		return info.Mode().Perm()&0o005 != 0o005
	}
	return info.Mode().Perm()&0o004 == 0
}
