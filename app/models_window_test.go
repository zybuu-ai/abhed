package app

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/model"
)

// abhed models shows each configured model's window, reply cap and where
// they came from, and warns of an assumed one; no endpoint is contacted.
func TestModelsCmdShowsWindows(t *testing.T) {
	cfg := config.Config{}
	cfg.Model.Default = "claude"
	cfg.Model.Providers = map[string]config.ProviderConfig{
		"claude": {Type: "anthropic", Model: "claude-sonnet-5-5", APIKey: "k"},
		"pinned": {Type: "anthropic", Model: "claude-sonnet-5-5", APIKey: "k", ContextWindow: 200000},
		"odd":    {Type: "openai-compatible", BaseURL: "http://127.0.0.1:1/v1", Model: "mystery"},
	}
	var out bytes.Buffer
	if code := modelsCmd(cfg, nil, &out, &out); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	text := out.String()
	for _, want := range []string{
		"1,000,000 tokens · replies up to 128,000 (Abhed's table of published limits)",
		"200,000 tokens · replies up to 128,000 (context_window in the configuration)",
		"32,768 tokens (assumed: not known)",
		"⚠ the context window of mystery is not known",
		"* claude",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

// The doctor's line, discovery being off under test: the table's window.
func TestWindowLineForDoctor(t *testing.T) {
	line, warn := windowLine(context.Background(), config.ProviderConfig{Type: "anthropic", Model: "claude-haiku-4-5", APIKey: "k"}, true)
	if line != "200,000 tokens · replies up to 64,000 (Abhed's table of published limits)" || warn != "" {
		t.Fatalf("%q %q", line, warn)
	}
}

// A kept limit shows with its date, and forget-window drops it.
func TestModelsForgetWindow(t *testing.T) {
	mem := &config.WindowMemory{Path: filepath.Join(t.TempDir(), "w.json")}
	model.SetWindowMemory(mem)
	t.Cleanup(func() { model.SetWindowMemory(nil) })
	cfg := config.Config{}
	cfg.Model.Default = "claude"
	cfg.Model.Providers = map[string]config.ProviderConfig{"claude": {Type: "anthropic", Model: "claude-sonnet-5-5", APIKey: "k"}}
	a, err := cfg.Model.Providers["claude"].Adapter()
	if err != nil {
		t.Fatal(err)
	}
	model.LearnWindow(a, 200_000, true)
	var out, errb bytes.Buffer
	if modelsCmd(cfg, nil, &out, &errb) != 0 || !strings.Contains(out.String(), "200,000 tokens") ||
		!strings.Contains(out.String(), "named by the provider on 20") || !strings.Contains(out.String(), "forget-window") {
		t.Fatalf("%s%s", out.String(), errb.String())
	}
	out.Reset()
	if modelsCmd(cfg, []string{"forget-window", "claude"}, &out, &errb) != 0 || !strings.Contains(out.String(), "forgot the 200,000-token limit") {
		t.Fatalf("%s%s", out.String(), errb.String())
	}
	out.Reset()
	if modelsCmd(cfg, nil, &out, &errb) != 0 || !strings.Contains(out.String(), "1,000,000 tokens") {
		t.Fatalf("after forget: %s", out.String())
	}
	if modelsCmd(cfg, []string{"forget-window", "nope"}, &out, &errb) != 1 || modelsCmd(cfg, []string{"bogus"}, &out, &errb) != 2 {
		t.Fatal("bad arguments accepted")
	}
	// An extra name is refused, not ignored: nothing is forgotten.
	fresh, _ := cfg.Model.Providers["claude"].Adapter()
	model.LearnWindow(fresh, 300_000, true)
	if modelsCmd(cfg, []string{"forget-window", "claude", "extra"}, &out, &errb) != 2 {
		t.Fatal("forget-window with an extra argument was taken")
	}
	if b, _ := cfg.Model.Providers["claude"].Adapter(); b.Profile().ContextWindow != 300_000 {
		t.Fatal("an extra argument still forgot the limit")
	}
}
