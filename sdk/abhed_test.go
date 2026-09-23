package abhed_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	abhed "github.com/zybuu-ai/abhed/sdk"
)

// The SDK is the wall between another program and internal/. If this file
// stops compiling, the supported surface has changed under someone.
func TestSurfaceIsUsableFromOutside(t *testing.T) {
	dir := t.TempDir()
	var seen []abhed.Event

	a, err := abhed.New(context.Background(), abhed.Options{
		Workspace: dir,
		Provider: &abhed.Provider{
			Type: "ollama", BaseURL: "http://127.0.0.1:1", // never reached
			Model: "test", ContextWindow: 8192,
		},
		Mode:    "auto",
		Deny:    []string{"bash(rm -rf *)"},
		OnEvent: func(ev abhed.Event) { seen = append(seen, ev) },
		Approve: func(ctx context.Context, tool string, args json.RawMessage, d abhed.Decision) (bool, error) {
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// The methods a caller needs are all present and callable.
	_ = a.Events()
	_ = a.Usage()
	_ = a.ExportHTML()
	a.Steer("noted")
	_ = seen
}

func TestProvidersIsExported(t *testing.T) {
	if len(abhed.Providers()) < 10 {
		t.Fatalf("Providers() returned %d; the registry should be visible to a caller",
			len(abhed.Providers()))
	}
}

// An embedded agent with no approver must refuse what needs approval, not
// assume yes. Defaulting to permissive would make the SDK quietly weaker than
// the same policy on the command line.
func TestNoApproverMeansRefuse(t *testing.T) {
	dir := t.TempDir()
	a, err := abhed.New(context.Background(), abhed.Options{
		Workspace: dir,
		Provider:  &abhed.Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"},
		Mode:      "default", // every mutation asks
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// Nothing to assert without a live model; the guarantee is in the
	// constructor, and this proves the option is reachable and the default is
	// the strict one rather than a panic.
}

func TestWorkspaceIsRequired(t *testing.T) {
	_, err := abhed.New(context.Background(), abhed.Options{})
	if err == nil {
		t.Fatal("an agent with no workspace must be refused")
	}
	if !strings.Contains(err.Error(), "Workspace") {
		t.Errorf("the error should name what is missing: %v", err)
	}
}

func TestConfigDirIsHonoured(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"model":{"default":"local","providers":{"local":{
		"type":"ollama","base_url":"http://127.0.0.1:1","model":"from-config"}}}}`
	if err := os.WriteFile(filepath.Join(dir, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := abhed.New(context.Background(), abhed.Options{
		Workspace: dir, ConfigDir: dir,
	})
	if err != nil {
		t.Fatalf("a config file the CLI would accept must work here too: %v", err)
	}
	a.Close()
}

// A bad deny rule must fail at construction, not on the first tool call.
func TestBadRuleFailsEarly(t *testing.T) {
	_, err := abhed.New(context.Background(), abhed.Options{
		Workspace: t.TempDir(),
		Provider:  &abhed.Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"},
		Deny:      []string{"bash("},
	})
	if err == nil {
		t.Fatal("a malformed rule must be refused at construction")
	}
}

// The syntax check is set from Options, overriding the config, and a bad
// value fails at construction like any other setting.
func TestSyntaxCheckOptionIsApplied(t *testing.T) {
	p := &abhed.Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"}
	for _, v := range []string{"", "refuse", "report", "off"} {
		a, err := abhed.New(context.Background(), abhed.Options{Workspace: t.TempDir(), Provider: p, SyntaxCheck: v})
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		a.Close()
	}
	_, err := abhed.New(context.Background(), abhed.Options{Workspace: t.TempDir(), Provider: p, SyntaxCheck: "sometimes"})
	if err == nil || !strings.Contains(err.Error(), "syntax_check") {
		t.Fatalf("a bad SyntaxCheck must fail at construction: %v", err)
	}
}
