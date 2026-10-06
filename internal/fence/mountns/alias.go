package mountns

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// mountEntry is one line of /proc/self/mountinfo: the mount's id, its
// filesystem's device, the folder of that filesystem it shows (root), and
// where it is mounted (point).
type mountEntry struct {
	ID    int
	Dev   string
	Root  string
	Point string
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
		out = append(out, mountEntry{ID: id, Dev: f[2], Root: unescapeMountinfo(f[3]), Point: unescapeMountinfo(f[4])})
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

// alias is another mount that reaches the workspace's files: Path is where
// it does. Rel is "" when Path is the workspace itself, else the mount shows
// only the workspace's Rel, and Path is that.
type alias struct {
	Path string
	Rel  string
}

// errAlias is a mount of the workspace's files the fence cannot cover.
var errAlias = errors.New("mountns: the workspace is also reachable elsewhere")

// aliases finds every mount other than wsMount, on which the workspace at
// wsPath sits, that shows the same filesystem's folders holding the
// workspace or part of it, such as /sysroot on an ostree host or a bind
// mount. Mounts whose root was unlinked are returned apart, by where they
// are mounted, since what they show can no longer be named.
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

// rebase is the part of p that falls inside the workspace's folder rel, as
// a plan relative to rel. When rel itself lies in a folder p hides or holds
// read-only, the whole of it is covered instead, and whole says how.
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

// isEmpty is whether a plan holds nothing to apply.
func (p Plan) isEmpty() bool { return len(p.Pin)+len(p.ReadOnly)+len(p.Empty) == 0 }
