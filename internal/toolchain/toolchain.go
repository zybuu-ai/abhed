// Package toolchain installs language toolchains on demand: Go, Python,
// Node, Rust, Java with Maven and Gradle, Ruby and .NET. Each comes from its
// project's official release source, is verified against the checksum that
// source publishes, and is unpacked into a per-user cache, one directory per
// platform, language and version, that commands may read but not change.
// Nothing is installed system-wide, and nothing runs as another user.
package toolchain

import (
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Platform is an operating system and architecture, as Go names them.
type Platform struct{ OS, Arch string }

func (p Platform) String() string { return p.OS + "-" + p.Arch }

// Host is the platform this process runs on.
func Host() Platform { return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH} }

// Lang is a language Abhed can install a toolchain for.
type Lang struct {
	// Name is how a request names it: go, python, node, rust, java, maven,
	// gradle, ruby or dotnet.
	Name string
	// Display is its name for a person.
	Display string
	// Programs are the commands it provides; a command not found among them
	// is offered this language.
	Programs []string
	// Aliases are other names a request may use.
	Aliases []string
}

// Langs are the languages, in the order lists show them.
var Langs = []Lang{
	{Name: "go", Display: "Go", Programs: []string{"go", "gofmt"}, Aliases: []string{"golang"}},
	{Name: "python", Display: "Python", Programs: []string{"python3", "python", "pip3", "pip"}, Aliases: []string{"py", "cpython"}},
	{Name: "node", Display: "Node.js", Programs: []string{"node", "npm", "npx", "corepack"}, Aliases: []string{"nodejs", "node.js"}},
	{Name: "rust", Display: "Rust", Programs: []string{"cargo", "rustc", "rustdoc", "rustfmt"}, Aliases: []string{"cargo"}},
	{Name: "java", Display: "Java (Eclipse Temurin)", Programs: []string{"java", "javac", "jar", "jshell"}, Aliases: []string{"jdk", "temurin", "openjdk"}},
	{Name: "maven", Display: "Apache Maven", Programs: []string{"mvn"}, Aliases: []string{"mvn"}},
	{Name: "gradle", Display: "Gradle", Programs: []string{"gradle"}},
	{Name: "ruby", Display: "Ruby", Programs: []string{"ruby", "gem", "bundle", "bundler", "irb"}},
	{Name: "dotnet", Display: ".NET SDK", Programs: []string{"dotnet"}, Aliases: []string{".net", "csharp", "c#"}},
}

// Find is the language a name or alias means.
func Find(name string) (Lang, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, l := range Langs {
		if l.Name == name {
			return l, true
		}
		for _, a := range l.Aliases {
			if a == name {
				return l, true
			}
		}
	}
	return Lang{}, false
}

// ForProgram is the language a command's program belongs to.
func ForProgram(program string) (Lang, bool) {
	for _, l := range Langs {
		for _, p := range l.Programs {
			if p == program {
				return l, true
			}
		}
	}
	return Lang{}, false
}

// Spec is a request for a toolchain: a language and, optionally, a version
// prefix such as 1.22, 3.11 or 21. Empty is the latest stable.
type Spec struct {
	Lang    string
	Version string
}

func (s Spec) String() string {
	if s.Version == "" {
		return s.Lang
	}
	return s.Lang + "@" + s.Version
}

var versionRE = regexp.MustCompile(`^[0-9]{1,4}(\.[0-9]{1,4}){0,3}$`)

// ParseSpec reads "go", "go@1.22", "python 3.11" or "Python 3.11".
func ParseSpec(s string) (Spec, error) {
	s = strings.TrimSpace(s)
	name, ver, ok := strings.Cut(s, "@")
	if !ok {
		name, ver, _ = strings.Cut(s, " ")
	}
	l, found := Find(name)
	if !found {
		return Spec{}, fmt.Errorf("%q is not a language Abhed installs (it installs %s)", name, Names())
	}
	v, err := NormalizeVersion(l.Name, ver)
	if err != nil {
		return Spec{}, err
	}
	return Spec{Lang: l.Name, Version: v}, nil
}

// Names lists the languages' names.
func Names() string {
	var n []string
	for _, l := range Langs {
		n = append(n, l.Name)
	}
	return strings.Join(n, ", ")
}

// NormalizeVersion turns what a person or a project file wrote into a
// numeric version prefix, or "" for the latest stable, refusing anything
// else: a version is untrusted text that ends up in a path and a URL.
func NormalizeVersion(lang, v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	for _, p := range []string{"go", "python", "ruby-", "v", "temurin-", "jdk-"} {
		v = strings.TrimPrefix(v, p)
	}
	switch v {
	case "", "latest", "stable", "lts", "lts/*", "node", "current":
		return "", nil
	}
	if !versionRE.MatchString(v) {
		return "", fmt.Errorf("%q is not a %s version Abhed understands; give numbers such as 1.22 or 3.11.4", v, lang)
	}
	return v, nil
}

// Matches reports whether version satisfies the prefix want: "1.22" matches
// 1.22 and 1.22.5, not 1.220.
func Matches(version, want string) bool {
	return want == "" || version == want || strings.HasPrefix(version, want+".")
}

// Major is a version's first number, or its first two for languages whose
// majors move slowly (go 1.22, python 3.11, rust 1.82): the unit consent is
// asked for.
func Major(lang, version string) string {
	parts := strings.Split(version, ".")
	switch lang {
	case "go", "python", "ruby", "dotnet", "maven", "rust":
		if len(parts) >= 2 {
			return parts[0] + "." + parts[1]
		}
	}
	return parts[0]
}

// compareVersions orders numeric versions: -1, 0 or 1.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

// newest picks the highest of versions matching want.
func newest(versions []string, want string) (string, bool) {
	var ok []string
	for _, v := range versions {
		if versionRE.MatchString(v) && Matches(v, want) {
			ok = append(ok, v)
		}
	}
	if len(ok) == 0 {
		return "", false
	}
	sort.Slice(ok, func(i, j int) bool { return compareVersions(ok[i], ok[j]) > 0 })
	return ok[0], true
}
