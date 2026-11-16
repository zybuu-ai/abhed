package toolset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// The index is built to the configured read rules: a deny rule, and the
// built-in ask rules for credentials files, keep a file out of it.
func TestIndexKeepsToTheReadRules(t *testing.T) {
	ws := t.TempDir()
	cfg := config.Default()
	cfg.Permissions.Deny = append(cfg.Permissions.Deny, "read(secrets/**)")
	withheld := IndexOptions(cfg, ws).Withheld
	for rel, want := range map[string]bool{"secrets/a.yaml": true, ".env": true, "certs/server.key": true, "app/main.go": false} {
		if got := withheld(filepath.Join(ws, rel)); got != want {
			t.Errorf("%s: withheld %v, want %v", rel, got, want)
		}
	}
}

// A session's own engine, with rules beyond the configuration's, is what the
// index is built to when the surface gives it.
func TestIndexBuildsToTheSessionsRules(t *testing.T) {
	ws := t.TempDir()
	for rel, content := range map[string]string{"app/a.go": "package app\n// ledger total\n", "team/b.go": "package team\n// ledger secret\n"} {
		p := filepath.Join(ws, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pol := policy.New(policy.ModeDefault)
	pol.Roots = func() []string { return []string{ws} }
	if err := pol.AddLayer("team", []string{"read(team/**)"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	ix, err := OpenIndexWithheld(t.Context(), config.Default(), ws, pol.ReadWithheldFunc())
	if err != nil {
		t.Fatal(err)
	}
	hits, _ := ix.Search(t.Context(), "ledger", 10)
	for _, h := range hits {
		if strings.Contains(h.Doc.Path, "team") {
			t.Fatal("a file the session's layer denies was indexed")
		}
	}
	if len(hits) == 0 {
		t.Fatal("nothing indexed")
	}
}
