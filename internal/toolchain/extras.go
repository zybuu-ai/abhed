package toolchain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/filelock"
)

// Extra is a tool installed beside a language's toolchain on request, as
// part of the same agreement: today, the language's debugger.
type Extra struct {
	Name    string
	Lang    string
	Display string
	Version string
}

// The pinned debuggers. Delve is built from its module, checked by the Go
// checksum database; debugpy's wheel is pinned by its sha256 as PyPI lists it.
const (
	delveModule      = "github.com/go-delve/delve"
	delveVersion     = "1.27.2"
	delveMinGo       = "1.25"
	debugpyVersion   = "1.8.22"
	debugpyWheel     = "debugpy-1.8.22-py2.py3-none-any.whl"
	debugpyMinPython = "3.10"
)

// debugpyWheelSHA256 is the pinned wheel's sha256; tests set it.
var debugpyWheelSHA256 = "a9e9d3550e15ca479c59333e90845029190531f0cacfedab3b815a57bd913947"

// Extras are the extras Abhed installs.
var Extras = []Extra{
	{Name: "delve", Lang: "go", Display: "Delve debugger", Version: delveVersion},
	{Name: "debugpy", Lang: "python", Display: "debugpy debugger", Version: debugpyVersion},
}

// DebuggerFor is the debugger installed with lang, if Abhed installs one.
func DebuggerFor(lang string) (Extra, bool) {
	for _, e := range Extras {
		if e.Lang == lang {
			return e, true
		}
	}
	return Extra{}, false
}

// extraMarker records what an installed extra holds.
const extraMarker = ".abhed-extra.json"

// ExtraInstalled is one extra in the cache.
type ExtraInstalled struct {
	Name        string    `json:"name"`
	Lang        string    `json:"lang"`
	Version     string    `json:"version"`
	Platform    string    `json:"platform"`
	Source      string    `json:"source"`
	URL         string    `json:"url"`
	Digest      string    `json:"digest"`
	BuiltWith   string    `json:"built_with,omitempty"`
	Bytes       int64     `json:"bytes"`
	InstalledAt time.Time `json:"installed_at"`
	Dir         string    `json:"-"`
}

// ExtraDir is where an extra is, or would be.
func (c Cache) ExtraDir(p Platform, name, version string) string {
	return filepath.Join(c.Root, p.String(), "extras", name, version)
}

// LookupExtra reports the pinned version of e, installed for p.
func (c Cache) LookupExtra(p Platform, e Extra) (ExtraInstalled, bool) {
	dir := c.ExtraDir(p, e.Name, e.Version)
	data, err := os.ReadFile(filepath.Join(dir, extraMarker)) // #nosec G304 -- a directory of the toolchain cache
	if err != nil {
		return ExtraInstalled{}, false
	}
	var x ExtraInstalled
	if json.Unmarshal(data, &x) != nil || x.Name != e.Name || x.Version != e.Version {
		return ExtraInstalled{}, false
	}
	x.Dir = dir
	return x, true
}

// ExtraEnv is where an installed extra's programs are, for PATH, and the
// folder Python imports it from, for PYTHONPATH.
func ExtraEnv(x ExtraInstalled) (bin []string, pythonPath string) {
	switch x.Name {
	case "delve":
		return []string{filepath.Join(x.Dir, "bin")}, ""
	case "debugpy":
		return nil, filepath.Join(x.Dir, "site")
	}
	return nil, ""
}

// ExtraPlan is what installing an extra would do.
type ExtraPlan struct {
	Extra     Extra           `json:"extra"`
	Installed *ExtraInstalled `json:"installed,omitempty"`
	// Source, URL and Digest name where it comes from and what checks it.
	Source string `json:"source"`
	URL    string `json:"url"`
	Digest string `json:"digest"`
	Size   int64  `json:"size,omitempty"`
	// Builder is the Go toolchain for this host Delve is built with, when
	// it is not the session's own; it is installed first if not cached.
	Builder *Plan `json:"builder,omitempty"`
	// Note says how it is made, for a person.
	Note string `json:"note,omitempty"`
}

// PlanExtra is what installing e beside the language plan lang would do,
// or why it cannot be installed there.
func (s *Session) PlanExtra(ctx context.Context, e Extra, lang Plan) (ExtraPlan, error) {
	if s.Off {
		return ExtraPlan{}, ErrOff
	}
	p := s.platform()
	ep := ExtraPlan{Extra: e}
	if x, ok := s.Cache.LookupExtra(p, e); ok {
		ep.Installed, ep.Source, ep.URL, ep.Digest = &x, x.Source, x.URL, x.Digest
		return ep, nil
	}
	switch e.Name {
	case "delve":
		if p.OS != "linux" || p.Arch != "amd64" && p.Arch != "arm64" {
			return ExtraPlan{}, fmt.Errorf("the Delve debugger is installed only for Linux sandboxes on amd64 or arm64, where the debugger runs, not %s", p)
		}
		if compareVersions(lang.Version(), delveMinGo) < 0 {
			return ExtraPlan{}, fmt.Errorf("the Delve debugger %s is built with Go %s or later; ask for go@%s or later to debug", delveVersion, delveMinGo, delveMinGo)
		}
		ep.Source = "proxy.golang.org, checked by sum.golang.org"
		ep.URL = strings.TrimSuffix(s.Source.Endpoints.goProxy(), "/") + "/" + delveModule + "/@v/v" + delveVersion + ".zip"
		ep.Digest = "go.sum, checked by " + s.Source.Endpoints.goSumDB()
		ep.Note = "built here with Go " + lang.Version() + " for " + p.String()
		if h := Host(); h != p {
			b, err := s.planFor(ctx, Spec{Lang: "go", Version: lang.Version()}, h)
			if err != nil {
				return ExtraPlan{}, fmt.Errorf("the Delve debugger is built with Go %s for this host (%s): %w", lang.Version(), h, err)
			}
			if b.Installed == nil {
				ep.Note += fmt.Sprintf(", which first installs Go %s for this host (%s) to build it", b.Version(), h)
			}
			ep.Builder = &b
		}
	case "debugpy":
		if compareVersions(lang.Version(), debugpyMinPython) < 0 {
			return ExtraPlan{}, fmt.Errorf("debugpy %s needs Python %s or later", debugpyVersion, debugpyMinPython)
		}
		a, err := s.Source.resolveDebugpy(ctx)
		if err != nil {
			return ExtraPlan{}, err
		}
		ep.Source, ep.URL, ep.Digest, ep.Size = a.Source, a.URL, a.Digest, a.Size
	default:
		return ExtraPlan{}, fmt.Errorf("no source for %s", e.Name)
	}
	return ep, nil
}

// planFor is Plan for an exact spec on another platform, with no pins.
func (s *Session) planFor(ctx context.Context, spec Spec, p Platform) (Plan, error) {
	if in, ok := s.Cache.Lookup(p, spec.Lang, spec.Version); ok {
		return Plan{Spec: spec, Why: "builds the debugger", Installed: &in}, nil
	}
	a, err := s.Source.Resolve(ctx, spec, p)
	if err != nil {
		return Plan{}, err
	}
	if in, ok := s.Cache.Lookup(p, spec.Lang, a.Version); ok {
		return Plan{Spec: spec, Why: "builds the debugger", Artifact: a, Installed: &in}, nil
	}
	return Plan{Spec: spec, Why: "builds the debugger", Artifact: a}, nil
}

// InstallExtra carries out an extra's plan beside the installed language
// lang, recording how it ended.
func (s *Session) InstallExtra(ctx context.Context, ep ExtraPlan, lang Installed, by, surface, consent string, progress Progress) (ExtraInstalled, error) {
	p := s.platform()
	ev := agent.ToolchainAction{Lang: ep.Extra.Lang, Extra: ep.Extra.Name, Version: ep.Extra.Version, Platform: p.String(),
		Source: ep.Source, URL: ep.URL, Digest: ep.Digest, Bytes: ep.Size, Consent: consent, By: by, Surface: surface}
	record := func(outcome string, err error) {
		ev.Outcome = outcome
		if err != nil {
			ev.Error = err.Error()
		}
		if s.Record != nil {
			s.Record(ev)
		}
	}
	if s.Off {
		record(agent.ToolchainRefused, ErrOff)
		return ExtraInstalled{}, ErrOff
	}
	if ep.Installed != nil {
		ev.Cached, ev.Digest, ev.Bytes, ev.BuiltWith = true, ep.Installed.Digest, ep.Installed.Bytes, ep.Installed.BuiltWith
		record(agent.ToolchainOK, nil)
		return *ep.Installed, nil
	}
	var x ExtraInstalled
	var err error
	switch ep.Extra.Name {
	case "delve":
		goroot := lang.Dir
		if ep.Builder != nil && ep.Builder.Installed != nil {
			goroot = ep.Builder.Installed.Dir
		} else if ep.Builder != nil {
			var b Installed
			if b, err = s.installFor(ctx, *ep.Builder, Host(), by, surface, consent, progress); err == nil {
				goroot = b.Dir
			}
		}
		if err == nil {
			ev.BuiltWith = "go " + lang.Version
			x, err = s.Cache.installDelve(ctx, s.Source.Endpoints, goroot, lang.Version, p)
		}
	case "debugpy":
		var a Artifact
		if a, err = s.Source.resolveDebugpy(ctx); err == nil {
			x, err = s.Cache.installDebugpy(ctx, s.Source, a, p, progress)
		}
	default:
		err = fmt.Errorf("no source for %s", ep.Extra.Name)
	}
	switch {
	case err != nil && ctx.Err() != nil:
		record(agent.ToolchainCancelled, err)
		return ExtraInstalled{}, err
	case err != nil:
		record(agent.ToolchainFailed, err)
		return ExtraInstalled{}, err
	}
	ev.Digest, ev.Bytes = x.Digest, x.Bytes
	record(agent.ToolchainOK, nil)
	return x, nil
}

// installFor installs a plan for another platform, the host's builder,
// recording it as its own install.
func (s *Session) installFor(ctx context.Context, plan Plan, p Platform, by, surface, consent string, progress Progress) (Installed, error) {
	other := *s
	other.Platform = func() Platform { return p }
	return other.Install(ctx, plan, by, surface, consent, progress)
}

// goProxy and goSumDB are where Delve's module comes from and what checks it.
func (e Endpoints) goProxy() string {
	if e.GoProxy != "" {
		return e.GoProxy
	}
	return "https://proxy.golang.org"
}

func (e Endpoints) goSumDB() string {
	if e.GoSumDB != "" {
		return e.GoSumDB
	}
	return "sum.golang.org"
}

// goBuild runs the go command; tests replace it.
var goBuild = func(ctx context.Context, goBin string, env []string, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, goBin, args...) // #nosec G204 -- the cache's own Go, with fixed arguments
	cmd.Env, cmd.Dir = env, dir
	return cmd.CombinedOutput()
}

// delveBuildTimeout bounds building Delve.
const delveBuildTimeout = 15 * time.Minute

// installDelve builds the pinned Delve with the Go at goroot for p, from the
// module proxy with every module checked by the checksum database, and
// places it read-only. Nothing from the person's own Go setup is used.
func (c Cache) installDelve(ctx context.Context, ep Endpoints, goroot, goVersion string, p Platform) (ExtraInstalled, error) {
	e, _ := DebuggerFor("go")
	return c.placeExtra(e, p, func(tmp, top string) (ExtraInstalled, error) {
		ctx, cancel := context.WithTimeout(ctx, delveBuildTimeout)
		defer cancel()
		gopath := filepath.Join(tmp, "gopath")
		env := []string{
			"PATH=" + filepath.Join(goroot, "bin") + string(os.PathListSeparator) + "/usr/bin:/bin",
			"HOME=" + tmp, "GOROOT=" + goroot, "GOPATH=" + gopath, "GOMODCACHE=" + filepath.Join(gopath, "pkg", "mod"),
			"GOCACHE=" + filepath.Join(tmp, "gocache"), "GOENV=off", "GOTOOLCHAIN=local", "GOTELEMETRY=off",
			"GOPROXY=" + ep.goProxy(), "GOSUMDB=" + ep.goSumDB(), "GONOSUMDB=", "GONOSUMCHECK=", "GOPRIVATE=", "GONOPROXY=",
			"GOINSECURE=", "GOFLAGS=-mod=mod", "CGO_ENABLED=0", "GOOS=" + p.OS, "GOARCH=" + p.Arch,
		}
		out, err := goBuild(ctx, filepath.Join(goroot, "bin", "go"), env, tmp,
			"install", "-trimpath", delveModule+"/cmd/dlv@v"+delveVersion)
		if err != nil {
			return ExtraInstalled{}, fmt.Errorf("building Delve %s: %w: %s", delveVersion, err, lastLine(string(out)))
		}
		built := filepath.Join(gopath, "bin", "dlv")
		if h := Host(); h != p {
			built = filepath.Join(gopath, "bin", p.OS+"_"+p.Arch, "dlv")
		}
		if err := os.MkdirAll(filepath.Join(top, "bin"), 0o755); err != nil {
			return ExtraInstalled{}, err
		}
		digest, n, err := copyHashed(built, filepath.Join(top, "bin", "dlv"))
		if err != nil {
			return ExtraInstalled{}, fmt.Errorf("the Delve build is not where expected: %w", err)
		}
		return ExtraInstalled{Source: "proxy.golang.org, checked by sum.golang.org",
			URL:    strings.TrimSuffix(ep.goProxy(), "/") + "/" + delveModule + "/@v/v" + delveVersion + ".zip",
			Digest: "sha256:" + digest, BuiltWith: "go " + goVersion, Bytes: n}, nil
	})
}

// resolveDebugpy finds the pinned debugpy wheel in PyPI's listing and
// refuses it unless the listing's sha256 is the pinned one.
func (s *Source) resolveDebugpy(ctx context.Context) (Artifact, error) {
	api := s.Endpoints.PyPIAPI
	if api == "" {
		api = "https://pypi.org/pypi/"
	}
	var rel struct {
		URLs []struct {
			Filename string            `json:"filename"`
			URL      string            `json:"url"`
			Size     int64             `json:"size"`
			Digests  map[string]string `json:"digests"`
		} `json:"urls"`
	}
	if err := s.getJSON(ctx, api+"debugpy/"+debugpyVersion+"/json", []string{hostOf(api)}, &rel); err != nil {
		return Artifact{}, err
	}
	for _, u := range rel.URLs {
		if u.Filename != debugpyWheel {
			continue
		}
		if !strings.EqualFold(u.Digests["sha256"], debugpyWheelSHA256) {
			return Artifact{}, fmt.Errorf("PyPI lists %s with a sha256 that is not the one Abhed pins, so it is not installed", debugpyWheel)
		}
		return Artifact{Lang: "python", Version: debugpyVersion, URL: u.URL, Digest: "sha256:" + debugpyWheelSHA256, Size: u.Size,
			Format: "zip", Source: "PyPI (files.pythonhosted.org)", Hosts: []string{"files.pythonhosted.org"}}, nil
	}
	return Artifact{}, fmt.Errorf("PyPI does not list %s", debugpyWheel)
}

// installDebugpy downloads the pinned wheel, checks it, and unpacks it
// read-only where PYTHONPATH finds it.
func (c Cache) installDebugpy(ctx context.Context, src *Source, a Artifact, p Platform, progress Progress) (ExtraInstalled, error) {
	e, _ := DebuggerFor("python")
	return c.placeExtra(e, p, func(tmp, top string) (ExtraInstalled, error) {
		wheel := filepath.Join(tmp, "download")
		n, err := src.download(ctx, a, wheel, progress)
		if err != nil {
			return ExtraInstalled{}, err
		}
		if err := unpack(wheel, "zip", filepath.Join(top, "site")); err != nil {
			return ExtraInstalled{}, fmt.Errorf("unpack %s: %w", debugpyWheel, err)
		}
		return ExtraInstalled{Source: a.Source, URL: a.URL, Digest: a.Digest, Bytes: n}, nil
	})
}

// placeExtra makes an extra in a temporary folder, then moves it in place
// read-only with its marker; nothing partial is left where it belongs.
func (c Cache) placeExtra(e Extra, p Platform, build func(tmp, top string) (ExtraInstalled, error)) (ExtraInstalled, error) {
	dir := c.ExtraDir(p, e.Name, e.Version)
	if x, ok := c.LookupExtra(p, e); ok {
		return x, nil
	}
	if err := c.Prepare(); err != nil {
		return ExtraInstalled{}, err
	}
	if err := os.MkdirAll(filepath.Join(c.Root, ".partial"), 0o700); err != nil {
		return ExtraInstalled{}, err
	}
	unlock, err := filelock.Lock(filepath.Join(c.Root, ".partial", p.String()+"-"+e.Name+"-"+e.Version+".lock"),
		30*time.Minute, "another install holds %s (waited %s)")
	if err != nil {
		return ExtraInstalled{}, err
	}
	defer unlock()
	if x, ok := c.LookupExtra(p, e); ok {
		return x, nil
	}
	tmp, err := os.MkdirTemp(filepath.Join(c.Root, ".partial"), e.Name+"-")
	if err != nil {
		return ExtraInstalled{}, err
	}
	defer func() { _ = removeTree(tmp) }()
	top := filepath.Join(tmp, "extra")
	if err := os.Mkdir(top, 0o755); err != nil { // #nosec G301 -- read by the sandbox's commands
		return ExtraInstalled{}, err
	}
	x, err := build(tmp, top)
	if err != nil {
		return ExtraInstalled{}, err
	}
	x.Name, x.Lang, x.Version, x.Platform, x.InstalledAt = e.Name, e.Lang, e.Version, p.String(), time.Now().UTC()
	marker, _ := json.MarshalIndent(x, "", "  ")
	if err := os.WriteFile(filepath.Join(top, extraMarker), marker, 0o444); err != nil { // #nosec G306 -- read by the sandbox's commands
		return ExtraInstalled{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return ExtraInstalled{}, err
	}
	if err := os.Rename(top, dir); err != nil {
		if got, ok := c.LookupExtra(p, e); ok {
			return got, nil
		}
		return ExtraInstalled{}, err
	}
	if err := readOnly(dir); err != nil {
		return ExtraInstalled{}, err
	}
	x.Dir = dir
	return x, nil
}

// copyHashed copies an executable and returns its sha256 and size.
func copyHashed(src, dst string) (string, int64, error) {
	in, err := os.Open(src) // #nosec G304 -- the build's own output
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755) // #nosec G302 G304 -- an executable for the sandbox
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	if n == 0 {
		return "", 0, errors.New("the build is empty")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// lastLine is the last non-empty line of s, for an error.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
