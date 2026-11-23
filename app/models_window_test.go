package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/internal/model"
)

const windowModels = `{"model":{"default":"claude","providers":{
"claude":{"type":"anthropic","model":"claude-sonnet-5-5","api_key":"k"},
"pinned":{"type":"anthropic","model":"claude-sonnet-5-5","api_key":"k","context_window":200000},
"odd":{"type":"openai-compatible","base_url":"http://127.0.0.1:1/v1","model":"mystery"}}}}`

// abhed models list shows each configured model's window, reply cap and
// where they came from, and warns of an assumed one; no endpoint is contacted.
func TestModelsListShowsWindows(t *testing.T) {
	modelsHome(t, windowModels, 0o600)
	code, out, errs := runModels(t, "", false, "list")
	if code != 0 {
		t.Fatalf("list: %d %s", code, errs)
	}
	for _, want := range []string{
		"1,000,000 tokens · replies up to 128,000 (Abhed's table of published limits)",
		"200,000 tokens · replies up to 128,000 (context_window in the configuration)",
		"32,768 tokens (assumed: not known)",
		"⚠ the context window of mystery is not known",
		"* claude",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	code, out, _ = runModels(t, "", false, "list", "-json")
	var got struct {
		Models []modelRow `json:"models"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("list -json: %d %s", code, out)
	}
	for _, r := range got.Models {
		if r.Name == "odd" && (r.Window != model.FallbackWindow || r.WindowSource != model.WindowFallback) ||
			r.Name == "claude" && (r.Window != 1_000_000 || r.OutputLimit != 128_000 || r.WindowSource != model.WindowBuiltIn) {
			t.Errorf("row %+v", r)
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

// A kept limit shows with its date, and forget-window drops it; an extra
// argument is refused rather than ignored.
func TestModelsForgetWindow(t *testing.T) {
	modelsHome(t, windowModels, 0o600)
	mem := &config.WindowMemory{Path: filepath.Join(t.TempDir(), "w.json")}
	model.SetWindowMemory(mem)
	t.Cleanup(func() { model.SetWindowMemory(nil) })
	adapter := func() model.Adapter {
		a, err := config.ProviderConfig{Type: "anthropic", Model: "claude-sonnet-5-5", APIKey: "k"}.Adapter()
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	model.LearnWindow(adapter(), 200_000, true)
	_, out, _ := runModels(t, "", false, "list")
	if !strings.Contains(out, "200,000 tokens") || !strings.Contains(out, "named by the provider on 20") || !strings.Contains(out, "forget-window") {
		t.Fatalf("list:\n%s", out)
	}
	if code, _, errs := runModels(t, "", false, "forget-window", "claude", "extra"); code != 2 {
		t.Fatalf("an extra argument was taken: %d %s", code, errs)
	}
	if adapter().Profile().ContextWindow != 200_000 {
		t.Fatal("an extra argument still forgot the limit")
	}
	code, out, errs := runModels(t, "", false, "forget-window", "claude")
	if code != 0 || !strings.Contains(out, "forgot the 200,000-token limit") {
		t.Fatalf("forget: %d %s %s", code, out, errs)
	}
	if adapter().Profile().ContextWindow != 1_000_000 {
		t.Fatal("still kept after forget-window")
	}
	if code, _, _ := runModels(t, "", false, "forget-window", "nope"); code != 1 {
		t.Fatal("an unknown model was taken")
	}
	// With no memory, as for a guest, it says so.
	model.SetWindowMemory(nil)
	if code, _, errs := runModels(t, "", false, "forget-window", "claude"); code != 1 || !strings.Contains(errs, "a guest keeps none") {
		t.Fatalf("no memory: %d %s", code, errs)
	}
}

// forget-window is refused for a guest at the sign-in gate, as the other
// verbs that change a person's state are.
func TestGuestCannotForgetWindow(t *testing.T) {
	signInTest(t)
	if err := usePrincipal(identity.GuestPrincipal()); err != nil {
		t.Fatal(err)
	}
	if err := guestSubcommand([]string{"models", "forget-window", "claude"}); err == nil {
		t.Fatal("a guest may forget a kept window")
	}
	if err := guestSubcommand([]string{"models", "list"}); err != nil {
		t.Fatalf("a guest may not list: %v", err)
	}
}
