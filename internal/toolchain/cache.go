package toolchain

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/filelock"
)

// Cache is the per-user toolchain cache:
// <Root>/<os>-<arch>/<lang>/<version>, each read-only once installed.
type Cache struct {
	Root string
}

// DefaultRoot is ~/.cache/abhed/toolchains, or under XDG_CACHE_HOME.
func DefaultRoot() (string, error) {
	if x := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "abhed", "toolchains"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "abhed", "toolchains"), nil
}

// markerFile records what an installed directory holds; a directory without
// it is not an installed toolchain.
const markerFile = ".abhed-toolchain.json"

// Installed is one toolchain in the cache.
type Installed struct {
	Lang        string    `json:"lang"`
	Version     string    `json:"version"`
	Platform    string    `json:"platform"`
	URL         string    `json:"url"`
	Digest      string    `json:"digest"`
	Bytes       int64     `json:"bytes"`
	InstalledAt time.Time `json:"installed_at"`
	// Bundled are libraries installed inside it, each checked against its pin.
	Bundled []Bundled `json:"bundled,omitempty"`
	// Dir is where it is, filled in when read.
	Dir string `json:"-"`
}

// Prepare makes the cache, readable to every user: a container's commands
// run as this user's id, which a VM's shared folders may not map, and read
// the toolchains through it. Abhed's own folders above it are opened too.
func (c Cache) Prepare() error {
	if err := os.MkdirAll(c.Root, 0o755); err != nil {
		return err
	}
	for _, d := range []string{c.Root, filepath.Dir(c.Root)} {
		if filepath.Base(d) == "toolchains" || filepath.Base(d) == "abhed" {
			if err := os.Chmod(d, 0o755); err != nil { // #nosec G302 -- the cache holds public toolchains, read-only
				return err
			}
		}
	}
	return nil
}

// Dir is where a toolchain is, or would be.
func (c Cache) Dir(p Platform, lang, version string) string {
	return filepath.Join(c.Root, p.String(), lang, version)
}

// Lookup reports an installed toolchain.
func (c Cache) Lookup(p Platform, lang, version string) (Installed, bool) {
	return readMarker(c.Dir(p, lang, version))
}

func readMarker(dir string) (Installed, bool) {
	data, err := os.ReadFile(filepath.Join(dir, markerFile)) // #nosec G304 -- a directory of the toolchain cache
	if err != nil {
		return Installed{}, false
	}
	var in Installed
	if json.Unmarshal(data, &in) != nil {
		return Installed{}, false
	}
	in.Dir = dir
	return in, true
}

// List is every installed toolchain for p, by language then newest first;
// lang "" lists every language.
func (c Cache) List(p Platform, lang string) []Installed {
	var out []Installed
	for _, l := range Langs {
		if lang != "" && l.Name != lang {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(c.Root, p.String(), l.Name))
		if err != nil {
			continue
		}
		var mine []Installed
		for _, e := range entries {
			if !e.IsDir() || !versionRE.MatchString(e.Name()) {
				continue
			}
			if in, ok := readMarker(filepath.Join(c.Root, p.String(), l.Name, e.Name())); ok {
				mine = append(mine, in)
			}
		}
		sort.Slice(mine, func(i, j int) bool { return compareVersions(mine[i].Version, mine[j].Version) > 0 })
		out = append(out, mine...)
	}
	return out
}

// Best is the newest installed toolchain matching spec.
func (c Cache) Best(p Platform, spec Spec) (Installed, bool) {
	for _, in := range c.List(p, spec.Lang) {
		if Matches(in.Version, spec.Version) {
			return in, true
		}
	}
	return Installed{}, false
}

// Limits on what one toolchain may unpack to.
const (
	maxDownload = 2 << 30
	maxUnpacked = 8 << 30
	maxEntries  = 400_000
)

// Progress is told how much of a download has arrived.
type Progress func(done, total int64)

// Install downloads, verifies and unpacks a, unless it is installed already,
// and returns where it is. A download whose checksum does not match is
// thrown away; nothing partial is ever left where the toolchain belongs.
func (c Cache) Install(ctx context.Context, src *Source, a Artifact, progress Progress) (Installed, error) {
	if !versionRE.MatchString(a.Version) {
		return Installed{}, fmt.Errorf("refused the version %q", a.Version)
	}
	if _, ok := Find(a.Lang); !ok {
		return Installed{}, fmt.Errorf("refused the language %q", a.Lang)
	}
	dir := c.Dir(a.Platform, a.Lang, a.Version)
	if in, ok := readMarker(dir); ok {
		return in, nil
	}
	if err := c.Prepare(); err != nil {
		return Installed{}, err
	}
	if err := os.MkdirAll(filepath.Join(c.Root, ".partial"), 0o700); err != nil {
		return Installed{}, err
	}
	unlock, err := filelock.Lock(filepath.Join(c.Root, ".partial", a.Platform.String()+"-"+a.Lang+"-"+a.Version+".lock"),
		30*time.Minute, "another install holds %s (waited %s)")
	if err != nil {
		return Installed{}, err
	}
	defer unlock()
	if in, ok := readMarker(dir); ok {
		return in, nil
	}
	tmp, err := os.MkdirTemp(filepath.Join(c.Root, ".partial"), a.Lang+"-")
	if err != nil {
		return Installed{}, err
	}
	defer func() { _ = removeTree(tmp) }()
	archive := filepath.Join(tmp, "download")
	n, err := src.download(ctx, a, archive, progress)
	if err != nil {
		return Installed{}, err
	}
	out := filepath.Join(tmp, "unpacked")
	if err := unpack(archive, a.Format, out); err != nil {
		return Installed{}, fmt.Errorf("unpack %s %s: %w", a.Lang, a.Version, err)
	}
	top, err := topDir(out)
	if err != nil {
		return Installed{}, err
	}
	in := Installed{Lang: a.Lang, Version: a.Version, Platform: a.Platform.String(), URL: a.URL, Digest: a.Digest,
		Bytes: n, InstalledAt: time.Now().UTC()}
	if fix, ok := postInstall[a.Lang]; ok {
		if top, err = fix(top, a); err != nil {
			return Installed{}, err
		}
	}
	if bundle, ok := bundles[a.Lang]; ok {
		if in.Bundled, err = bundle(ctx, src, top, tmp, a); err != nil {
			return Installed{}, err
		}
	}
	marker, _ := json.MarshalIndent(in, "", "  ")
	if err := os.WriteFile(filepath.Join(top, markerFile), marker, 0o444); err != nil {
		return Installed{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return Installed{}, err
	}
	if err := os.Rename(top, dir); err != nil {
		if got, ok := readMarker(dir); ok {
			return got, nil
		}
		return Installed{}, err
	}
	if err := readOnly(dir); err != nil {
		return Installed{}, err
	}
	in.Dir = dir
	return in, nil
}

// bundles add to a language's tree the libraries its builds link that
// images may lack, before it is moved in place.
var bundles = map[string]func(ctx context.Context, src *Source, top, tmp string, a Artifact) ([]Bundled, error){
	"ruby": rubyLinux,
}

// postInstall finishes a language's unpacked tree before it is moved in place.
// It returns the directory that becomes the toolchain.
var postInstall = map[string]func(dir string, a Artifact) (string, error){}

// Remove deletes an installed toolchain.
func (c Cache) Remove(p Platform, lang, version string) error {
	if !versionRE.MatchString(version) {
		return fmt.Errorf("refused the version %q", version)
	}
	if _, ok := Find(lang); !ok {
		return fmt.Errorf("refused the language %q", lang)
	}
	dir := c.Dir(p, lang, version)
	if _, ok := readMarker(dir); !ok {
		return fmt.Errorf("%s %s is not installed for %s", lang, version, p)
	}
	return removeTree(dir)
}

// download fetches a to path, checking its size and checksum as it arrives.
func (s *Source) download(ctx context.Context, a Artifact, path string, progress Progress) (int64, error) {
	u, err := url.Parse(a.URL)
	if err != nil {
		return 0, err
	}
	hosts := append(append([]string(nil), a.Hosts...), s.Endpoints.ExtraHosts...)
	if !hostAllowed(u, hosts) {
		return 0, fmt.Errorf("%s is not an official release host for %s", u.Host, a.Lang)
	}
	algo, want, _ := strings.Cut(a.Digest, ":")
	var h hash.Hash
	switch algo {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return 0, fmt.Errorf("no checksum to verify %s against", a.URL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return 0, err
	}
	if s.UserAgent != "" {
		req.Header.Set("User-Agent", s.UserAgent)
	}
	resp, err := s.client(a.Hosts).Do(req) // #nosec G704 -- an official release host, checked above and on every redirect
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s answered %s", u.Host, resp.Status)
	}
	total := a.Size
	if total <= 0 {
		total = resp.ContentLength
	}
	if total > maxDownload {
		return 0, fmt.Errorf("%s %s is %d bytes, more than the %d Abhed downloads", a.Lang, a.Version, total, maxDownload)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- inside the cache's own temporary directory
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var done int64
	buf := make([]byte, 256<<10)
	for {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			done += int64(k)
			if done > maxDownload || a.Size > 0 && done > a.Size {
				return 0, fmt.Errorf("the download of %s is larger than published", a.URL)
			}
			h.Write(buf[:k])
			if _, err := f.Write(buf[:k]); err != nil {
				return 0, err
			}
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return 0, rerr
		}
	}
	if a.Size > 0 && done != a.Size {
		return 0, fmt.Errorf("the download of %s is %d bytes, not the %d published", a.URL, done, a.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return 0, fmt.Errorf("the download of %s does not match its published %s checksum (got %s), so it was thrown away", a.URL, algo, got)
	}
	return done, f.Close()
}

// unpack extracts archive into dir, refusing any entry that would land
// outside it.
func unpack(archive, format, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var err error
	switch format {
	case "tar.gz":
		err = untarGz(archive, dir)
	case "zip":
		err = unzip(archive, dir)
	default:
		return fmt.Errorf("unknown archive format %q", format)
	}
	if err != nil {
		return err
	}
	return linksStayWithin(dir)
}

// within resolves name under dir, refusing one that escapes it as text or
// that passes through a symbolic link: nothing is created or linked through
// a link, so a chain of links cannot carry an entry out of dir.
func within(dir, name string) (string, error) {
	name = filepath.FromSlash(name)
	if filepath.IsAbs(name) || name == "" {
		return "", fmt.Errorf("refused the entry %q", name)
	}
	p := filepath.Join(dir, name)
	if p != dir && !strings.HasPrefix(p, dir+string(filepath.Separator)) {
		return "", fmt.Errorf("refused the entry %q, which leaves the toolchain", name)
	}
	if err := noLinkParents(dir, p); err != nil {
		return "", fmt.Errorf("refused the entry %q: %w", name, err)
	}
	return p, nil
}

// noLinkParents refuses p when a folder between dir and p is a symbolic
// link or not a folder. A part that does not exist yet is made a real folder.
func noLinkParents(dir, p string) error {
	rel, err := filepath.Rel(dir, filepath.Dir(p))
	if err != nil {
		return err
	}
	cur := dir
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
		if c == "." || c == "" {
			continue
		}
		cur = filepath.Join(cur, c)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("it passes through the link %s", strings.TrimPrefix(cur, dir+string(filepath.Separator)))
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a folder", strings.TrimPrefix(cur, dir+string(filepath.Separator)))
		}
	}
	return nil
}

// linksStayWithin checks, once everything is unpacked, that every link that
// resolves lands inside dir, so a chain made in any order is refused too.
func linksStayWithin(dir string) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}
		r, err := filepath.EvalSymlinks(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // dangling, and nothing is written through it
		}
		if err != nil || r != real && !strings.HasPrefix(r, real+string(filepath.Separator)) {
			return fmt.Errorf("refused the link %s, which leaves the toolchain", strings.TrimPrefix(p, dir+string(filepath.Separator)))
		}
		return nil
	})
}

// linkWithin reports whether a link at path pointing at target stays in dir.
func linkWithin(dir, path, target string) bool {
	if filepath.IsAbs(target) {
		return false
	}
	t := filepath.Join(filepath.Dir(path), target)
	return t == dir || strings.HasPrefix(t, dir+string(filepath.Separator))
}

func untarGz(archive, dir string) error {
	f, err := os.Open(archive) // #nosec G304 -- the cache's own download
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	var total int64
	for n := 0; ; n++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if n > maxEntries {
			return errors.New("the archive has too many entries")
		}
		p, err := within(dir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > maxUnpacked {
				return errors.New("the archive unpacks to more than Abhed allows")
			}
			if err := writeFileOverLink(p, tr, hdr.Size, hdr.FileInfo().Mode()); err != nil && !caseCollision(p, err) {
				return err
			}
		case tar.TypeSymlink:
			if !linkWithin(dir, p, hdr.Linkname) {
				return fmt.Errorf("refused the link %q -> %q, which leaves the toolchain", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, p); err != nil && !caseCollision(p, err) {
				return err
			}
		case tar.TypeLink:
			src, err := within(dir, hdr.Linkname)
			if err != nil {
				return err
			}
			// Only a regular file already unpacked here may be hard-linked.
			if info, err := os.Lstat(src); err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("refused the hard link %q -> %q, which is not a file of the toolchain", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.Link(src, p); err != nil && !caseCollision(p, err) {
				return err
			}
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
		default:
			// Devices, fifos and the like have no place in a toolchain.
			return fmt.Errorf("refused the entry %q of type %c", hdr.Name, hdr.Typeflag)
		}
	}
}

// writeFileOverLink is writeFile, except that where a link already holds
// the name in another case, as on a case-insensitive disk, the file takes
// its place: a link to the file's own name would otherwise point at itself.
func writeFileOverLink(p string, r io.Reader, size int64, mode fs.FileMode) error {
	err := writeFile(p, r, size, mode)
	if err == nil || !caseCollision(p, err) {
		return err
	}
	if info, lerr := os.Lstat(p); lerr == nil && info.Mode()&fs.ModeSymlink != 0 {
		if rerr := os.Remove(p); rerr != nil {
			return rerr
		}
		return writeFile(p, r, size, mode)
	}
	return err
}

func writeFile(p string, r io.Reader, size int64, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm) // #nosec G304 -- checked to lie inside the unpack directory
	if err != nil {
		return err
	}
	if _, err := io.CopyN(f, r, size); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func unzip(archive, dir string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	if len(zr.File) > maxEntries {
		return errors.New("the archive has too many entries")
	}
	var total uint64
	for _, zf := range zr.File {
		p, err := within(dir, zf.Name)
		if err != nil {
			return err
		}
		mode := zf.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case mode&fs.ModeSymlink != 0:
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			target, err := io.ReadAll(io.LimitReader(rc, 4096))
			_ = rc.Close()
			if err != nil {
				return err
			}
			if !linkWithin(dir, p, string(target)) {
				return fmt.Errorf("refused the link %q, which leaves the toolchain", zf.Name)
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(string(target), p); err != nil && !caseCollision(p, err) {
				return err
			}
		case mode.IsRegular():
			total += zf.UncompressedSize64
			if total > maxUnpacked {
				return errors.New("the archive unpacks to more than Abhed allows")
			}
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			err = writeFileOverLink(p, rc, int64(zf.UncompressedSize64), mode) // #nosec G115 -- bounded by maxUnpacked above
			_ = rc.Close()
			if err != nil && !caseCollision(p, err) {
				return err
			}
		default:
			return fmt.Errorf("refused the entry %q", zf.Name)
		}
	}
	return nil
}

// caseCollision reports whether creating p failed only because a name that
// differs from it in case exists, as on macOS's case-insensitive disks, where
// a Linux build's terminfo holds such pairs; the second of a pair is skipped.
func caseCollision(p string, err error) bool {
	if !errors.Is(err, fs.ErrExist) {
		return false
	}
	entries, rerr := os.ReadDir(filepath.Dir(p))
	if rerr != nil {
		return false
	}
	base, folded := filepath.Base(p), false
	for _, e := range entries {
		if e.Name() == base {
			return false
		}
		folded = folded || strings.EqualFold(e.Name(), base)
	}
	return folded
}

// topDir is the single directory an archive unpacked to, or dir itself when
// it holds more than one entry.
func topDir(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		return filepath.Join(dir, entries[0].Name()), nil
	}
	if len(entries) == 0 {
		return "", errors.New("the archive is empty")
	}
	return dir, nil
}

// readOnly takes write permission away from every file and directory in dir.
func readOnly(dir string) error {
	var dirs []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		return os.Chmod(p, info.Mode().Perm()&^0o222)
	})
	if err != nil {
		return err
	}
	// Directories last, deepest first, so the walk could still enter them.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], 0o555); err != nil {
			return err
		}
	}
	return nil
}

// removeTree gives write permission back to a tree and removes it.
func removeTree(dir string) error {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755) // #nosec G302 -- a directory about to be removed
		}
		return nil
	})
	return os.RemoveAll(dir)
}
