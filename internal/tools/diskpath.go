package tools

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/zybuu-ai/abhed/internal/nlink"
)

// DiskPath spells each existing component of an absolute path as the disk holds it, found by
// identity as StateSet does, so VAULT on a disk that ignores case reads vault; the rest is kept.
func DiskPath(p string) string {
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) {
		return clean
	}
	vol := filepath.VolumeName(clean)
	sep := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(clean[len(vol):], sep), sep)
	cur := vol + sep
	for i, name := range parts {
		if name == "" {
			continue
		}
		disk, exists := diskName(cur, name)
		if !exists {
			return filepath.Join(append([]string{cur}, parts[i:]...)...)
		}
		cur = filepath.Join(cur, disk)
	}
	return cur
}

// diskName returns the name dir holds for the entry name opens, and whether there is one.
func diskName(dir, name string) (string, bool) {
	l := listDir(dir)
	key := foldKey(name)
	if l != nil && slices.Contains(l.folded[key], name) {
		return name, true
	}
	info, err := os.Lstat(filepath.Join(dir, name))
	if err != nil || l == nil {
		return name, err == nil // a folder that cannot be listed keeps the spelling given
	}
	// Equal but for case or Unicode form: a hard link under another name is never taken for it.
	if n, ok := sameAs(dir, l.folded[key], info); ok {
		return n, true
	}
	if len(l.names) <= maxListedNames {
		if n, ok := byIdentity(dir, l.names, info); ok {
			return n, true
		}
	}
	return name, true
}

// byIdentity finds a short alias's full name, only where no other name can be the same file.
// Windows reports no link count, so there a hard-linked file named by its 8.3 alias may read as a link's name.
func byIdentity(dir string, names []string, info os.FileInfo) (string, bool) {
	if !info.IsDir() && nlink.Of(info) != 1 {
		return "", false
	}
	return sameAs(dir, names, info)
}

// sameAs returns the first of names in dir that is the file info describes.
func sameAs(dir string, names []string, info os.FileInfo) (string, bool) {
	for _, n := range names {
		if got, err := os.Lstat(filepath.Join(dir, n)); err == nil && os.SameFile(got, info) {
			return n, true
		}
	}
	return "", false
}

// foldKey is a name as a disk that ignores case and Unicode form compares it: STRASSE is straße.
func foldKey(n string) string { return norm.NFC.String(cases.Fold().String(norm.NFC.String(n))) }

// A folder's listing is kept while its identity and times are unchanged. Where the disk keeps
// whole seconds, a folder changed in the last few could change again unseen, so it is not kept.
const (
	maxListedNames  = 20000
	maxHeldNames    = 200000
	listingSettleIn = 3 * time.Second
)

type dirListing struct {
	info   os.FileInfo
	ctime  int64
	names  []string
	folded map[string][]string // by foldKey
}

var listings struct {
	sync.Mutex
	dirs  map[string]*dirListing
	names int
}

// listDir returns dir's entries, or nil when it cannot be read.
func listDir(dir string) *dirListing {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}
	listings.Lock()
	l := listings.dirs[dir]
	listings.Unlock()
	if l != nil && os.SameFile(l.info, info) && l.info.ModTime().Equal(info.ModTime()) && l.ctime == changeTime(info) {
		return l
	}
	f, err := openDir(dir)
	if err != nil {
		return nil
	}
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return nil
	}
	sort.Strings(names)
	l = &dirListing{info: info, ctime: changeTime(info), names: names, folded: make(map[string][]string, len(names))}
	for _, n := range names {
		k := foldKey(n)
		l.folded[k] = append(l.folded[k], n)
	}
	coarse := info.ModTime().Nanosecond() == 0
	if len(names) <= maxListedNames && (!coarse || time.Since(info.ModTime()) > listingSettleIn) {
		listings.Lock()
		if old := listings.dirs[dir]; old != nil {
			listings.names -= len(old.names)
			delete(listings.dirs, dir)
		}
		if listings.dirs == nil || listings.names+len(names) > maxHeldNames {
			listings.dirs, listings.names = make(map[string]*dirListing), 0
		}
		listings.dirs[dir] = l
		listings.names += len(names)
		listings.Unlock()
	}
	return l
}
