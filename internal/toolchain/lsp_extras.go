package toolchain

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The pinned language servers. gopls is built from its module, checked by
// the Go checksum database, as Delve is; the npm packages are pinned by the
// sha512 integrity the registry lists, which must agree; rust-analyzer's
// release assets by the sha256 GitHub lists for each, pinned here, as the
// project publishes no checksum file.
const (
	goplsModule   = "golang.org/x/tools/gopls"
	goplsVersion  = "0.23.0"
	goplsMinGo    = "1.26"
	bpyrVersion   = "1.40.2"
	tslsVersion   = "6.0.2"
	tsVersion     = "5.9.3"
	tslsMinNode   = "22.22.2"
	bpyrMinNode   = "14"
	raVersion     = "2026-10-12"
	defaultNPM    = "https://registry.npmjs.org/"
	defaultRADown = "https://github.com/rust-lang/rust-analyzer/releases/download/"
)

// npmPackage is one npm package an extra installs, pinned by its integrity.
type npmPackage struct {
	name, version, integrity string
	// bins are the commands it provides and the file each runs with node.
	bins map[string]string
}

// npmPins are the pinned npm packages; tests set them.
var npmPins = map[string][]npmPackage{
	"basedpyright": {{name: "basedpyright", version: bpyrVersion,
		integrity: "sha512-9uld8+5tZC2Gxf8nIynyunK0N4xn01W1ApzLMXDAcuA8uJq2QSfldYn9gSbxn9MWR8wmHDo7GaEHLhlnLFHaJA==",
		bins:      map[string]string{"basedpyright-langserver": "langserver.index.js", "basedpyright": "index.js"}}},
	"typescript-language-server": {
		{name: "typescript-language-server", version: tslsVersion,
			integrity: "sha512-Ph7Ql+xkUJvMamnkh1h+aKP6HxLR7lrWIYkPn9/LZBkJdJZgtIj7OoVhkDYtQ7zFPTPmqZHOVXaUKGyz9M2q1Q==",
			bins:      map[string]string{"typescript-language-server": "lib/cli.mjs"}},
		// The tsserver it drives; typescript 7's native build has none.
		{name: "typescript", version: tsVersion,
			integrity: "sha512-jl1vZzPDinLr9eUt3J/t7V6FgNEw9QjvBPdysz9KfQDD41fQrC2Y4vKQdiaUpFT4bXlb1RHhLpp8wtm6M5TgSw==",
			bins:      map[string]string{"tsserver": "bin/tsserver", "tsc": "bin/tsc"}},
	},
}

// raPins are rust-analyzer's release assets for raVersion, by platform,
// with the sha256 GitHub lists; tests set them.
var raPins = map[string]struct {
	target string
	size   int64
	sha256 string
}{
	"linux-amd64":  {"x86_64-unknown-linux-gnu", 14939384, "040cab4e2abc490fadd95c3f6cbc60b941a78761dbc3acf0ec66fd7a97f31db3"},
	"linux-arm64":  {"aarch64-unknown-linux-gnu", 14352498, "d7219d5d8f5537bb227b521157aebddfe7e4a90f4a85a74f31e53722f5691025"},
	"darwin-amd64": {"x86_64-apple-darwin", 14714609, "56a5698a2e0d495c3ec28155416db310c879f21a70f6f38db549166cf157cfab"},
	"darwin-arm64": {"aarch64-apple-darwin", 13943252, "a2e45e828b37bfc4b507c2ca456e8b61096e0bdfbae225f3d33295546d1eeacf"},
}

func (e Endpoints) npmRegistry() string {
	if e.NPMRegistry != "" {
		return e.NPMRegistry
	}
	return defaultNPM
}

func (e Endpoints) raDownload() string {
	if e.RustAnalyzerDownload != "" {
		return e.RustAnalyzerDownload
	}
	return defaultRADown
}

// integrityHex is an npm sha512 integrity as the sha512:<hex> a download
// checks.
func integrityHex(integrity string) (string, error) {
	b64, ok := strings.CutPrefix(integrity, "sha512-")
	if !ok {
		return "", fmt.Errorf("the integrity %q is not sha512", integrity)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != 64 {
		return "", fmt.Errorf("the integrity %q cannot be read", integrity)
	}
	return "sha512:" + hex.EncodeToString(raw), nil
}

// resolveNPM is the pinned package's tarball, refused unless the
// registry lists it with the pinned integrity.
func (s *Source) resolveNPM(ctx context.Context, pkg npmPackage) (Artifact, error) {
	reg := s.Endpoints.npmRegistry()
	var meta struct {
		Dist struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
			Size      int64  `json:"unpackedSize"`
		} `json:"dist"`
	}
	if err := s.getJSON(ctx, reg+pkg.name+"/"+pkg.version, []string{hostOf(reg)}, &meta); err != nil {
		return Artifact{}, err
	}
	if meta.Dist.Integrity != pkg.integrity {
		return Artifact{}, fmt.Errorf("the npm registry lists %s %s with an integrity that is not the one Abhed pins, so it is not installed", pkg.name, pkg.version)
	}
	digest, err := integrityHex(pkg.integrity)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Lang: "node", Version: pkg.version, URL: meta.Dist.Tarball, Digest: digest, Format: "tar.gz",
		Source: "the npm registry (registry.npmjs.org)", Hosts: []string{hostOf(reg)}}, nil
}

// nodeWrapper runs a package's script with the node on PATH.
func nodeWrapper(pkg, script string) string {
	return `#!/bin/sh
command -v node >/dev/null 2>&1 || { echo "abhed: ` + pkg + ` needs Node.js on PATH; install it with: abhed toolchain install node" >&2; exit 127; }
d=$(cd "$(dirname "$0")/.." && pwd) || exit 1
exec node "$d/lib/node_modules/` + pkg + `/` + script + `" "$@"
`
}

// installNPM downloads each of the extra's pinned packages, checks it,
// and lays it out as npm would a global install, with a command for each
// of its programs.
func (c Cache) installNPM(ctx context.Context, src *Source, e Extra, p Platform, progress Progress) (ExtraInstalled, error) {
	pkgs := npmPins[e.Name]
	if len(pkgs) == 0 {
		return ExtraInstalled{}, fmt.Errorf("no npm packages are pinned for %s", e.Name)
	}
	return c.placeExtra(e, p, func(tmp, top string) (ExtraInstalled, error) {
		var x ExtraInstalled
		for i, pkg := range pkgs {
			a, err := src.resolveNPM(ctx, pkg)
			if err != nil {
				return ExtraInstalled{}, err
			}
			tgz := filepath.Join(tmp, fmt.Sprintf("pkg%d.tgz", i))
			n, err := src.download(ctx, a, tgz, progress)
			if err != nil {
				return ExtraInstalled{}, err
			}
			out := filepath.Join(tmp, fmt.Sprintf("pkg%d", i))
			if err := unpack(tgz, "tar.gz", out); err != nil {
				return ExtraInstalled{}, fmt.Errorf("unpack %s: %w", pkg.name, err)
			}
			dest := filepath.Join(top, "lib", "node_modules", pkg.name)
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return ExtraInstalled{}, err
			}
			if err := os.Rename(filepath.Join(out, "package"), dest); err != nil {
				return ExtraInstalled{}, fmt.Errorf("%s has no package folder: %w", pkg.name, err)
			}
			if err := os.MkdirAll(filepath.Join(top, "bin"), 0o755); err != nil {
				return ExtraInstalled{}, err
			}
			for cmd, script := range pkg.bins {
				if _, err := os.Lstat(filepath.Join(dest, filepath.FromSlash(script))); err != nil {
					return ExtraInstalled{}, fmt.Errorf("%s has no %s", pkg.name, script)
				}
				if err := writeExclusive(filepath.Join(top, "bin", cmd), []byte(nodeWrapper(pkg.name, script)), 0o755); err != nil {
					return ExtraInstalled{}, err
				}
			}
			if i == 0 {
				x = ExtraInstalled{Source: a.Source, URL: a.URL, Digest: a.Digest}
			}
			x.Bytes += n
		}
		return x, nil
	})
}

// raArtifact is rust-analyzer's pinned release asset for p.
func (s *Source) raArtifact(p Platform) (Artifact, error) {
	pin, ok := raPins[p.String()]
	if !ok {
		return Artifact{}, fmt.Errorf("no rust-analyzer build for %s is pinned in Abhed", p)
	}
	return Artifact{Lang: "rust", Version: raVersion, Platform: p,
		URL:    s.Endpoints.raDownload() + raVersion + "/rust-analyzer-" + pin.target + ".gz",
		Digest: "sha256:" + pin.sha256, Size: pin.size, Format: "gz",
		Source: "rust-lang/rust-analyzer's GitHub releases, with digests pinned in Abhed", Hosts: append([]string(nil), githubHosts...)}, nil
}

// installRustAnalyzer downloads the pinned release asset, checks it and
// unpacks the one binary it holds.
func (c Cache) installRustAnalyzer(ctx context.Context, src *Source, e Extra, p Platform, progress Progress) (ExtraInstalled, error) {
	a, err := src.raArtifact(p)
	if err != nil {
		return ExtraInstalled{}, err
	}
	return c.placeExtra(e, p, func(tmp, top string) (ExtraInstalled, error) {
		gz := filepath.Join(tmp, "download")
		n, err := src.download(ctx, a, gz, progress)
		if err != nil {
			return ExtraInstalled{}, err
		}
		if err := os.MkdirAll(filepath.Join(top, "bin"), 0o755); err != nil {
			return ExtraInstalled{}, err
		}
		if err := gunzipTo(gz, filepath.Join(top, "bin", "rust-analyzer")); err != nil {
			return ExtraInstalled{}, fmt.Errorf("unpack rust-analyzer: %w", err)
		}
		return ExtraInstalled{Source: a.Source, URL: a.URL, Digest: a.Digest, Bytes: n}, nil
	})
}

// shipsRustAnalyzer reports whether the Rust toolchain at dir has its own
// rust-analyzer, which Rust's full releases carry as a component.
func shipsRustAnalyzer(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Lstat(filepath.Join(dir, "bin", "rust-analyzer"))
	return err == nil && info.Mode().IsRegular()
}

// providedRustAnalyzer is the Rust toolchain's own rust-analyzer as the
// extra: it is that Rust's, in its folder, and goes when it does.
func providedRustAnalyzer(e Extra, lang Installed) ExtraInstalled {
	return ExtraInstalled{Name: e.Name, Lang: e.Lang, Version: "rust-" + lang.Version, Platform: lang.Platform,
		Source: "Rust " + lang.Version + "'s own rust-analyzer component, checked with its release",
		URL:    lang.URL, Digest: lang.Digest, Bytes: lang.Bytes, InstalledAt: lang.InstalledAt, Dir: lang.Dir}
}

// ExtraAvailable reports whether e is there for commands that have the
// installed languages active: in the cache, or carried by the language.
func (c Cache) ExtraAvailable(p Platform, e Extra, active []Installed) bool {
	if _, ok := c.LookupExtra(p, e); ok {
		return true
	}
	for _, in := range active {
		if in.Lang == e.Lang && e.Name == "rust-analyzer" && shipsRustAnalyzer(in.Dir) {
			return true
		}
	}
	return false
}

// maxBinary bounds a single unpacked binary.
const maxBinary = 512 << 20

// gunzipTo unpacks a gzip of one file to an executable at dst.
func gunzipTo(src, dst string) error {
	f, err := os.Open(src) // #nosec G304 -- the cache's own download
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755) // #nosec G302 G304 -- an executable for the sandbox
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(zr, maxBinary+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxBinary {
		err = errors.New("it unpacks to more than Abhed allows")
	}
	return err
}
