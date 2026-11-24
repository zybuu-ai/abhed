package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

func answers(t *testing.T, input string) (*prompter, *strings.Builder) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in")
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	var out strings.Builder
	return newPrompter(f, &out), &out
}

// An interactive serve with no sign-in on a reachable address asks first;
// one on loopback, with sign-in, or under a supervisor does not.
func TestServeAsksBeforeServingWithoutSignIn(t *testing.T) {
	none := config.Default()
	local := config.Default()
	local.Auth.Mode = "local"

	p, out := answers(t, "1\n")
	if _, ok := serveSignInChoice(none, ":8080", true, p); ok || !strings.Contains(out.String(), "abhed user add") {
		t.Fatalf("set up first: ok %v\n%s", ok, out)
	}
	p, _ = answers(t, "2\n")
	if addr, ok := serveSignInChoice(none, ":8080", true, p); !ok || addr != "127.0.0.1:8080" {
		t.Fatalf("this machine only: %q %v", addr, ok)
	}
	p, _ = answers(t, "3\n")
	if addr, ok := serveSignInChoice(none, "0.0.0.0:9000", true, p); !ok || addr != "0.0.0.0:9000" {
		t.Fatalf("as configured: %q %v", addr, ok)
	}
	for name, c := range map[string]struct {
		cfg         config.Config
		addr        string
		interactive bool
	}{
		"loopback":     {none, "127.0.0.1:8080", true},
		"with sign-in": {local, ":8080", true},
		"supervised":   {none, ":8080", false},
	} {
		p, out := answers(t, "")
		if addr, ok := serveSignInChoice(c.cfg, c.addr, c.interactive, p); !ok || addr != c.addr || out.Len() != 0 {
			t.Errorf("%s: asked or changed: %q %v %q", name, addr, ok, out)
		}
	}
}
