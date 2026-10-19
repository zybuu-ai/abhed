package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
