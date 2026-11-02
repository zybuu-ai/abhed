package toolchain

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ulikunitz/xz"
)

// Bundled is a library installed inside a toolchain because builds link it
// and images may lack it.
type Bundled struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	URL     string `json:"url"`
	Digest  string `json:"digest"`
}

// debianLib is one Debian package a toolchain bundles a library from, its
// sha256 pinned as packages.debian.org and the archive's Packages list it.
type debianLib struct {
	name, version, pool string
	files               []string // the library's files in the package, by name
	sha256              map[string]string
	size                map[string]int64
}

// libyaml is Debian 12's libyaml, which ruby-builder's Linux builds link
// (psych, so YAML, RubyGems and Bundler) and debian:bookworm-slim lacks.
var libyaml = debianLib{
	name: "libyaml-0-2", version: "0.2.5-1", pool: "liby/libyaml/",
	files: []string{"libyaml-0.so.2", "libyaml-0.so.2.0.9"},
	sha256: map[string]string{
		"amd64": "207b539919a47c85bcf738677f0ccf5bbac9844f2d3f158696f518be4c4ba6c4",
		"arm64": "4da1a8966803efcfe320b6737e1a2a8306eced0cc578205a2a5ee65e3247fc76",
	},
	size: map[string]int64{"amd64": 53580, "arm64": 50352},
}

// debianHosts are where Debian's packages are served from.
var debianHosts = []string{"deb.debian.org"}

func (l debianLib) artifact(e Endpoints, p Platform) (Artifact, bool) {
	sum, ok := l.sha256[p.Arch]
	if !ok || p.OS != "linux" {
		return Artifact{}, false
	}
	pool := e.DebianPool
	if pool == "" {
		pool = "https://deb.debian.org/debian/pool/main/"
	}
	return Artifact{Lang: "ruby", Version: l.version, Platform: p, URL: pool + l.pool + l.name + "_" + l.version + "_" + p.Arch + ".deb",
		Digest: "sha256:" + sum, Size: l.size[p.Arch], Format: "deb", Source: "Debian 12 (deb.debian.org)", Hosts: debianHosts}, true
}

// rubyWrapper runs a Linux Ruby with its own libraries found: only Ruby and
// what it starts get LD_LIBRARY_PATH, not every command of the session.
const rubyWrapper = `#!/bin/sh
d=$(cd "$(dirname "$0")/.." && pwd) || exit 1
LD_LIBRARY_PATH="$d/lib:$d/lib/abhed-deps${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" exec "$d/bin/ruby" "$@"
`

// rubyWrapperDir is where the wrapper is, first on PATH before bin.
const rubyWrapperDir = "abhed-bin"

// rubyLinux finishes a Linux Ruby: libyaml bundled, and the wrapper that
// finds its libraries. A macOS build needs neither.
func rubyLinux(ctx context.Context, src *Source, top, tmp string, a Artifact) ([]Bundled, error) {
	if a.Platform.OS != "linux" {
		return nil, nil
	}
	got, err := bundleLibyaml(ctx, src, top, tmp, a)
	if err != nil {
		return nil, err
	}
	// A folder of its own: one the archive made, or a link, refuses the install.
	if err := os.Mkdir(filepath.Join(top, rubyWrapperDir), 0o755); err != nil { // #nosec G301 -- read by the sandbox's commands
		return nil, fmt.Errorf("the Ruby wrapper's folder: %w", err)
	}
	if err := writeExclusive(filepath.Join(top, rubyWrapperDir, "ruby"), []byte(rubyWrapper), 0o755); err != nil {
		return nil, err
	}
	return got, nil
}

// bundleLibyaml puts libyaml in a Linux Ruby's lib/abhed-deps, where its
// LD_LIBRARY_PATH finds it.
func bundleLibyaml(ctx context.Context, src *Source, top, tmp string, a Artifact) ([]Bundled, error) {
	art, ok := libyaml.artifact(src.Endpoints, a.Platform)
	if !ok {
		return nil, nil
	}
	deb := filepath.Join(tmp, "libyaml.deb")
	if _, err := src.download(ctx, art, deb, nil); err != nil {
		return nil, fmt.Errorf("libyaml for Ruby: %w", err)
	}
	dest, err := realDir(top, filepath.Join("lib", "abhed-deps"))
	if err != nil {
		return nil, err
	}
	if err := debLibs(deb, dest, libyaml.files); err != nil {
		return nil, fmt.Errorf("libyaml for Ruby: %w", err)
	}
	return []Bundled{{Name: "libyaml", Version: libyaml.version, URL: art.URL, Digest: art.Digest}}, nil
}

// debLibs copies the named files (regular files and links to them) out of a
// .deb's data.tar.xz into dest, flat.
func debLibs(deb, dest string, names []string) error {
	f, err := os.Open(deb) // #nosec G304 -- the cache's own download
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	magic := make([]byte, 8)
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != "!<arch>\n" {
		return errors.New("not a Debian package")
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	for {
		hdr := make([]byte, 60)
		if _, err := io.ReadFull(r, hdr); err != nil {
			return errors.New("the package has no data.tar.xz")
		}
		name := strings.TrimSuffix(strings.TrimSpace(string(hdr[0:16])), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
		if err != nil || size < 0 || size > 64<<20 {
			return errors.New("the package's member list cannot be read")
		}
		body := io.LimitReader(r, size)
		if name == "data.tar.xz" {
			return extractNamed(body, dest, want)
		}
		if _, err := io.Copy(io.Discard, body); err != nil {
			return err
		}
		if size%2 == 1 {
			_, _ = r.ReadByte()
		}
	}
}

func extractNamed(r io.Reader, dest string, want map[string]bool) error {
	xr, err := xz.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(xr)
	found := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		base := filepath.Base(hdr.Name)
		if !want[base] {
			continue
		}
		p := filepath.Join(dest, base)
		switch hdr.Typeflag {
		case tar.TypeReg:
			if hdr.Size > 16<<20 {
				return fmt.Errorf("%s is larger than a library should be", base)
			}
			if err := writeFile(p, tr, hdr.Size, 0o644); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if strings.Contains(hdr.Linkname, "/") || !want[hdr.Linkname] {
				return fmt.Errorf("refused the link %s -> %s", base, hdr.Linkname)
			}
			if err := os.Symlink(hdr.Linkname, p); err != nil {
				return err
			}
		default:
			continue
		}
		found[base] = true
	}
	for n := range want {
		if !found[n] {
			return fmt.Errorf("the package has no %s", n)
		}
	}
	return nil
}
