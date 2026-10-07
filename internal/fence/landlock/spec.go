package landlock

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Spec is what a command may reach. Every path is absolute. A grant covers
// the path and everything beneath it; a grant that does not exist when the
// ruleset is applied is left out, which only narrows the domain.
type Spec struct {
	// Exec are folders and files to read and run programs from: the system
	// folders.
	Exec []string `json:"exec,omitempty"`
	// Read are folders and files to read but not run from, such as /proc.
	Read []string `json:"read,omitempty"`
	// Write are folders to read, write, create in, remove from and run from:
	// the workspace and the command's private temp folder.
	Write []string `json:"write,omitempty"`
	// Devices are device files or folders to open, read, write and control,
	// such as /dev/null.
	Devices []string `json:"devices,omitempty"`
	// Deny are Abhed's own state: its home folder, the record, secrets. No
	// grant may cover them or sit inside them.
	Deny []string `json:"deny,omitempty"`
	// Within are Exec grants allowed to sit inside a denied path: a part of
	// Abhed's state a command may read and run but never write, such as the
	// skills in ~/.abhed/skills. Each must also be listed in Exec.
	Within []string `json:"within,omitempty"`
	// DenyTCP refuses every TCP connect and bind, for when the network is
	// off. It needs ABI 4 (Linux 6.7) or later.
	DenyTCP bool `json:"deny_tcp,omitempty"`
}

type grantKind struct {
	name  string
	paths []string
}

func (s Spec) grants() []grantKind {
	return []grantKind{
		{"write", s.Write}, {"exec", s.Exec}, {"read", s.Read}, {"devices", s.Devices},
	}
}

// Validate refuses a spec that Landlock cannot apply as written: a relative
// or empty path, a denied path under any grant, or a grant inside denied
// state. It checks paths as written and with symbolic links resolved, so a
// link cannot hide the overlap. It needs no kernel support.
func (s Spec) Validate() error {
	for _, g := range append(s.grants(), grantKind{"deny", s.Deny}) {
		for _, p := range g.paths {
			if p == "" || !path.IsAbs(p) {
				return fmt.Errorf("%w: the %s path %q is not absolute", ErrSpec, g.name, p)
			}
		}
	}
	for _, w := range s.Within {
		if !slices.Contains(s.Exec, w) {
			return fmt.Errorf("%w: %q may sit inside denied state only as an exec grant", ErrSpec, w)
		}
	}
	for _, d := range s.Deny {
		for _, g := range s.grants() {
			for _, p := range g.paths {
				if g.name == "exec" && slices.Contains(s.Within, p) && !within(path.Clean(d), path.Clean(p)) && !within(resolve(d), resolve(p)) {
					continue
				}
				if err := overlap(g.name, p, d, path.Clean(p), path.Clean(d)); err != nil {
					return err
				}
				if err := overlap(g.name, p, d, resolve(p), resolve(d)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// overlap refuses grant g over denied path d, compared as gc and dc.
func overlap(kind, g, d, gc, dc string) error {
	if within(dc, gc) {
		why := "it would be readable"
		if kind == "write" {
			why = "Landlock cannot carve a read-only or hidden area out of a writable one"
		}
		return fmt.Errorf("%w: the denied path %s sits under the %s grant %s, and %s", ErrSpec, d, kind, g, why)
	}
	if within(gc, dc) {
		return fmt.Errorf("%w: the %s grant %s sits inside the denied path %s", ErrSpec, kind, g, d)
	}
	return nil
}

// within reports whether p is base or beneath it. Both are clean.
func within(p, base string) bool {
	return p == base || base == "/" || strings.HasPrefix(p, base+"/")
}

// resolve follows symbolic links in p as far as the path exists, keeping the
// rest as written, so a denied path not yet created still compares by where
// it would land.
func resolve(p string) string {
	p = path.Clean(p)
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return path.Clean(filepath.ToSlash(r) + rest)
		}
		parent := path.Dir(cur)
		if parent == cur {
			return p
		}
		rest = "/" + path.Base(cur) + rest
		cur = parent
	}
}
