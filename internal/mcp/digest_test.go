package mcp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/mcp"
)

// A digest is checked against the program the command resolves to: the
// right one starts, another refuses the server, and a URL server with a
// digest is refused since it cannot be checked.
func TestDigestPinsTheProgram(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	right := "sha256:" + hex.EncodeToString(sum[:])
	wrong := "sha256:" + strings.Repeat("0", 64)
	// A link to the program, found on the server's PATH, resolves to it.
	bin := t.TempDir()
	if err := os.Symlink(exe, filepath.Join(bin, "fake-mcp")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connect := func(cfg mcp.ServerConfig) []error {
		g := mcp.NewGateway()
		defer g.Close()
		cfg.Name, cfg.Enabled = "fake", true
		return g.Connect(ctx, []mcp.ServerConfig{cfg})
	}
	env := []string{"ABHED_MCP_FAKE_SERVER=1", "PATH=" + bin}
	if errs := connect(mcp.ServerConfig{Command: "fake-mcp", Env: env, Digest: right}); len(errs) != 0 {
		t.Fatalf("the pinned program was refused: %v", errs)
	}
	errs := connect(mcp.ServerConfig{Command: "fake-mcp", Env: env, Digest: wrong})
	if len(errs) != 1 || !errors.Is(errs[0], mcp.ErrDigestMismatch) {
		t.Fatalf("another program started under the digest: %v", errs)
	}
	if errs := connect(mcp.ServerConfig{Command: exe, Env: env, Digest: "sha1:abc"}); len(errs) != 1 {
		t.Fatalf("a malformed digest was accepted: %v", errs)
	}
	if errs := connect(mcp.ServerConfig{URL: "http://127.0.0.1:1/mcp", Digest: right}); len(errs) != 1 || !strings.Contains(errs[0].Error(), "cannot be checked") {
		t.Fatalf("a URL server with a digest: %v", errs)
	}
}
