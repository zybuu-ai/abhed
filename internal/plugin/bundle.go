package plugin

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/internal/frontmatter"
)

// Limits on what a bundle may hold, so reading one is bounded.
const (
	MaxFiles     = 2000
	MaxFileBytes = 8 << 20
	MaxTotal     = 64 << 20
)

// Bundle is a bundle read into memory and checked against its manifest.
// Nothing in it has run.
type Bundle struct {
	Manifest *Manifest
	// ManifestBytes are the manifest exactly as read; Digest is their SHA-256
	// and the bundle's identity.
	ManifestBytes []byte
	Digest        string
	// Signature is the Sigstore bundle over the manifest, nil when unsigned.
	Signature []byte
	// Files are the listed files' contents, by path.
	Files map[string][]byte
}

// Open reads the bundle at p: a directory, or a .tar.gz or .tgz of one. It
// refuses links, devices, unlisted or missing files and any hash mismatch.
func Open(p string) (*Bundle, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	var raw map[string][]byte
	switch {
	case info.IsDir():
		raw, err = readDir(p)
	case info.Mode().IsRegular():
		var f *os.File
		if f, err = os.Open(p); err != nil { // #nosec G304 -- the archive the person named
			return nil, err
		}
		defer func() { _ = f.Close() }()
		raw, err = ReadArchive(f)
	default:
		return nil, fmt.Errorf("%s is neither a folder nor an archive", p)
	}
	if err != nil {
		return nil, err
	}
	return FromFiles(raw)
}

// readDir reads every regular file under root. A link or any other kind of
// file is refused, never followed.
func readDir(root string) (map[string][]byte, error) {
	out := map[string][]byte{}
	total := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		switch t := d.Type(); {
		case t.IsDir():
			return nil
		case !t.IsRegular():
			return fmt.Errorf("%s is a link or special file; a bundle holds regular files only", rel)
		}
		if len(out) >= MaxFiles {
			return fmt.Errorf("more than %d files", MaxFiles)
		}
		data, err := readBounded(p)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if total += len(data); total > MaxTotal {
			return fmt.Errorf("more than %d bytes in all", MaxTotal)
		}
		out[rel] = data
		return nil
	})
	return out, err
}

// readBounded reads one regular file, never through a link, even one put
// in its place after the walk looked.
func readBounded(p string) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|oNoFollow, 0) // #nosec G304 -- a file of the bundle the person named
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("over %d bytes", MaxFileBytes)
	}
	return data, nil
}

// ReadArchive reads a gzipped tar. Entries may sit under one top folder,
// which is dropped. Links, devices and paths leaving the root are refused.
func ReadArchive(r io.Reader) (map[string][]byte, error) {
	gz, err := gzip.NewReader(io.LimitReader(r, MaxTotal*2))
	if err != nil {
		return nil, fmt.Errorf("not a gzipped tar: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	total := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the archive: %w", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || slices.Contains(strings.Split(name, "/"), "..") {
			return nil, fmt.Errorf("archive entry %q leaves the bundle", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		case tar.TypeXGlobalHeader:
			continue
		default:
			return nil, fmt.Errorf("archive entry %q is a link or special file; a bundle holds regular files only", h.Name)
		}
		if len(out) >= MaxFiles {
			return nil, fmt.Errorf("more than %d files", MaxFiles)
		}
		if h.Size > MaxFileBytes {
			return nil, fmt.Errorf("%s is over %d bytes", name, MaxFileBytes)
		}
		data, err := io.ReadAll(io.LimitReader(tr, MaxFileBytes+1))
		if err != nil {
			return nil, err
		}
		if total += len(data); total > MaxTotal {
			return nil, fmt.Errorf("more than %d bytes in all", MaxTotal)
		}
		name = path.Clean(name)
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("archive holds %s twice", name)
		}
		out[name] = data
	}
	return stripTop(out), nil
}

// stripTop drops a single folder every entry sits under, as `tar czf x.tgz
// dir` makes, when the manifest is not at the root.
func stripTop(files map[string][]byte) map[string][]byte {
	if _, ok := files[ManifestName]; ok {
		return files
	}
	top := ""
	for p := range files {
		first, _, nested := strings.Cut(p, "/")
		if !nested || (top != "" && first != top) {
			return files
		}
		top = first
	}
	out := make(map[string][]byte, len(files))
	for p, d := range files {
		out[strings.TrimPrefix(p, top+"/")] = d
	}
	return out
}

// FromFiles checks files, keyed by slash path, as a bundle.
func FromFiles(raw map[string][]byte) (*Bundle, error) {
	mdata, ok := raw[ManifestName]
	if !ok {
		return nil, fmt.Errorf("no %s at the bundle's root", ManifestName)
	}
	m, err := ParseManifest(mdata)
	if err != nil {
		return nil, err
	}
	b := &Bundle{Manifest: m, ManifestBytes: mdata, Digest: Digest(mdata), Signature: raw[SignatureName], Files: map[string][]byte{}}
	listed := map[string]bool{}
	for _, f := range m.Files {
		data, ok := raw[f.Path]
		if !ok {
			return nil, fmt.Errorf("%s is listed but not in the bundle", f.Path)
		}
		if got := Digest(data); got != f.SHA256 {
			return nil, fmt.Errorf("%s does not match its sha256 in the manifest (it was changed after the manifest was made)", f.Path)
		}
		b.Files[f.Path] = data
		listed[f.Path] = true
	}
	var extra []string
	for p := range raw {
		if p != ManifestName && p != SignatureName && !listed[p] {
			extra = append(extra, p)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return nil, fmt.Errorf("the bundle holds files its manifest does not list: %s", strings.Join(extra, ", "))
	}
	if err := b.checkContents(); err != nil {
		return nil, err
	}
	return b, nil
}

// Contents are what a bundle adds, by kind.
type Contents struct {
	Agents   []string // agents/*.md, as paths
	Skills   []string // skill folder names under skills/
	Commands []string // commands/**/*.md, as paths
	Docs     []string
}

// Contents lists what b adds.
func (b *Bundle) Contents() Contents {
	var c Contents
	skills := map[string]bool{}
	for p := range b.Files {
		top, rest, _ := strings.Cut(p, "/")
		switch top {
		case "agents":
			if !strings.Contains(rest, "/") && strings.HasSuffix(rest, ".md") {
				c.Agents = append(c.Agents, p)
			}
		case "skills":
			if name, file, ok := strings.Cut(rest, "/"); ok && file == "SKILL.md" {
				skills[name] = true
			}
		case "commands":
			if strings.HasSuffix(rest, ".md") {
				c.Commands = append(c.Commands, p)
			}
		case "docs":
			c.Docs = append(c.Docs, p)
		}
	}
	for s := range skills {
		c.Skills = append(c.Skills, s)
	}
	sort.Strings(c.Agents)
	sort.Strings(c.Skills)
	sort.Strings(c.Commands)
	sort.Strings(c.Docs)
	return c
}

// checkContents refuses a skill folder without SKILL.md, and content that
// needs more than the manifest requests.
func (b *Bundle) checkContents() error {
	for p := range b.Files {
		if rest, ok := strings.CutPrefix(p, "skills/"); ok {
			name, _, nested := strings.Cut(rest, "/")
			if !nested {
				return fmt.Errorf("%s: a skill is a folder skills/<name>/ holding SKILL.md", p)
			}
			if !partRE.MatchString(name) {
				return fmt.Errorf("skill folder %q: letters, digits, '_' and '-'", name)
			}
			if _, ok := b.Files["skills/"+name+"/SKILL.md"]; !ok {
				return fmt.Errorf("skills/%s has no SKILL.md", name)
			}
		}
	}
	need := b.Needs()
	have := b.Manifest.Capabilities
	var missing []string
	for _, pair := range []struct {
		kind       string
		need, have []string
	}{{"tools", need.Tools, have.Tools}, {"egress", need.Egress, have.Egress}, {"hooks", need.Hooks, have.Hooks}, {"mcp_servers", need.MCPServers, have.MCPServers}} {
		for _, n := range pair.need {
			if !slices.Contains(pair.have, n) {
				missing = append(missing, pair.kind+" "+n)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the manifest's capabilities leave out what its content uses: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Needs are the capabilities b's content uses: the tools its agents and
// commands list, the hosts its hooks and URL servers reach, its hook events
// and its server names.
func (b *Bundle) Needs() Capabilities {
	var c Capabilities
	add := func(list *[]string, v string) {
		if v != "" && !slices.Contains(*list, v) {
			*list = append(*list, v)
		}
	}
	cont := b.Contents()
	for _, p := range append(append([]string{}, cont.Agents...), cont.Commands...) {
		doc, err := frontmatter.Parse(string(b.Files[p]))
		if err != nil {
			continue
		}
		for _, f := range doc.Top() {
			k := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(strings.Trim(f.Key, `"'`)))
			if k != "tools" && k != "allowedtools" {
				continue
			}
			items := f.List
			if len(items) == 0 && f.Value != "" {
				items = strings.Split(f.Value, ",")
			}
			for _, t := range items {
				add(&c.Tools, strings.TrimSpace(t))
			}
		}
	}
	for _, h := range b.Manifest.Hooks {
		for _, ev := range h.Events {
			add(&c.Hooks, ev)
		}
		if h.URL != "" {
			host, _ := hostOf(h.URL)
			add(&c.Egress, host)
		}
	}
	for _, s := range b.Manifest.MCPServers {
		add(&c.MCPServers, s.Name)
		if s.URL != "" {
			host, _ := hostOf(s.URL)
			add(&c.Egress, host)
		}
	}
	for _, l := range []*[]string{&c.Tools, &c.Egress, &c.Hooks, &c.MCPServers} {
		sort.Strings(*l)
	}
	return c
}
