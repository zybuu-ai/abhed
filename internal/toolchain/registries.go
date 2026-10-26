package toolchain

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
