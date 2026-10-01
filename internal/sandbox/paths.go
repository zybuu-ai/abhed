package sandbox

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

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

// PathForms are p made absolute and p with its links resolved, once each:
// a rule must name both, since a command may reach either.
func PathForms(p string) []string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil
	}
	if real := RealPath(abs); real != abs {
		return []string{abs, real}
	}
	return []string{abs}
}

// formsOf is PathForms over a list.
func formsOf(paths []string) []string {
	var out []string
	for _, p := range paths {
		out = append(out, PathForms(p)...)
	}
	return out
}

// Within reports whether p is root or under it, with the parts below root;
// names are compared without case, as APFS and NTFS compare them.
func Within(p, root string) (rest []string, ok bool) {
	pp, rp := parts(p), parts(root)
	if len(pp) < len(rp) {
		return nil, false
	}
	for i := range rp {
		if !strings.EqualFold(pp[i], rp[i]) {
			return nil, false
		}
	}
	return pp[len(rp):], true
}

func parts(p string) []string {
	var out []string
	for _, s := range strings.Split(filepath.Clean(p), string(filepath.Separator)) {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// holders are the folders inside the workspace that hold a write-protected
// path, such as .git for .git/hooks. Renaming one would carry the protected
// path out from under its rule, so each is pinned in place: up to the nearest
// .git, or else every folder up to the workspace.
func holders(workspaces, protected []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range protected {
		for _, ws := range workspaces {
			rest, ok := Within(p, ws)
			if !ok || len(rest) < 2 {
				continue
			}
			from := 0
			for i := len(rest) - 2; i >= 0; i-- {
				if strings.EqualFold(rest[i], ".git") {
					from = i
					break
				}
			}
			for i := from; i < len(rest)-1; i++ {
				dir := filepath.Join(append([]string{ws}, rest[:i+1]...)...)
				if !seen[dir] {
					seen[dir] = true
					out = append(out, dir)
				}
			}
		}
	}
	return out
}

// regexQuote escapes a path for a seatbelt regex.
func regexQuote(p string) string {
	var b strings.Builder
	for _, r := range p {
		if strings.ContainsRune(`\.^$|?*+()[]{}"`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// anyCase is a regex matching s in any case, as APFS names compare.
func anyCase(s string) string {
	var b strings.Builder
	for _, r := range s {
		lo, up := strings.ToLower(string(r)), strings.ToUpper(string(r))
		if lo == up {
			b.WriteString(regexQuote(string(r)))
			continue
		}
		b.WriteString("[" + lo + up + "]")
	}
	return b.String()
}

// insideAny reports whether p is under one of roots.
func insideAny(p string, roots []string) bool {
	for _, r := range roots {
		if _, ok := Within(p, r); ok {
			return true
		}
	}
	return false
}
