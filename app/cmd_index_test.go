package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// Without embeddings `abhed index` keeps nothing, and says so rather than
// claim to have built what the next session uses.
func TestIndexSaysWhatItKeeps(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n\nfunc Hello() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := stdoutOf(t, func() int { return buildIndexCmd(ws, config.TrustRefused) })
	if code != 0 || !strings.Contains(out, "nothing kept") || !strings.Contains(out, "each session rebuilds") {
		t.Fatalf("index without embeddings: %d\n%s", code, out)
	}
	for _, c := range subcommands {
		if c.name == "index" && !strings.Contains(c.about, "rebuilds the rest") {
			t.Fatalf("index help claims more is kept than is: %q", c.about)
		}
	}
}
