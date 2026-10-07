package server

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// plainRedactor is an operator redactor with only Redact and Span.
type plainRedactor struct{ value, label string }

func (p plainRedactor) Redact(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte(p.value), []byte(p.label))
}
func (p plainRedactor) Span() int { return len(p.value) }

// An owner's store chained with a custom operator redactor still refuses a
// path holding either one's value, and still names the owner's secrets.
func TestChainedKeepsPathCheckAndNames(t *testing.T) {
	own := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := own.Set("OWN_TOKEN", "own-token-value-7c1"); err != nil {
		t.Fatal(err)
	}
	c := chained{own.Session(), plainRedactor{value: "operator-value-9d2", label: "[secret:OP]"}}
	if label, found := c.FindInPath("/tmp/own-token-value-7c1/x"); !found || label != "[secret:OWN_TOKEN]" {
		t.Fatalf("owner's value in a path: %q %v", label, found)
	}
	if _, found := c.FindInPath("/tmp/operator-value-9d2"); !found {
		t.Fatal("the operator's value in a path was not found")
	}
	if _, found := c.FindInPath("/tmp/plain"); found {
		t.Fatal("a plain path was refused")
	}
	if names := c.Names(); !slices.Contains(names, "OWN_TOKEN") {
		t.Fatalf("names: %v", names)
	}
}

// The shared-store warning is given at each session's start as well as at
// startup, naming whose session it is.
func TestSharedStoreWarnsPerSession(t *testing.T) {
	t.Setenv(secrets.EnvFile, filepath.Join(t.TempDir(), "secrets.json"))
	var buf bytes.Buffer
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	cfg.Permissions.Allow = []string{"secret(GITHUB_TOKEN)"}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	buf.Reset()
	if _, _, err := s.sessionSecrets(StartSpec{User: "ann"}); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "shares the operator's secrets store") || !strings.Contains(out, "user=ann") {
		t.Fatalf("no warning at the session's start:\n%s", out)
	}
}
