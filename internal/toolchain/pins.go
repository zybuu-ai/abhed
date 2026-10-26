package toolchain

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Pin is a version a project's files ask for.
type Pin struct {
	Spec Spec
	// File is the file that asks, relative to the workspace.
	File string
}

// maxPinFile bounds a pin file read.
const maxPinFile = 1 << 20

// readSmall reads a regular file of the workspace's top folder, or "".
func readSmall(workspace, name string) string {
	p := filepath.Join(workspace, name)
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(p) // #nosec G304 -- a named file of the workspace's top folder
	if err != nil {
		return ""
	}
	defer f.Close()
	data, _ := io.ReadAll(io.LimitReader(f, maxPinFile))
	return string(data)
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			return l
		}
	}
	return ""
}

var (
	goToolchainRE = regexp.MustCompile(`(?m)^toolchain\s+go([0-9.]+)\s*$`)
	goDirectiveRE = regexp.MustCompile(`(?m)^go\s+([0-9.]+)\s*$`)
	rustChannelRE = regexp.MustCompile(`(?m)^\s*channel\s*=\s*"([^"]+)"`)
)

// toolVersionsNames are .tool-versions' names for the languages.
var toolVersionsNames = map[string]string{
	"golang": "go", "go": "go", "python": "python", "nodejs": "node", "node": "node", "rust": "rust",
	"java": "java", "ruby": "ruby", "dotnet": "dotnet", "dotnet-core": "dotnet", "maven": "maven", "gradle": "gradle",
}

// Pins are the versions the workspace's top folder pins. A version that is
// not plain numbers (a pyenv name, a rust nightly, a custom toolchain path)
// is left out: a pin is untrusted text and only ever narrows a request.
func Pins(workspace string) map[string]Pin {
	out := map[string]Pin{}
	add := func(lang, version, file string) {
		if _, have := out[lang]; have {
			return
		}
		v, err := NormalizeVersion(lang, version)
		if err != nil || v == "" {
			return
		}
		out[lang] = Pin{Spec: Spec{Lang: lang, Version: v}, File: file}
	}
	if s := readSmall(workspace, "go.mod"); s != "" {
		if m := goToolchainRE.FindStringSubmatch(s); m != nil {
			add("go", m[1], "go.mod")
		} else if m := goDirectiveRE.FindStringSubmatch(s); m != nil {
			add("go", m[1], "go.mod")
		}
	}
	add("python", firstLine(readSmall(workspace, ".python-version")), ".python-version")
	add("node", firstLine(readSmall(workspace, ".nvmrc")), ".nvmrc")
	add("node", firstLine(readSmall(workspace, ".node-version")), ".node-version")
	if s := readSmall(workspace, "rust-toolchain.toml"); s != "" {
		if m := rustChannelRE.FindStringSubmatch(s); m != nil {
			add("rust", m[1], "rust-toolchain.toml")
		}
	}
	if s := firstLine(readSmall(workspace, "rust-toolchain")); s != "" && !strings.Contains(s, "[") {
		add("rust", s, "rust-toolchain")
	}
	add("java", firstLine(readSmall(workspace, ".java-version")), ".java-version")
	add("ruby", firstLine(readSmall(workspace, ".ruby-version")), ".ruby-version")
	if s := readSmall(workspace, "global.json"); s != "" {
		var g struct {
			SDK struct {
				Version string `json:"version"`
			} `json:"sdk"`
		}
		if json.Unmarshal([]byte(s), &g) == nil {
			add("dotnet", g.SDK.Version, "global.json")
		}
	}
	sc := bufio.NewScanner(strings.NewReader(readSmall(workspace, ".tool-versions")))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if lang, ok := toolVersionsNames[f[0]]; ok {
			v := f[1]
			if lang == "java" {
				// asdf names a vendor and version: temurin-21.0.4+7.0.LTS.
				v = strings.TrimPrefix(v, "temurin-")
				v, _, _ = strings.Cut(v, "+")
			}
			add(lang, v, ".tool-versions")
		}
	}
	return out
}

// pinFiles are the files Pins reads.
var pinFiles = []string{"go.mod", ".python-version", ".nvmrc", ".node-version", "rust-toolchain.toml", "rust-toolchain",
	".java-version", ".ruby-version", "global.json", ".tool-versions"}

// PinsFrom are the pins of the nearest folder at or above dir that has a
// pin file, as a command run in dir would find its project's.
func PinsFrom(dir string) map[string]Pin {
	d := filepath.Clean(dir)
	for i := 0; i < 32 && d != ""; i++ {
		for _, f := range pinFiles {
			if info, err := os.Lstat(filepath.Join(d, f)); err == nil && info.Mode().IsRegular() {
				return Pins(d)
			}
		}
		up := filepath.Dir(d)
		if up == d {
			break
		}
		d = up
	}
	return map[string]Pin{}
}

// Choose is the version to install: an explicit request, else the project's
// pin, else the latest stable.
func Choose(requested Spec, pins map[string]Pin) (Spec, string) {
	if requested.Version != "" {
		return requested, "requested"
	}
	if p, ok := pins[requested.Lang]; ok {
		return p.Spec, "pinned by " + p.File
	}
	return requested, "latest stable"
}
