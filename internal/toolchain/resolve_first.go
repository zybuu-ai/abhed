package toolchain

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
)

// resolveGo reads go.dev/dl's release list, whose entries carry each
// archive's sha256.
func (s *Source) resolveGo(ctx context.Context, want string, p Platform) (Artifact, error) {
	var rel []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
		Files   []struct {
			Filename string `json:"filename"`
			OS       string `json:"os"`
			Arch     string `json:"arch"`
			SHA256   string `json:"sha256"`
			Size     int64  `json:"size"`
			Kind     string `json:"kind"`
		} `json:"files"`
	}
	base := s.Endpoints.GoDL
	if err := s.getJSON(ctx, base+"?mode=json&include=all", []string{hostOf(base)}, &rel); err != nil {
		return Artifact{}, err
	}
	var best Artifact
	for _, r := range rel {
		v := strings.TrimPrefix(r.Version, "go")
		if !r.Stable || !versionRE.MatchString(v) || !Matches(v, want) {
			continue
		}
		if best.Version != "" && compareVersions(v, best.Version) <= 0 {
			continue
		}
		for _, f := range r.Files {
			if f.Kind == "archive" && f.OS == p.OS && f.Arch == p.Arch && strings.HasSuffix(f.Filename, ".tar.gz") && f.SHA256 != "" {
				best = Artifact{Version: v, URL: base + f.Filename, Digest: "sha256:" + f.SHA256, Size: f.Size, Format: "tar.gz",
					Source: "the Go project's releases (go.dev/dl)", Hosts: []string{hostOf(base), "dl.google.com"}}
			}
		}
	}
	if best.Version == "" {
		return Artifact{}, errNoBuild("Go", want, p)
	}
	return best, nil
}

// nodeArch is Node's name for an architecture.
func nodeArch(arch string) string {
	switch arch {
	case "amd64":
		return "x64"
	case "386":
		return "x86"
	}
	return arch
}

// resolveNode reads nodejs.org/dist's index; the latest stable is the newest
// LTS. The tarball's sha256 comes from the release's SHASUMS256.txt.
func (s *Source) resolveNode(ctx context.Context, want string, p Platform) (Artifact, error) {
	var idx []struct {
		Version string `json:"version"`
		Files   []string
		LTS     any `json:"lts"`
	}
	base := s.Endpoints.NodeDist
	hosts := []string{hostOf(base)}
	if err := s.getJSON(ctx, base+"index.json", hosts, &idx); err != nil {
		return Artifact{}, err
	}
	osName := map[string]string{"darwin": "darwin", "linux": "linux"}[p.OS]
	if osName == "" {
		return Artifact{}, errNoBuild("Node.js", want, p)
	}
	key := map[string]string{"darwin": "osx-" + nodeArch(p.Arch) + "-tar", "linux": "linux-" + nodeArch(p.Arch)}[p.OS]
	version := ""
	for _, r := range idx {
		v := strings.TrimPrefix(r.Version, "v")
		lts := r.LTS != nil && r.LTS != false
		if !versionRE.MatchString(v) || !Matches(v, want) || want == "" && !lts || !contains(r.Files, key) {
			continue
		}
		if version == "" || compareVersions(v, version) > 0 {
			version = v
		}
	}
	if version == "" {
		return Artifact{}, errNoBuild("Node.js", want, p)
	}
	name := fmt.Sprintf("node-v%s-%s-%s.tar.gz", version, osName, nodeArch(p.Arch))
	dir := base + "v" + version + "/"
	sums, err := s.get(ctx, dir+"SHASUMS256.txt", hosts)
	if err != nil {
		return Artifact{}, err
	}
	sum, ok := sumFor(sums, name)
	if !ok {
		return Artifact{}, fmt.Errorf("SHASUMS256.txt of Node.js %s does not list %s", version, name)
	}
	return Artifact{Version: version, URL: dir + name, Digest: "sha256:" + sum, Format: "tar.gz",
		Source: "the Node.js project's releases (nodejs.org/dist)", Hosts: hosts}, nil
}

// sumFor finds a file's checksum in a "<hex>  <name>" list.
func sumFor(list []byte, name string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(list))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && isHex(f[0]) {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

func isHex(s string) bool {
	if len(s) < 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// pythonTarget is python-build-standalone's name for a platform.
func pythonTarget(p Platform) string {
	return map[string]string{
		"linux-amd64": "x86_64-unknown-linux-gnu", "linux-arm64": "aarch64-unknown-linux-gnu",
		"darwin-amd64": "x86_64-apple-darwin", "darwin-arm64": "aarch64-apple-darwin",
	}[p.String()]
}

// resolvePython reads the newest python-build-standalone release, which
// carries every supported minor version, and its SHA256SUMS.
func (s *Source) resolvePython(ctx context.Context, want string, p Platform) (Artifact, error) {
	target := pythonTarget(p)
	if target == "" {
		return Artifact{}, errNoBuild("Python", want, p)
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	base := s.Endpoints.PythonAPI
	apiHosts := []string{hostOf(base)}
	if err := s.getJSON(ctx, base+"/latest", apiHosts, &rel); err != nil {
		return Artifact{}, err
	}
	suffix := "-" + target + "-install_only.tar.gz"
	var best Artifact
	sumsURL := ""
	for _, a := range rel.Assets {
		if a.Name == "SHA256SUMS" {
			sumsURL = a.URL
		}
		rest, ok := strings.CutPrefix(a.Name, "cpython-")
		if !ok || !strings.HasSuffix(rest, suffix) {
			continue
		}
		v, _, _ := strings.Cut(strings.TrimSuffix(rest, suffix), "+")
		if !versionRE.MatchString(v) || !Matches(v, want) {
			continue
		}
		if best.Version == "" || compareVersions(v, best.Version) > 0 {
			best = Artifact{Version: v, URL: a.URL, Size: a.Size, Format: "tar.gz",
				Source: "python-build-standalone (github.com/astral-sh/python-build-standalone), not python.org"}
		}
	}
	if best.Version == "" {
		return Artifact{}, errNoBuild("Python", want, p)
	}
	if sumsURL == "" {
		return Artifact{}, fmt.Errorf("release %s has no SHA256SUMS", rel.Tag)
	}
	hosts := append([]string(nil), githubHosts...)
	sums, err := s.get(ctx, sumsURL, hosts)
	if err != nil {
		return Artifact{}, err
	}
	name := best.URL[strings.LastIndex(best.URL, "/")+1:]
	if un, err := url.PathUnescape(name); err == nil {
		name = un // the list names python+build, the URL python%2Bbuild
	}
	sum, ok := sumFor(sums, name)
	if !ok {
		return Artifact{}, fmt.Errorf("SHA256SUMS of release %s does not list %s", rel.Tag, name)
	}
	best.Digest, best.Hosts = "sha256:"+sum, hosts
	return best, nil
}
