package mountns

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// mountEntry is one /proc/self/mountinfo line: mount id, device, the folder of
// the filesystem it shows (root) and where it is mounted (point).
type mountEntry struct {
	ID    int
	Dev   string
	Root  string
	Point string
	// Type, Source and Super follow the separator: the filesystem's type,
	// what it was mounted from, and its own options.
	Type, Source, Super string
}

// parseMountinfo reads /proc/self/mountinfo's lines.
func parseMountinfo(data string) ([]mountEntry, error) {
	var out []mountEntry
	for line := range strings.SplitSeq(data, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 5 {
			return nil, fmt.Errorf("a mountinfo line has %d fields: %q", len(f), line)
		}
		id, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, fmt.Errorf("a mountinfo line's id: %q", line)
		}
		e := mountEntry{ID: id, Dev: f[2], Root: unescapeMountinfo(f[3]), Point: unescapeMountinfo(f[4])}
		if i := slices.Index(f, "-"); i > 5 && i+2 < len(f) {
			e.Type, e.Source = f[i+1], unescapeMountinfo(f[i+2])
			if i+3 < len(f) {
				e.Super = unescapeMountinfo(f[i+3])
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// unescapeMountinfo undoes the kernel's octal escapes (\040 for a space).
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// within is the rest of child below parent, both clean absolute paths, and
// whether child is parent or below it.
func within(parent, child string) (string, bool) {
	switch {
	case child == parent:
		return "", true
	case parent == "/":
		return strings.TrimPrefix(child, "/"), strings.HasPrefix(child, "/")
	case strings.HasPrefix(child, parent+"/"):
		return child[len(parent)+1:], true
	}
	return "", false
}

// deletedRoot is whether a mount's root was unlinked since it was mounted.
func deletedRoot(root string) bool {
	return strings.HasSuffix(root, "//deleted") || strings.HasSuffix(root, " (deleted)")
}

// alias is another mount reaching the workspace's files at Path. Rel is "" when
// Path is the workspace itself, else the folder Path alone shows.
type alias struct {
	Path string
	Rel  string
}

// errAlias is a mount of the workspace's files the fence cannot cover.
var errAlias = errors.New("mountns: the workspace is also reachable elsewhere")

// aliases finds every mount but wsMount of its filesystem showing the workspace
// at wsPath or part of it; those whose root was unlinked come apart (deleted).
func aliases(entries []mountEntry, wsMount int, wsPath string) (found []alias, deleted []string, err error) {
	var ws *mountEntry
	for i := range entries {
		if entries[i].ID == wsMount {
			ws = &entries[i]
		}
	}
	if ws == nil {
		return nil, nil, fmt.Errorf("%w: the workspace's mount %d is not in mountinfo", errAlias, wsMount)
	}
	rest, ok := within(ws.Point, wsPath)
	if !ok || deletedRoot(ws.Root) {
		return nil, nil, fmt.Errorf("%w: %s is not under its mount at %s", errAlias, wsPath, ws.Point)
	}
	fsPath := path.Join(ws.Root, rest)
	for _, e := range entries {
		if e.ID == wsMount || e.Dev != ws.Dev {
			continue
		}
		if deletedRoot(e.Root) {
			deleted = append(deleted, e.Point)
			continue
		}
		var a alias
		if r, ok := within(e.Root, fsPath); ok {
			a = alias{Path: path.Join(e.Point, r)}
		} else if r, ok := within(fsPath, e.Root); ok {
			a = alias{Path: e.Point, Rel: r}
		} else {
			continue
		}
		// Two mounts can give one path, as on ostree, where /var is mounted
		// again over /sysroot's view of it; the path is covered once.
		if !slices.Contains(found, a) {
			found = append(found, a)
		}
	}
	return found, deleted, nil
}

// What covers a whole alias that shows only part of the workspace.
const (
	wholeReadOnly = "read_only"
	wholeEmpty    = "empty"
)

// rebase is p's part inside workspace folder rel, relative to rel; if rel is in
// a folder p hides or holds read-only, all of it is covered (whole).
func rebase(p Plan, rel string) (sub Plan, whole string) {
	inside := func(q string) (string, bool) { return within("/"+rel, "/"+q) }
	covers := func(q string) bool { _, ok := within("/"+q, "/"+rel); return ok }
	for _, q := range p.Empty {
		if covers(q) {
			return Plan{}, wholeEmpty
		}
	}
	for _, q := range p.ReadOnly {
		if covers(q) {
			return Plan{}, wholeReadOnly
		}
	}
	for _, kind := range []struct{ from, to *[]string }{{&p.Pin, &sub.Pin}, {&p.ReadOnly, &sub.ReadOnly}, {&p.Empty, &sub.Empty}} {
		for _, q := range *kind.from {
			if r, ok := inside(q); ok && r != "" {
				*kind.to = append(*kind.to, r)
			}
		}
	}
	return sub, ""
}

// viewTypes are filesystems that show another folder's files under a device
// of their own, so the device check above does not find them.
func viewType(t string) bool {
	switch {
	case strings.HasPrefix(t, "fuse"), t == "overlay", t == "aufs", strings.HasPrefix(t, "nfs"),
		t == "cifs", t == "smb3", t == "9p", t == "virtiofs":
		return true
	}
	return false
}

// views are mounts of such a filesystem naming the workspace, or a folder in or
// above it, as source or overlay layer: the fence's covers miss them.
func views(entries []mountEntry, wsMount int, wsPath, hostname string) []string {
	var out []string
	related := func(p string) bool {
		p = path.Clean(p)
		if !strings.HasPrefix(p, "/") {
			return false
		}
		_, in := within(p, wsPath)
		_, holds := within(wsPath, p)
		return (in || holds) && p != "/"
	}
	for _, e := range entries {
		// The workspace's own mount, as on a network home folder, is no view of it.
		if !viewType(e.Type) || e.ID == wsMount {
			continue
		}
		var named []string
		// A source host:/path shows this host's folder only when the host is this one.
		if host, p, ok := strings.Cut(e.Source, ":"); ok && !strings.HasPrefix(e.Source, "/") {
			if _, h, at := strings.Cut(host, "@"); at {
				host = h
			}
			host = strings.Trim(host, "[]")
			if host == "localhost" || host == "::1" || strings.HasPrefix(host, "127.") || (hostname != "" && strings.EqualFold(host, hostname)) {
				named = append(named, p)
			}
		} else {
			named = append(named, e.Source)
		}
		for _, opt := range strings.Split(e.Super, ",") {
			k, v, ok := strings.Cut(opt, "=")
			if !ok {
				continue
			}
			switch k {
			case "lowerdir", "upperdir", "workdir", "lowerdir+", "datadir+":
				named = append(named, strings.Split(v, ":")...)
			}
		}
		for _, n := range named {
			if related(n) && !slices.Contains(out, e.Point) {
				out = append(out, e.Point)
			}
		}
	}
	return out
}
