package toolchain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Artifact is one toolchain build: where it comes from, the checksum its
// project publishes for it, and how it is packed.
type Artifact struct {
	Lang     string   `json:"lang"`
	Version  string   `json:"version"`
	Platform Platform `json:"-"`
	URL      string   `json:"url"`
	// Digest is "sha256:<hex>" or "sha512:<hex>", as the project publishes it.
	Digest string `json:"digest"`
	// Size is the download's size in bytes, when the source says it.
	Size int64 `json:"size,omitempty"`
	// Format is tar.gz or zip.
	Format string `json:"format"`
	// Source names the release source for a person.
	Source string `json:"source"`
	// Hosts are the hosts the download may come from, redirects included.
	Hosts []string `json:"-"`
}

// Endpoints are the official release sources, by language. A test points
// them at a fake; nothing else changes them.
type Endpoints struct {
	GoDL         string // the Go release list (go.dev/dl)
	NodeDist     string // nodejs.org/dist
	PythonAPI    string // the python-build-standalone releases API
	RustDist     string // static.rust-lang.org/dist
	AdoptiumAPI  string // api.adoptium.net
	MavenRepo    string // repo.maven.apache.org/maven2
	GradleAPI    string // services.gradle.org
	DotnetIndex  string // the .NET release metadata
	RubyDownload string // the ruby-builder release downloads
	GoProxy      string // the Go module proxy Delve is built from; "" is proxy.golang.org
	GoSumDB      string // the checksum database that checks it; "" is sum.golang.org
	PyPIAPI      string // PyPI's JSON API, for debugpy; "" is pypi.org/pypi/
	// Extra hosts a download may reach, for a test's fake.
	ExtraHosts []string
}

// Official are the projects' own release sources.
var Official = Endpoints{
	GoDL:         "https://go.dev/dl/",
	NodeDist:     "https://nodejs.org/dist/",
	PythonAPI:    "https://api.github.com/repos/astral-sh/python-build-standalone/releases",
	RustDist:     "https://static.rust-lang.org/dist/",
	AdoptiumAPI:  "https://api.adoptium.net/v3/",
	MavenRepo:    "https://repo.maven.apache.org/maven2/",
	GradleAPI:    "https://services.gradle.org/",
	DotnetIndex:  "https://builds.dotnet.microsoft.com/dotnet/release-metadata/releases-index.json",
	RubyDownload: "https://github.com/ruby/ruby-builder/releases/download/toolcache/",
}

// githubHosts are where a GitHub release download is served from.
var githubHosts = []string{"github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"}

// Source finds and fetches toolchains from the official sources.
type Source struct {
	Endpoints Endpoints
	// Client makes every request; nil uses a client with timeouts.
	Client *http.Client
	// UserAgent names Abhed to the sources.
	UserAgent string
}

// maxManifest bounds a release list or checksum file.
const maxManifest = 64 << 20

func (s *Source) client(hosts []string) *http.Client {
	base := s.Client
	if base == nil {
		base = &http.Client{Timeout: 30 * time.Minute}
	}
	c := *base
	allowed := append(append([]string(nil), hosts...), s.Endpoints.ExtraHosts...)
	// A redirect may only move within the language's own release hosts.
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if !hostAllowed(req.URL, allowed) {
			return fmt.Errorf("refused a redirect to %s, which is not an official release host", req.URL.Host)
		}
		return nil
	}
	return &c
}

func hostAllowed(u *url.URL, hosts []string) bool {
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())) {
		return false
	}
	h := u.Host
	for _, a := range hosts {
		if strings.EqualFold(h, a) || strings.EqualFold(u.Hostname(), a) {
			return true
		}
	}
	return false
}

func isLoopback(h string) bool { return h == "127.0.0.1" || h == "localhost" || h == "::1" }

// get fetches a manifest from one of hosts.
func (s *Source) get(ctx context.Context, raw string, hosts []string) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if !hostAllowed(u, append(append([]string(nil), hosts...), s.Endpoints.ExtraHosts...)) {
		return nil, fmt.Errorf("%s is not an official release host", u.Host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	if s.UserAgent != "" {
		req.Header.Set("User-Agent", s.UserAgent)
	}
	resp, err := s.client(hosts).Do(req) // #nosec G704 -- an official release host, checked above and on every redirect
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", u.Host, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifest+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxManifest {
		return nil, fmt.Errorf("the list from %s is larger than %d bytes", u.Host, maxManifest)
	}
	return data, nil
}

func (s *Source) getJSON(ctx context.Context, raw string, hosts []string, v any) error {
	data, err := s.get(ctx, raw, hosts)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("the list from %s cannot be read: %w", raw, err)
	}
	return nil
}

// hostOf is a URL's host, for an allowlist.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// Resolve finds the build a spec names for platform: the newest version
// matching it, from the language's official source.
func (s *Source) Resolve(ctx context.Context, spec Spec, p Platform) (Artifact, error) {
	var a Artifact
	var err error
	switch spec.Lang {
	case "go":
		a, err = s.resolveGo(ctx, spec.Version, p)
	case "node":
		a, err = s.resolveNode(ctx, spec.Version, p)
	case "python":
		a, err = s.resolvePython(ctx, spec.Version, p)
	default:
		if r, ok := moreResolvers[spec.Lang]; ok {
			a, err = r(s, ctx, spec.Version, p)
		} else {
			err = fmt.Errorf("no source for %s", spec.Lang)
		}
	}
	if err != nil {
		return Artifact{}, err
	}
	a.Lang, a.Platform = spec.Lang, p
	if !strings.HasPrefix(a.Digest, "sha256:") && !strings.HasPrefix(a.Digest, "sha512:") {
		return Artifact{}, fmt.Errorf("%s %s has no published checksum Abhed can check", spec.Lang, a.Version)
	}
	return a, nil
}

// moreResolvers are the languages beyond the first three.
var moreResolvers = map[string]func(*Source, context.Context, string, Platform) (Artifact, error){}

// errNoBuild is a version with no build for a platform.
func errNoBuild(lang, want string, p Platform) error {
	if want == "" {
		return fmt.Errorf("no %s build for %s was found", lang, p)
	}
	return fmt.Errorf("no %s %s build for %s was found", lang, want, p)
}
