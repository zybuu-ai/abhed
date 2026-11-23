package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
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
	if code := modelsCmd(cfg, &out); code != 0 {
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
