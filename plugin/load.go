package plugin

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Loaded is one approved plugin's content, read and checked at load. The
// bytes here are the ones that were hashed, so nothing is read twice.
type Loaded struct {
	Name, Version, Publisher, Digest, Basis string
	Signer                                  *Signer
	// Root is the installed folder.
	Root     string
	Agents   []LoadedFile
	Skills   []LoadedSkill
	Commands []LoadedFile
	// Hooks and MCPServers carry the plugin's name in theirs, and RootVar
	// replaced with Root.
	Hooks      []Hook
	MCPServers []MCPServer
}

// LoadedFile is one agent or command file.
type LoadedFile struct {
	// Rel is the path under agents/ or commands/, slash-separated.
	Rel  string
	Path string
	Data []byte
}

// LoadedSkill is one skill folder.
type LoadedSkill struct {
	Name     string
	Dir      string
	SkillMD  []byte
	Pipeline []byte
}

// HookName is a plugin hook's name as sessions and the record see it.
func HookName(plugin, hook string) string { return plugin + "/" + hook }

// ServerName is a plugin MCP server's name, so its tools are
// mcp__<plugin>-<server>__* and no rule written for another server matches.
func ServerName(plugin, server string) string { return plugin + "-" + server }

// LoadAll reads every approved plugin that today's policy still admits.
// Each one refused is a warning, never an error: one bad plugin must not
// stop a session, and it never loads in part.
func LoadAll(s *Store, p Policy, abhedVersion string) ([]Loaded, []string) {
	st, err := s.Read()
	if err != nil {
		return nil, []string{fmt.Sprintf("plugins: %v; none loaded", err)}
	}
	if len(st.Plugins) == 0 {
		return nil, nil
	}
	if p.Disabled {
		return nil, []string{"plugins: none loaded: " + ErrDisabled.Error()}
	}
	var out []Loaded
	var warns []string
	for _, name := range st.Names() {
		e := st.Plugins[name]
		if e.Approved == nil {
			continue
		}
		basis, err := p.Admit(e.Approved.Evidence(), st.Publishers)
		if err != nil {
			warns = append(warns, fmt.Sprintf("plugin %s is not loaded: %v", name, err))
			continue
		}
		b, err := s.Open(name, e.Approved)
		if err != nil {
			warns = append(warns, fmt.Sprintf("plugin %s is not loaded: %v", name, err))
			continue
		}
		if !AtLeast(abhedVersion, b.Manifest.Abhed) {
			warns = append(warns, fmt.Sprintf("plugin %s is not loaded: it needs Abhed %s or later", name, b.Manifest.Abhed))
			continue
		}
		out = append(out, toLoaded(b, s.Path(name, e.Approved), basis, e.Approved.Signer))
	}
	return out, warns
}

func toLoaded(b *Bundle, root, basis string, signer *Signer) Loaded {
	m := b.Manifest
	l := Loaded{Name: m.Name, Version: m.Version, Publisher: m.Publisher, Digest: b.Digest, Basis: basis, Signer: signer, Root: root}
	c := b.Contents()
	for _, p := range c.Agents {
		l.Agents = append(l.Agents, LoadedFile{Rel: strings.TrimPrefix(p, "agents/"), Path: filepath.Join(root, filepath.FromSlash(p)), Data: b.Files[p]})
	}
	for _, p := range c.Commands {
		l.Commands = append(l.Commands, LoadedFile{Rel: strings.TrimPrefix(p, "commands/"), Path: filepath.Join(root, filepath.FromSlash(p)), Data: b.Files[p]})
	}
	for _, name := range c.Skills {
		dir := path.Join("skills", name)
		l.Skills = append(l.Skills, LoadedSkill{Name: name, Dir: filepath.Join(root, filepath.FromSlash(dir)),
			SkillMD: b.Files[dir+"/SKILL.md"], Pipeline: b.Files[dir+"/pipeline.json"]})
	}
	quoted := shellQuote(root)
	for _, h := range m.Hooks {
		h.Name = HookName(m.Name, h.Name)
		h.Command = expand(h.Command, quoted)
		h.Events = append([]string(nil), h.Events...)
		l.Hooks = append(l.Hooks, h)
	}
	for _, sv := range m.MCPServers {
		sv.Name = ServerName(m.Name, sv.Name)
		sv.Command = expand(sv.Command, root)
		args := make([]string, len(sv.Args))
		for i, a := range sv.Args {
			args[i] = expand(a, root)
		}
		sv.Args = args
		l.MCPServers = append(l.MCPServers, sv)
	}
	return l
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Describe is what a bundle adds and requests, one line each, for a person
// to approve.
func Describe(b *Bundle) []string {
	m := b.Manifest
	c := b.Contents()
	var out []string
	add := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }
	add("%s %s from %s", m.Name, m.Version, m.Publisher)
	if m.Description != "" {
		add("  %s", m.Description)
	}
	add("  digest   %s", b.Digest)
	if m.Abhed != "" {
		add("  needs    Abhed %s or later", m.Abhed)
	}
	add("It adds:")
	none := true
	for _, a := range c.Agents {
		add("  agent    %s", strings.TrimSuffix(strings.TrimPrefix(a, "agents/"), ".md"))
		none = false
	}
	for _, s := range c.Skills {
		add("  skill    %s", s)
		none = false
	}
	for _, cmd := range c.Commands {
		add("  command  /%s", strings.TrimSuffix(strings.TrimPrefix(cmd, "commands/"), ".md"))
		none = false
	}
	for _, h := range m.Hooks {
		what := h.Command
		if h.URL != "" {
			what = "POST " + h.URL
		}
		add("  hook     %s on %s: %s", HookName(m.Name, h.Name), strings.Join(h.Events, ", "), what)
		none = false
	}
	for _, s := range m.MCPServers {
		what := strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
		if s.URL != "" {
			what = s.URL
		}
		add("  mcp      %s: %s", ServerName(m.Name, s.Name), what)
		none = false
	}
	if len(c.Docs) > 0 {
		add("  docs     %d file(s)", len(c.Docs))
	}
	if none {
		add("  nothing that loads")
	}
	add("It requests:")
	caps := m.Capabilities
	for _, row := range []struct {
		label string
		v     []string
	}{{"tools", caps.Tools}, {"egress", caps.Egress}, {"hooks", caps.Hooks}, {"mcp", caps.MCPServers}} {
		v := "none"
		if len(row.v) > 0 {
			v = strings.Join(row.v, ", ")
		}
		add("  %-8s %s", row.label, v)
	}
	add("Its hooks only ever block; its servers and hooks stay under your deny rules, the sandbox and the egress policy.")
	return out
}

// Diff is what changes from old to new, one line each.
func Diff(old, new *Bundle) []string {
	var out []string
	add := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }
	add("%s %s -> %s", new.Manifest.Name, old.Manifest.Version, new.Manifest.Version)
	if old.Manifest.Publisher != new.Manifest.Publisher {
		add("  publisher %q -> %q", old.Manifest.Publisher, new.Manifest.Publisher)
	}
	oldF, newF := fileMap(old), fileMap(new)
	var paths []string
	for p := range oldF {
		paths = append(paths, p)
	}
	for p := range newF {
		if _, ok := oldF[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		o, inOld := oldF[p]
		n, inNew := newF[p]
		switch {
		case !inOld:
			add("  + %s", p)
		case !inNew:
			add("  - %s", p)
		case o != n:
			add("  ~ %s", p)
		}
	}
	hookLine := func(h Hook) string {
		return fmt.Sprintf("hook %s on %s: %s%s", h.Name, strings.Join(h.Events, ","), h.Command, h.URL)
	}
	srvLine := func(s MCPServer) string {
		return fmt.Sprintf("mcp %s: %s %s%s", s.Name, s.Command, strings.Join(s.Args, " "), s.URL)
	}
	diffLines(&out, mapLines(old.Manifest.Hooks, hookLine), mapLines(new.Manifest.Hooks, hookLine))
	diffLines(&out, mapLines(old.Manifest.MCPServers, srvLine), mapLines(new.Manifest.MCPServers, srvLine))
	oc, nc := old.Manifest.Capabilities, new.Manifest.Capabilities
	for _, row := range []struct {
		label string
		o, n  []string
	}{{"tools", oc.Tools, nc.Tools}, {"egress", oc.Egress, nc.Egress}, {"hooks", oc.Hooks, nc.Hooks}, {"mcp", oc.MCPServers, nc.MCPServers}} {
		for _, v := range row.n {
			if !slices.Contains(row.o, v) {
				add("  + requests %s %s", row.label, v)
			}
		}
		for _, v := range row.o {
			if !slices.Contains(row.n, v) {
				add("  - requests %s %s", row.label, v)
			}
		}
	}
	if len(out) == 1 {
		add("  no file, hook, server or request changed")
	}
	return out
}

func fileMap(b *Bundle) map[string]string {
	out := map[string]string{}
	for _, f := range b.Manifest.Files {
		out[f.Path] = f.SHA256
	}
	return out
}

func mapLines[T any](items []T, f func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, f(it))
	}
	return out
}

func diffLines(out *[]string, old, new []string) {
	for _, n := range new {
		if !slices.Contains(old, n) {
			*out = append(*out, "  + "+n)
		}
	}
	for _, o := range old {
		if !slices.Contains(new, o) {
			*out = append(*out, "  - "+o)
		}
	}
}
