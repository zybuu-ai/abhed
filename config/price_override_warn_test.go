package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A managed price that replaces a person's own is said once, as the guide
// promises; the managed price is the one used.
func TestManagedPriceReplacingYoursWarns(t *testing.T) {
	withManaged(t, `{"pricing":{"gemma4:26b":{"input":0,"output":0}}}`)
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(`{"pricing":{"gemma4:26b":{"input":5,"output":5}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var said []string
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Warn: func(s string) { said = append(said, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Pricing["gemma4:26b"]; p.Input != 0 || p.Output != 0 {
		t.Fatalf("price %+v", p)
	}
	n := 0
	for _, s := range said {
		if strings.Contains(s, "sets pricing.gemma4:26b, which is ignored: the managed configuration sets that model's price") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("warnings %q", said)
	}
}
