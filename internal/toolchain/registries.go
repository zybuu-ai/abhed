package toolchain

import (
	"fmt"
	"regexp"
	"strings"
)

// Registries are each language's official package registries, the hosts a
// person may open to the session's commands with its toolchain.
var Registries = map[string][]string{
	"go":     {"proxy.golang.org", "sum.golang.org"},
	"python": {"pypi.org", "files.pythonhosted.org"},
	"node":   {"registry.npmjs.org"},
	"rust":   {"index.crates.io", "static.crates.io", "crates.io"},
	"java":   {"repo.maven.apache.org", "repo1.maven.org"},
	"maven":  {"repo.maven.apache.org", "repo1.maven.org"},
	"gradle": {"plugins.gradle.org", "services.gradle.org", "downloads.gradle.org", "repo.maven.apache.org"},
	"ruby":   {"rubygems.org", "index.rubygems.org"},
	"dotnet": {"api.nuget.org"},
}

// AllRegistries are every language's registries, each once.
func AllRegistries() []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range Langs {
		for _, h := range Registries[l.Name] {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// registryCommands are the programs that fetch from each language's registry.
var registryCommands = []struct {
	lang string
	re   *regexp.Regexp
}{
	{"go", regexp.MustCompile(`(?:^|[;&|(\s])go\s+(?:get|install|mod|build|test|run|list|work)\b`)},
	{"python", regexp.MustCompile(`(?:^|[;&|(\s])(?:pip3?|uv|poetry|pipx)\b|python3?\s+-m\s+pip\b`)},
	{"node", regexp.MustCompile(`(?:^|[;&|(\s])(?:npm|npx|yarn|pnpm)\b`)},
	{"rust", regexp.MustCompile(`(?:^|[;&|(\s])cargo\b`)},
	{"ruby", regexp.MustCompile(`(?:^|[;&|(\s])(?:gem|bundle)\b`)},
	{"maven", regexp.MustCompile(`(?:^|[;&|(\s])mvnw?\b`)},
	{"gradle", regexp.MustCompile(`(?:^|[;&|(\s])(?:gradlew?|\./gradlew)\b`)},
	{"dotnet", regexp.MustCompile(`(?:^|[;&|(\s])dotnet\b`)},
}

// RegistryOf is the language whose package registry a refused command was
// after: one a registry host in its output or line names, else the program
// it ran.
func RegistryOf(command, output string) (string, bool) {
	for _, l := range Langs {
		for _, h := range Registries[l.Name] {
			if strings.Contains(output, h) || strings.Contains(command, h) {
				return l.Name, true
			}
		}
	}
	for _, rc := range registryCommands {
		if rc.re.MatchString(command) {
			return rc.lang, true
		}
	}
	return "", false
}

// RegistryNote is what a command refused a package registry is told: that
// it was the registry, closed to commands, and how to have it opened, or
// why it cannot be here.
func (s *Session) RegistryNote(command, output string) string {
	lang, ok := RegistryOf(command, output)
	if !ok || s.Off {
		return ""
	}
	l, _ := Find(lang)
	hosts := strings.Join(Registries[lang], ", ")
	if s.Registries == nil || s.Open == nil {
		return fmt.Sprintf("NOTE: the sandbox refused %s's package registry (%s): commands here cannot reach it. Tell the user; it cannot be opened in this session.", l.Display, hosts)
	}
	if _, why := s.Registries(lang); why != "" {
		return fmt.Sprintf("NOTE: the sandbox refused %s's package registry (%s): %s. Tell the user; retrying fails the same way.", l.Display, hosts, why)
	}
	return fmt.Sprintf("NOTE: the sandbox refused %s's package registry (%s), which is not open to commands yet; retrying fails the same way. "+
		"Call the toolchain tool with name %q: the user is asked, and agreeing opens %s's registries to commands; then run the command again.", l.Display, hosts, lang, l.Display)
}
