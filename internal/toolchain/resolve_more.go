package toolchain

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func init() {
	moreResolvers["rust"] = (*Source).resolveRust
	moreResolvers["java"] = (*Source).resolveJava
	moreResolvers["maven"] = (*Source).resolveMaven
	moreResolvers["gradle"] = (*Source).resolveGradle
	moreResolvers["dotnet"] = (*Source).resolveDotnet
	moreResolvers["ruby"] = (*Source).resolveRuby
}

// rustTarget is Rust's name for a platform.
func rustTarget(p Platform) string {
	return map[string]string{
		"linux-amd64": "x86_64-unknown-linux-gnu", "linux-arm64": "aarch64-unknown-linux-gnu",
		"darwin-amd64": "x86_64-apple-darwin", "darwin-arm64": "aarch64-apple-darwin",
		"linux-s390x": "s390x-unknown-linux-gnu", "linux-ppc64le": "powerpc64le-unknown-linux-gnu",
	}[p.String()]
}

// tomlSection reads the key = "value" pairs of one section of a channel
// manifest; it is the only part of TOML these manifests need.
func tomlSection(doc, section string) map[string]string {
	out := map[string]string{}
	in := false
	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			if in {
				break
			}
			in = line == "["+section+"]"
			continue
		}
		if !in {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if s, err := strconv.Unquote(v); err == nil {
			v = s
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// resolveRust reads the release channel manifest on static.rust-lang.org:
// channel-rust-stable.toml, or channel-rust-1.80.toml for a version, which
// names the build for each target and its sha256.
func (s *Source) resolveRust(ctx context.Context, want string, p Platform) (Artifact, error) {
	target := rustTarget(p)
	if target == "" {
		return Artifact{}, errNoBuild("Rust", want, p)
	}
	channel := "stable"
	if want != "" {
		channel = want
	}
	base := s.Endpoints.RustDist
	hosts := []string{hostOf(base), "static.rust-lang.org"}
	doc, err := s.get(ctx, base+"channel-rust-"+channel+".toml", hosts)
	if err != nil {
		return Artifact{}, err
	}
	version, _, _ := strings.Cut(tomlSection(string(doc), "pkg.rust")["version"], " ")
	t := tomlSection(string(doc), "pkg.rust.target."+target)
	if !versionRE.MatchString(version) || !Matches(version, want) || t["available"] != "true" || t["url"] == "" {
		return Artifact{}, errNoBuild("Rust", want, p)
	}
	return Artifact{Version: version, URL: t["url"], Digest: "sha256:" + strings.ToLower(t["hash"]), Format: "tar.gz",
		Source: "the Rust project's release channel (static.rust-lang.org)", Hosts: hosts}, nil
}

// adoptium names an architecture and an OS as the Adoptium API does.
func adoptium(p Platform) (arch, os string) {
	arch = map[string]string{"amd64": "x64", "arm64": "aarch64", "s390x": "s390x", "ppc64le": "ppc64le"}[p.Arch]
	os = map[string]string{"linux": "linux", "darwin": "mac"}[p.OS]
	return arch, os
}

// resolveJava asks the Adoptium API for Eclipse Temurin's newest JDK of a
// feature version (the newest LTS by default), with its sha256.
func (s *Source) resolveJava(ctx context.Context, want string, p Platform) (Artifact, error) {
	arch, osName := adoptium(p)
	if arch == "" || osName == "" {
		return Artifact{}, errNoBuild("Java", want, p)
	}
	base := s.Endpoints.AdoptiumAPI
	api := []string{hostOf(base)}
	feature, _, _ := strings.Cut(want, ".")
	if feature == "" {
		var info struct {
			LTS int `json:"most_recent_lts"`
		}
		if err := s.getJSON(ctx, base+"info/available_releases", api, &info); err != nil {
			return Artifact{}, err
		}
		if info.LTS <= 0 {
			return Artifact{}, errNoBuild("Java", want, p)
		}
		feature = strconv.Itoa(info.LTS)
	}
	q := url.Values{"architecture": {arch}, "image_type": {"jdk"}, "os": {osName}, "vendor": {"eclipse"}}
	var assets []struct {
		Binary struct {
			Package struct {
				Checksum string `json:"checksum"`
				Link     string `json:"link"`
				Name     string `json:"name"`
				Size     int64  `json:"size"`
			} `json:"package"`
		} `json:"binary"`
		Version struct {
			Major    int `json:"major"`
			Minor    int `json:"minor"`
			Security int `json:"security"`
		} `json:"version"`
	}
	if err := s.getJSON(ctx, base+"assets/latest/"+feature+"/hotspot?"+q.Encode(), api, &assets); err != nil {
		return Artifact{}, err
	}
	for _, a := range assets {
		v := fmt.Sprintf("%d.%d.%d", a.Version.Major, a.Version.Minor, a.Version.Security)
		pkg := a.Binary.Package
		if !Matches(v, want) || !strings.HasSuffix(pkg.Name, ".tar.gz") || pkg.Checksum == "" {
			continue
		}
		return Artifact{Version: v, URL: pkg.Link, Digest: "sha256:" + strings.ToLower(pkg.Checksum), Size: pkg.Size, Format: "tar.gz",
			Source: "Eclipse Temurin from the Adoptium API (api.adoptium.net)", Hosts: append([]string(nil), githubHosts...)}, nil
	}
	return Artifact{}, errNoBuild("Java", want, p)
}

// resolveMaven reads Maven Central's metadata for Apache Maven and the
// binary's published sha512. Maven runs on Java, so it is one build for
// every platform.
func (s *Source) resolveMaven(ctx context.Context, want string, p Platform) (Artifact, error) {
	base := s.Endpoints.MavenRepo + "org/apache/maven/apache-maven/"
	hosts := []string{hostOf(base)}
	data, err := s.get(ctx, base+"maven-metadata.xml", hosts)
	if err != nil {
		return Artifact{}, err
	}
	var meta struct {
		Versions []string `xml:"versioning>versions>version"`
	}
	if err := xml.Unmarshal(data, &meta); err != nil {
		return Artifact{}, fmt.Errorf("Maven's metadata cannot be read: %w", err)
	}
	v, ok := newest(meta.Versions, want)
	if !ok {
		return Artifact{}, errNoBuild("Maven", want, p)
	}
	file := base + v + "/apache-maven-" + v + "-bin.tar.gz"
	sumText, err := s.get(ctx, file+".sha512", hosts)
	if err != nil {
		return Artifact{}, err
	}
	sum := strings.Fields(string(sumText))
	if len(sum) == 0 || len(sum[0]) != 128 || !isHex(sum[0]) {
		return Artifact{}, fmt.Errorf("Maven %s's sha512 cannot be read", v)
	}
	return Artifact{Version: v, URL: file, Digest: "sha512:" + strings.ToLower(sum[0]), Format: "tar.gz",
		Source: "Apache Maven from Maven Central (repo.maven.apache.org)", Hosts: hosts}, nil
}

// resolveGradle reads services.gradle.org's list of versions, each with its
// distribution's sha256; only final releases count.
func (s *Source) resolveGradle(ctx context.Context, want string, p Platform) (Artifact, error) {
	base := s.Endpoints.GradleAPI
	api := []string{hostOf(base)}
	var all []struct {
		Version     string `json:"version"`
		DownloadURL string `json:"downloadUrl"`
		ChecksumURL string `json:"checksumUrl"`
		Checksum    string `json:"checksum"`
		Snapshot    bool   `json:"snapshot"`
		Nightly     bool   `json:"nightly"`
		RCFor       string `json:"rcFor"`
		MilestoneF  string `json:"milestoneFor"`
		Broken      bool   `json:"broken"`
	}
	if err := s.getJSON(ctx, base+"versions/all", api, &all); err != nil {
		return Artifact{}, err
	}
	hosts := []string{hostOf(base), "services.gradle.org", "downloads.gradle.org", "downloads.gradle-dn.com"}
	best := -1
	for i, r := range all {
		if r.Snapshot || r.Nightly || r.RCFor != "" || r.MilestoneF != "" || r.Broken || !versionRE.MatchString(r.Version) || !Matches(r.Version, want) {
			continue
		}
		if best < 0 || compareVersions(r.Version, all[best].Version) > 0 {
			best = i
		}
	}
	if best < 0 {
		return Artifact{}, errNoBuild("Gradle", want, p)
	}
	r := all[best]
	sum := strings.TrimSpace(r.Checksum)
	if sum == "" && r.ChecksumURL != "" {
		b, err := s.get(ctx, r.ChecksumURL, hosts)
		if err != nil {
			return Artifact{}, err
		}
		sum = strings.TrimSpace(string(b))
	}
	if len(sum) != 64 || !isHex(sum) {
		return Artifact{}, fmt.Errorf("Gradle %s has no sha256 Abhed can read", r.Version)
	}
	return Artifact{Version: r.Version, URL: r.DownloadURL, Digest: "sha256:" + strings.ToLower(sum), Format: "zip",
		Source: "Gradle from services.gradle.org", Hosts: hosts}, nil
}

// dotnetRID is .NET's runtime identifier for a platform.
func dotnetRID(p Platform) string {
	return map[string]string{"linux-amd64": "linux-x64", "linux-arm64": "linux-arm64", "darwin-amd64": "osx-x64", "darwin-arm64": "osx-arm64"}[p.String()]
}

// resolveDotnet reads the .NET release metadata: the newest supported
// channel by default, a channel (8.0) or an SDK version (8.0.100), and the
// SDK's published sha512.
func (s *Source) resolveDotnet(ctx context.Context, want string, p Platform) (Artifact, error) {
	rid := dotnetRID(p)
	if rid == "" {
		return Artifact{}, errNoBuild(".NET", want, p)
	}
	hosts := []string{hostOf(s.Endpoints.DotnetIndex), "builds.dotnet.microsoft.com", "dotnetcli.azureedge.net", "download.visualstudio.microsoft.com"}
	var idx struct {
		Index []struct {
			Channel  string `json:"channel-version"`
			Phase    string `json:"support-phase"`
			Releases string `json:"releases.json"`
		} `json:"releases-index"`
	}
	if err := s.getJSON(ctx, s.Endpoints.DotnetIndex, hosts, &idx); err != nil {
		return Artifact{}, err
	}
	parts := strings.Split(want, ".")
	channel := ""
	if len(parts) >= 2 {
		channel = parts[0] + "." + parts[1]
	} else if want != "" {
		channel = parts[0] + ".0"
	}
	relURL, chosen := "", ""
	for _, c := range idx.Index {
		supported := c.Phase == "active" || c.Phase == "maintenance"
		if channel == "" && supported && (chosen == "" || compareVersions(c.Channel, chosen) > 0) || channel != "" && c.Channel == channel {
			relURL, chosen = c.Releases, c.Channel
		}
	}
	if relURL == "" {
		return Artifact{}, errNoBuild(".NET", want, p)
	}
	type sdk struct {
		Version string `json:"version"`
		Files   []struct {
			Name string `json:"name"`
			RID  string `json:"rid"`
			URL  string `json:"url"`
			Hash string `json:"hash"`
		} `json:"files"`
	}
	var rel struct {
		Releases []struct {
			SDK  sdk   `json:"sdk"`
			SDKs []sdk `json:"sdks"`
		} `json:"releases"`
	}
	if err := s.getJSON(ctx, relURL, hosts, &rel); err != nil {
		return Artifact{}, err
	}
	var best Artifact
	for _, r := range rel.Releases {
		for _, k := range append([]sdk{r.SDK}, r.SDKs...) {
			if !versionRE.MatchString(k.Version) || len(parts) >= 3 && !Matches(k.Version, want) {
				continue
			}
			if best.Version != "" && compareVersions(k.Version, best.Version) <= 0 {
				continue
			}
			for _, f := range k.Files {
				if f.RID == rid && strings.HasSuffix(f.Name, ".tar.gz") && len(f.Hash) == 128 && isHex(f.Hash) {
					best = Artifact{Version: k.Version, URL: f.URL, Digest: "sha512:" + strings.ToLower(f.Hash), Format: "tar.gz",
						Source: "the .NET SDK from Microsoft's release metadata (builds.dotnet.microsoft.com)", Hosts: hosts}
				}
			}
		}
	}
	if best.Version == "" {
		return Artifact{}, errNoBuild(".NET", want, p)
	}
	return best, nil
}
