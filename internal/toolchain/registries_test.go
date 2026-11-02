package toolchain

import (
	"strings"
	"testing"
)

// A refused command is matched to the registry it was after, by a host in
// its output or by the program it ran, and told how to have it opened.
func TestRegistryNote(t *testing.T) {
	for _, c := range []struct{ command, output, lang string }{
		{"go get github.com/x/y", `Get "https://proxy.golang.org/github.com/x/y/@v/list": Forbidden`, "go"},
		{"pip install pytest", "Tunnel connection failed: 403 Forbidden", "python"},
		{"curl https://registry.npmjs.org/left-pad", "curl: (56) CONNECT tunnel failed, response 403", "node"},
		{"cargo build", "failed to get `serde`", "rust"},
	} {
		if got, ok := RegistryOf(c.command, c.output); !ok || got != c.lang {
			t.Errorf("%q: %q %v", c.command, got, ok)
		}
	}
	if _, ok := RegistryOf("curl https://example.com", "403"); ok {
		t.Error("a host that is no registry was taken for one")
	}
	s := &Session{Registries: func(string) ([]string, string) { return []string{"pypi.org"}, "" }, Open: func(string) ([]string, error) { return nil, nil }}
	if n := s.RegistryNote("pip install pytest", "403 Forbidden"); !strings.Contains(n, `name "python"`) || !strings.Contains(n, "pypi.org") {
		t.Fatalf("note %q", n)
	}
	s.Registries = func(string) ([]string, string) { return nil, "sandbox.toolchains_registries is off" }
	if n := s.RegistryNote("pip install pytest", "403 Forbidden"); !strings.Contains(n, "toolchains_registries is off") {
		t.Fatalf("note %q", n)
	}
}
