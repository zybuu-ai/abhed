package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// A Go and Node workspace on the default image is told which toolchains the
// image lacks and how to name another; one with no markers is told nothing.
func TestDoctorToolchainWarnings(t *testing.T) {
	t.Setenv("ABHED_SANDBOX_IMAGE", "")
	sandbox.SetConfiguredImage("")
	ws := t.TempDir()
	c := sandbox.NewContainer(sandbox.DefaultPolicy(ws))
	if got := toolchainWarnings(c, ws); got != nil {
		t.Fatalf("no project: %v", got)
	}
	for _, f := range []string{"go.mod", "package.json"} {
		if err := os.WriteFile(filepath.Join(ws, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := toolchainWarnings(c, ws)
	if len(got) != 1 {
		t.Fatalf("warnings %v", got)
	}
	for _, want := range []string{"Go and Node project", "has no go and node", "sandbox.image", "an image of your own"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("missing %q in %s", want, got[0])
		}
	}
	if andList([]string{"a", "b", "c"}) != "a, b and c" {
		t.Fatal(andList([]string{"a", "b", "c"}))
	}
}

// The doctor's toolchains lines say what the workspace needs, by loose files
// too, and whether the cache has it for the platform commands run on.
func TestDoctorToolchainLines(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "hello.py"), []byte("print(1)"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(toolchainLines(config.Default(), ws, nil), "\n")
	for _, want := range []string{"installs ask", "Python · not installed, needed by *.py files", "abhed toolchain install python"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}
