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

// A server accounts sign in to binds each session to its account's own
// store, under the accounts directory, with no shared-store warning; a caller
// with no account gets an empty store nobody can write.
func TestAccountSessionsReadTheirOwnStore(t *testing.T) {
	t.Setenv(secrets.EnvFile, filepath.Join(t.TempDir(), "secrets.json"))
	dir := t.TempDir()
	t.Setenv(secrets.EnvAccountsDir, dir)
	var buf bytes.Buffer
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	cfg.Permissions.Allow = []string{"secret(GITHUB_TOKEN)"}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	v, _, err := s.sessionSecrets(StartSpec{User: "ann"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v.Path(), dir) {
		t.Fatalf("ann's store is %s, not under %s", v.Path(), dir)
	}
	if strings.Contains(buf.String(), "shares") {
		t.Fatalf("a shared-store warning:\n%s", buf.String())
	}
	none, _, err := s.sessionSecrets(StartSpec{User: "anonymous"})
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := none.Names(); len(names) != 0 || none.Set("GITHUB_TOKEN", "value-123456789") == nil {
		t.Fatal("a caller with no account got a store with values, or one it can write")
	}
}
