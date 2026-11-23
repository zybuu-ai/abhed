package model

import (
	"strings"
	"testing"
	"time"
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
	}
	for _, c := range cases {
		w, o, ok := LookupLimits(c.id)
		if !ok || w != c.window || o != c.output {
			t.Errorf("%s: window %d output %d (%v), want %d %d", c.id, w, o, ok, c.window, c.output)
		}
	}
	for _, id := range []string{"my-finetune", "gemma4:26b", "", "deepseek-v4-pro", "mixtral", "llama3.1:70b", "qwen3:8b"} {
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
		{Spec{Type: "vllm", Model: "claude-sonnet-5-5"}, FallbackWindow, WindowFallback, 128_000}, // a local server never gets the table's window
		{Spec{Type: "openai-compatible", Model: "llama3.1:8b", BaseURL: "http://localhost:11434/v1"}, FallbackWindow, WindowFallback, 0},
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
	l := resolveLimits(Spec{Type: "anthropic", Model: "claude-sonnet-5-5"}, "https://api.anthropic.com", "")
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
	c := resolveLimits(Spec{Type: "anthropic", Model: "x", ContextWindow: 500_000}, "", "")
	if c.discovered(100_000, 0) {
		t.Fatal("discovery overrode the configuration")
	}
	if !c.learn(100_000) {
		t.Fatal("the server's limit lowers a configured window")
	}
	f := resolveLimits(Spec{Type: "ollama", Model: "qwen3:8b"}, "http://127.0.0.1:11434/v1", "")
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
		"gemini-1.5-pro-002": 2_097_152, "claude-opus-5-5": 1_000_000} {
		if w, _, _ := LookupLimits(id); w != want {
			t.Errorf("%s: %d, want %d", id, w, want)
		}
	}
}

// A window below MinWindow, learned or discovered, is not believed.
func TestWindowFloor(t *testing.T) {
	l := resolveLimits(Spec{Type: "anthropic", Model: "claude-sonnet-5-5"}, "https://api.anthropic.com", "")
	if l.learn(5) || l.learn(MinWindow-1) || l.discovered(100, 0) {
		t.Fatal("a tiny window was taken")
	}
	if w, _, _ := l.snapshot(); w != 1_000_000 {
		t.Fatal(w)
	}
	if !l.learn(MinWindow) {
		t.Fatal("the floor itself is a window")
	}
}

type mapMemory map[string]int

func (m mapMemory) Recall(k string) (int, time.Time) {
	return m[k], time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
}
func (m mapMemory) Remember(k string, w int) { m[k] = w }
func (m mapMemory) Forget(k string)          { delete(m, k) }
func (m mapMemory) Secret() []byte           { return testSecret }

var testSecret = []byte("0123456789abcdef0123456789abcdef")

// A limit a provider named is kept for the next process, which starts with
// it; a limit Abhed only inferred is not kept.
func TestLearnedWindowIsRemembered(t *testing.T) {
	mem := mapMemory{}
	SetWindowMemory(mem)
	t.Cleanup(func() { SetWindowMemory(nil) })
	spec := Spec{Type: "anthropic", Model: "claude-sonnet-5-5", APIKey: "k"}
	a, _ := New(spec)
	if !LearnWindow(a, 200_000, true) {
		t.Fatal("not learned")
	}
	b, _ := New(spec) // the next process
	if p := b.Profile(); p.ContextWindow != 200_000 || p.WindowSource != WindowLearned {
		t.Fatalf("restarted with %+v", p)
	}
	other, _ := New(Spec{Type: "anthropic", Model: "claude-opus-5-5", APIKey: "k"})
	if other.Profile().ContextWindow != 1_000_000 {
		t.Fatal("another model took the limit")
	}
	c, _ := New(Spec{Type: "openai", Model: "gpt-4.1", APIKey: "k"})
	LearnWindow(c, 50_000, false)
	if d, _ := New(Spec{Type: "openai", Model: "gpt-4.1", APIKey: "k"}); d.Profile().ContextWindow != 1_047_576 {
		t.Fatal("an inferred limit was kept")
	}
	mem[WindowKey(testSecret, "openai", "https://api.openai.com/v1", "gpt-4o", "k")] = 10 // below the floor
	if e, _ := New(Spec{Type: "openai", Model: "gpt-4o", APIKey: "k"}); e.Profile().ContextWindow != 128_000 {
		t.Fatal("a remembered window under the floor was taken")
	}
	// A small named limit lowers this process's window but is not kept: one
	// relayed error must not pin every later session.
	g, _ := New(Spec{Type: "openai-compatible", BaseURL: "http://gateway.example/v1", Model: "claude-sonnet-5-5", APIKey: "k"})
	keptBefore := len(mem)
	if !LearnWindow(g, 2048, true) || g.Profile().ContextWindow != 2048 {
		t.Fatal("the small limit was not learned for the session")
	}
	if len(mem) != keptBefore {
		t.Fatal("a limit under MinRememberedWindow was written to the memory")
	}
	if h, _ := New(Spec{Type: "openai-compatible", BaseURL: "http://gateway.example/v1", Model: "claude-sonnet-5-5", APIKey: "k"}); h.Profile().ContextWindow == 2048 {
		t.Fatal("a limit under MinRememberedWindow was kept")
	}
	// A remembered limit does not lower a configured window; a live refusal does.
	conf := Spec{Type: "anthropic", Model: "claude-sonnet-5-5", APIKey: "k", ContextWindow: 1_000_000}
	cfgd, _ := New(conf)
	if p := cfgd.Profile(); p.ContextWindow != 1_000_000 || p.WindowSource != WindowConfig {
		t.Fatalf("remembered limit overrode the configuration: %+v", p)
	}
	if !LearnWindow(cfgd, 300_000, true) || cfgd.Profile().ContextWindow != 300_000 {
		t.Fatal("a refusal in this process did not lower the configured window")
	}
	// Another account on the same endpoint keeps its own limit.
	if o, _ := New(Spec{Type: "anthropic", Model: "claude-sonnet-5-5", APIKey: "other-key"}); o.Profile().ContextWindow != 1_000_000 {
		t.Fatal("another account took this account's limit")
	}
	// The learned date shows; Forget drops it.
	if b.Profile().LearnedAt.IsZero() {
		t.Fatal("no date on a remembered limit")
	}
	if !ForgetWindow(b) {
		t.Fatal("no memory to forget in")
	}
	if f, _ := New(spec); f.Profile().ContextWindow != 1_000_000 {
		t.Fatal("still remembered after Forget")
	}
}

// The key keeps nothing of the endpoint's credentials: user, password and
// query are left out, and the key is a keyed hash, so another secret gives
// another key and the file alone confirms no guess of a credential.
func TestWindowKeyHidesCredentials(t *testing.T) {
	k := WindowKey(testSecret, "openai-compatible", "http://user:sk-secret@gateway.example/v1?api-key=sk-secret2#frag", "m", "sk-secret3")
	for _, leak := range []string{"user", "sk-secret", "gateway", "api-key", "frag"} {
		if strings.Contains(k, leak) {
			t.Fatalf("key %q holds %q", k, leak)
		}
	}
	if k != WindowKey(testSecret, "openai-compatible", "HTTP://gateway.EXAMPLE/v1/", "m", "sk-secret3") {
		t.Fatal("the same endpoint without its credentials is another key")
	}
	if k == WindowKey(testSecret, "openai-compatible", "http://gateway.example/v1", "m", "another") {
		t.Fatal("two accounts share a key")
	}
	other := []byte("fedcba9876543210fedcba9876543210")
	if k == WindowKey(other, "openai-compatible", "http://gateway.example/v1", "m", "sk-secret3") {
		t.Fatal("two secrets give the same key: the hash is not keyed")
	}
	// Each part is length prefixed: parts that run together differently are
	// different keys.
	if WindowKey(testSecret, "a", "http://h/", "bc", "") == WindowKey(testSecret, "ab", "http://h/", "c", "") ||
		WindowKey(testSecret, "t", "http://h/", "m", "xy") == WindowKey(testSecret, "t", "http://h/", "mx", "y") {
		t.Fatal("parts run together")
	}
}

// A kept entry under the floor is never applied, whoever wrote it.
func TestRecallFloor(t *testing.T) {
	mem := mapMemory{}
	SetWindowMemory(mem)
	t.Cleanup(func() { SetWindowMemory(nil) })
	mem[WindowKey(testSecret, "anthropic", "https://api.anthropic.com", "claude-sonnet-5-5", "k")] = 4096
	if a, _ := New(Spec{Type: "anthropic", Model: "claude-sonnet-5-5", APIKey: "k"}); a.Profile().ContextWindow != 1_000_000 {
		t.Fatalf("a kept 4,096 was applied: %d", a.Profile().ContextWindow)
	}
	mem[WindowKey(testSecret, "anthropic", "https://api.anthropic.com", "claude-sonnet-5-5", "k")] = MinRememberedWindow
	if a, _ := New(Spec{Type: "anthropic", Model: "claude-sonnet-5-5", APIKey: "k"}); a.Profile().ContextWindow != MinRememberedWindow {
		t.Fatalf("a kept limit at the floor was not applied: %d", a.Profile().ContextWindow)
	}
}

// A memory with no secret is not used: nothing recalled, nothing kept.
func TestMemoryWithoutSecretIsUnused(t *testing.T) {
	mem := noSecret{mapMemory{}}
	SetWindowMemory(mem)
	t.Cleanup(func() { SetWindowMemory(nil) })
	a, _ := New(Spec{Type: "anthropic", Model: "claude-sonnet-5-5", APIKey: "k"})
	if !LearnWindow(a, 200_000, true) || len(mem.mapMemory) != 0 || ForgetWindow(a) {
		t.Fatalf("used without a secret: %v", mem.mapMemory)
	}
}

type noSecret struct{ mapMemory }

func (noSecret) Secret() []byte { return nil }
