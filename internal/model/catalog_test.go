package model

import (
	"testing"
)

// Each known model resolves to its published window and output cap, by the
// longest prefix, through the wrappers a gateway or a cloud puts on its id.
func TestLookupLimits(t *testing.T) {
	cases := []struct {
		id             string
		window, output int
	}{
		{"claude-opus-5-5", 1_000_000, 128_000},
		{"claude-sonnet-5-5", 1_000_000, 128_000},
		{"claude-haiku-5-5", 1_000_000, 128_000},
		{"claude-haiku-4-5", 200_000, 64_000},
		{"claude-haiku-4-5-20251001", 200_000, 64_000},
		{"claude-opus-4-1", 200_000, 32_000},
		{"claude-opus-4-5", 200_000, 64_000},
		{"claude-sonnet-4-5@20250929", 200_000, 64_000},
		{"anthropic.claude-sonnet-5-5", 1_000_000, 128_000},
		{"us.anthropic.claude-opus-5-5-v1:0", 1_000_000, 128_000},
		{"anthropic/claude-haiku-4-5", 200_000, 64_000},
		{"Claude-Opus-5", 1_000_000, 128_000},
		{"claude-3-5-haiku-20241022", 200_000, 8_192},
		{"claude-instant-9", 200_000, 0}, // any other Claude
		{"gpt-5", 272_000, 128_000},
		{"gpt-5-mini", 272_000, 128_000},
		{"gpt-4o-mini", 128_000, 16_384},
		{"gpt-4.1-nano", 1_047_576, 32_768},
		{"gpt-4", 8_192, 8_192},
		{"o4-mini", 200_000, 100_000},
		{"gemini-2.5-pro", 1_048_576, 65_536},
		{"models/gemini-2.0-flash", 1_048_576, 8_192},
		{"gemini-1.5-pro-002", 2_097_152, 8_192},
		{"grok-4.7", 500_000, 0},
		{"llama3.1:70b", 131_072, 0},
		{"meta-llama/llama-3.3-70b-instruct", 131_072, 0},
		{"qwen3-coder:30b", 262_144, 0},
		{"qwen3:8b", 32_768, 0},
	}
	for _, c := range cases {
		w, o, ok := LookupLimits(c.id)
		if !ok || w != c.window || o != c.output {
			t.Errorf("%s: window %d output %d (%v), want %d %d", c.id, w, o, ok, c.window, c.output)
		}
	}
	for _, id := range []string{"my-finetune", "gemma4:26b", "", "deepseek-v4-pro", "mixtral"} {
		if _, _, ok := LookupLimits(id); ok {
			t.Errorf("%q matched a row", id)
		}
	}
}

// The configuration's window wins; the table comes next; an unknown model
// gets the fallback; a server on this machine never gets more than the
// fallback from the table, since it decides its window at run time.
func TestResolveLimits(t *testing.T) {
	cases := []struct {
		spec   Spec
		window int
		source string
		output int
	}{
		{Spec{Type: "anthropic", Model: "claude-sonnet-5-5"}, 1_000_000, WindowBuiltIn, 128_000},
		{Spec{Type: "anthropic", Model: "claude-sonnet-5-5", ContextWindow: 200_000}, 200_000, WindowConfig, 128_000},
		{Spec{Type: "anthropic", Model: "claude-haiku-4-5", MaxOutputTokens: 4096}, 200_000, WindowBuiltIn, 4096},
		{Spec{Type: "anthropic", Model: "claude-haiku-4-5", MaxOutputTokens: 100_000}, 200_000, WindowBuiltIn, 64_000},
		{Spec{Type: "bedrock-anthropic", Model: "us.anthropic.claude-opus-5-5", BaseURL: "https://bedrock.example"}, 1_000_000, WindowBuiltIn, 128_000},
		{Spec{Type: "openai", Model: "gpt-4o"}, 128_000, WindowBuiltIn, 16_384},
		{Spec{Type: "openai-compatible", Model: "mystery", BaseURL: "https://gw.example/v1"}, FallbackWindow, WindowFallback, 0},
		{Spec{Type: "ollama", Model: "llama3.1:8b"}, FallbackWindow, WindowFallback, 0},
		{Spec{Type: "vllm", Model: "qwen3:8b"}, 32_768, WindowFallback, 0},
		{Spec{Type: "vllm", Model: "qwen3-coder"}, FallbackWindow, WindowFallback, 0},
		{Spec{Type: "openai-compatible", Model: "llama3.1:8b", BaseURL: "http://localhost:11434/v1"}, FallbackWindow, WindowFallback, 0},
		{Spec{Type: "groq", Model: "llama-3.3-70b-versatile"}, 131_072, WindowBuiltIn, 0},
		{Spec{Type: "ollama", Model: "gemma4:26b", ContextWindow: 65536}, 65536, WindowConfig, 0},
		{Spec{Type: "gemini", Model: "gemini-2.5-flash"}, 1_048_576, WindowBuiltIn, 65_536},
	}
	for _, c := range cases {
		w, o, src := ResolveLimits(c.spec)
		if w != c.window || src != c.source || o != c.output {
			t.Errorf("%s/%s: %d %s out %d, want %d %s out %d", c.spec.Type, c.spec.Model, w, src, o, c.window, c.source, c.output)
		}
	}
}

// The adapter a spec builds reports the resolved window, which was zero
// before: the founder's Claude session never compacted because of it.
func TestNewAdapterCarriesWindow(t *testing.T) {
	for _, s := range []Spec{
		{Type: "anthropic", Model: "claude-sonnet-5-5", APIKey: "k"},
		{Type: "bedrock-anthropic", Model: "anthropic.claude-sonnet-5-5", BaseURL: "https://b.example", APIKey: "k"},
		{Type: "vertex-anthropic", Model: "claude-sonnet-5-5", Project: "p", Region: "global", APIKey: "k"},
		{Type: "openai", Model: "gpt-5", APIKey: "k"},
		{Type: "gemini", Model: "gemini-2.5-pro", APIKey: "k"},
		{Type: "vertex-gemini", Model: "gemini-2.5-pro", Project: "p", Region: "us-central1", APIKey: "k"},
		{Type: "watsonx", Model: "ibm/granite-4", BaseURL: "https://wx.example", APIKey: "k", Project: "p"},
		{Type: "ollama", Model: "qwen3:8b", BaseURL: "http://127.0.0.1:1/v1"},
		{Type: "openrouter", Model: "anthropic/claude-opus-5-5", APIKey: "k"},
	} {
		a, err := New(s)
		if err != nil {
			t.Fatalf("%s: %v", s.Type, err)
		}
		p := a.Profile()
		if p.ContextWindow <= 0 || p.WindowSource == "" {
			t.Errorf("%s %s: window %d source %q", s.Type, s.Model, p.ContextWindow, p.WindowSource)
		}
	}
}

// A configured output cap above the model's maximum is clamped, so it is
// not a refused request; one below it stands.
func TestOutputClamp(t *testing.T) {
	a, err := New(Spec{Type: "anthropic", Model: "claude-haiku-4-5", APIKey: "k", MaxOutputTokens: 500_000})
	if err != nil {
		t.Fatal(err)
	}
	p := a.Profile()
	if p.MaxOutputTokens != 64_000 || p.OutputLimit != 64_000 {
		t.Fatalf("clamp: %+v", p)
	}
	if got := OutputReserve(p, 0); got != 64_000 {
		t.Fatalf("reserve %d", got)
	}
	if got := OutputReserve(Profile{}, 0); got != DefaultOutputReserve {
		t.Fatalf("default reserve %d", got)
	}
	if got := OutputReserve(Profile{OutputLimit: 4096}, 10_000); got != 4096 {
		t.Fatalf("request above the model's cap: %d", got)
	}
}

// A learned limit only lowers the window, the configured one included, and
// discovery never overrides the configuration or a learned limit.
func TestLimitsLearnAndDiscover(t *testing.T) {
	l := resolveLimits(Spec{Type: "anthropic", Model: "claude-sonnet-5-5"}, "https://api.anthropic.com")
	if !l.learn(200_000) {
		t.Fatal("a smaller limit is learned")
	}
	if l.learn(300_000) {
		t.Fatal("a larger limit is not learned")
	}
	if l.discovered(1_000_000, 0) {
		t.Fatal("discovery overrode a learned window")
	}
	if w, _, src := l.snapshot(); w != 200_000 || src != WindowLearned {
		t.Fatalf("%d %s", w, src)
	}
	c := resolveLimits(Spec{Type: "anthropic", Model: "x", ContextWindow: 500_000}, "")
	if c.discovered(100_000, 0) {
		t.Fatal("discovery overrode the configuration")
	}
	if !c.learn(100_000) {
		t.Fatal("the server's limit lowers a configured window")
	}
	f := resolveLimits(Spec{Type: "ollama", Model: "qwen3:8b"}, "http://127.0.0.1:11434/v1")
	if !f.discovered(8192, 0) {
		t.Fatal("discovery replaces an assumed window")
	}
	if w, _, src := f.snapshot(); w != 8192 || src != WindowDiscovered {
		t.Fatalf("%d %s", w, src)
	}
}

func TestWindowNote(t *testing.T) {
	if WindowNote(Profile{Name: "m", ContextWindow: 1000, WindowSource: WindowBuiltIn}) != "" {
		t.Fatal("a known window needs no note")
	}
	n := WindowNote(Profile{Name: "m", ContextWindow: FallbackWindow, WindowSource: WindowFallback})
	if n == "" || !contains(n, "32,768") || !contains(n, "context_window") {
		t.Fatalf("note %q", n)
	}
}

// The longest prefix decides, whatever order the rows are written in: a
// family row placed above its members must not shadow them.
func TestLookupLimitsIgnoresRowOrder(t *testing.T) {
	saved := catalog
	t.Cleanup(func() { catalog = saved })
	reversed := make([]catalogRow, len(saved))
	for i, r := range saved {
		reversed[len(saved)-1-i] = r
	}
	catalog = reversed
	for id, want := range map[string]int{"claude-haiku-4-5": 200_000, "claude-opus-4-1": 200_000, "gpt-4o-mini": 128_000,
		"gemini-1.5-pro-002": 2_097_152, "qwen3-coder:30b": 262_144, "claude-opus-5-5": 1_000_000} {
		if w, _, _ := LookupLimits(id); w != want {
			t.Errorf("%s: %d, want %d", id, w, want)
		}
	}
}
