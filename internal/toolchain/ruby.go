package toolchain

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// rubyBuild is one ruby/ruby-builder build Abhed knows the digest of.
type rubyBuild struct {
	asset  string // the release asset's platform part
	size   int64
	sha256 string
}

// rubyBuilds are the ruby/ruby-builder builds Abhed installs, with the
// sha256 GitHub publishes for each release asset, pinned here because the
// project publishes no checksum list of its own. A version not here is
// refused; the table moves with Abhed's releases.
var rubyBuilds = map[string]map[string]rubyBuild{
	"3.4.6": {
		"linux-amd64":  {"ubuntu-22.04", 41290391, "6fe5f3d274308e6d159bec44286aef46568ce3de82b9fec172eba6edb503bd52"},
		"linux-arm64":  {"ubuntu-22.04-arm64", 41085606, "c70344bfeb018b1d817a9fe12289cefa5f290a2b060fdfe73525f0a1cfafb082"},
		"darwin-arm64": {"macos-13-arm64", 15206260, "ddfd4686e27966455a86e6b29ae5ec105c4ba67b98ebb2a43d4944224deed51a"},
	},
	"3.4.5": {
		"linux-amd64":  {"ubuntu-22.04", 41201420, "97246f1170eac708172707f18a87df57746c9bf4b2bc620f5c305f5c53e52154"},
		"linux-arm64":  {"ubuntu-22.04-arm64", 41007819, "617eaffdf3f3724e43c0f16393bdc9bef4696348e74f95c901747f48234976a8"},
		"darwin-arm64": {"macos-13-arm64", 15188493, "eba72b6843874d317c761103afac5f125b4f8c72bc8ec080cf264d2a63f2ce0a"},
	},
	"3.3.9": {
		"linux-amd64":  {"ubuntu-22.04", 37579608, "d7d919561852a7a2f9bd170dc8b0f2d06cc7363cf1d32d07c1f4a176d2665d78"},
		"linux-arm64":  {"ubuntu-22.04-arm64", 37393774, "27dc307980dc1ce747c000a318ec10e17893ee626b675abc8994ae4fe94c4158"},
		"darwin-arm64": {"macos-13-arm64", 11287278, "58868c0c5e32d4d98ca9788bec72b709482c03f408ac915e0d3aa473b8bd0c0a"},
	},
	"3.2.9": {
		"linux-amd64":  {"ubuntu-22.04", 35367954, "b5f222583b30880ab7b53894cd238763b7294618c7d7eaa56b7b9bf917586eb2"},
		"linux-arm64":  {"ubuntu-22.04-arm64", 35193246, "c1c51584d30f38ff69e4db1be9db7479a9bb76d4c6046a33dc71d5837bade051"},
		"darwin-arm64": {"macos-13-arm64", 10719353, "7dedbda8cfbd0985cb47c70e8703b6bbb5131b03daf24d1468b1f1a84e3695f6"},
	},
}

// resolveRuby picks from the pinned table; nothing is fetched to decide.
func (s *Source) resolveRuby(_ context.Context, want string, p Platform) (Artifact, error) {
	var versions []string
	for v, builds := range rubyBuilds {
		if _, ok := builds[p.String()]; ok {
			versions = append(versions, v)
		}
	}
	v, ok := newest(versions, want)
	if !ok {
		return Artifact{}, errNoBuild("Ruby", want, p)
	}
	b := rubyBuilds[v][p.String()]
	return Artifact{Version: v, URL: s.Endpoints.RubyDownload + "ruby-" + v + "-" + b.asset + ".tar.gz",
		Digest: "sha256:" + b.sha256, Size: b.size, Format: "tar.gz",
		Source: "ruby/ruby-builder (github.com/ruby/ruby-builder), the builds ruby/setup-ruby uses, with digests pinned in Abhed",
		Hosts:  append([]string(nil), githubHosts...)}, nil
}

// rubyShebang is the build machine's path ruby-builder's scripts start with.
var rubyShebang = regexp.MustCompile(`^#!/opt/hostedtoolcache/Ruby/[^\s]+/bin/ruby`)

func init() {
	// ruby-builder's scripts name the build machine's ruby; they run the
	// ruby on PATH instead, which is this one.
	postInstall["ruby"] = func(dir string, _ Artifact) (string, error) {
		bin := filepath.Join(dir, "bin")
		entries, err := os.ReadDir(bin)
		if err != nil {
			return dir, nil
		}
		for _, e := range entries {
			p := filepath.Join(bin, e.Name())
			info, err := os.Lstat(p)
			if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
				continue
			}
			data, err := os.ReadFile(p) // #nosec G304 -- a file of the toolchain being installed
			if err != nil || !rubyShebang.Match(data) {
				continue
			}
			fixed := rubyShebang.ReplaceAll(data, []byte("#!/usr/bin/env ruby"))
			if err := os.WriteFile(p, fixed, info.Mode().Perm()); err != nil {
				return "", err
			}
		}
		return dir, nil
	}
	postInstall["rust"] = rustInstall
}

// rustInstall lays out the Rust build's components the way its install.sh
// would, without running the script: each component's manifest.in names
// the files and directories it puts under the prefix. The docs are left out.
func rustInstall(dir string, _ Artifact) (string, error) {
	comps, err := os.ReadFile(filepath.Join(dir, "components")) // #nosec G304 -- a file of the toolchain being installed
	if err != nil {
		return "", err
	}
	prefix := filepath.Join(filepath.Dir(dir), "prefix")
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		return "", err
	}
	for _, c := range strings.Fields(string(comps)) {
		if strings.HasPrefix(c, "rust-docs") || strings.ContainsAny(c, `/\`) || c == ".." {
			continue
		}
		manifest, err := os.ReadFile(filepath.Join(dir, c, "manifest.in")) // #nosec G304 -- a file of the toolchain being installed
		if err != nil {
			return "", err
		}
		for _, line := range strings.Split(string(manifest), "\n") {
			kind, rel, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok || kind != "file" && kind != "dir" {
				continue
			}
			from, err := within(filepath.Join(dir, c), rel)
			if err != nil {
				return "", err
			}
			to, err := within(prefix, rel)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
				return "", err
			}
			if err := os.Rename(from, to); err != nil {
				return "", err
			}
		}
	}
	return prefix, nil
}
