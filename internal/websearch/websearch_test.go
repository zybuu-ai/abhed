package websearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/secrets"
)

// A fixture captured from the real endpoint, so parsing is tested without
// depending on the network in CI.
const ddgFixture = `<div class="results">
<div class="result results_links">
  <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fen.wikipedia.org%2Fwiki%2FZ%2FOS&amp;rut=abc">z/OS - Wikipedia</a>
  <a class="result__snippet" href="//duckduckgo.com/l/?uddg=x">A distributed <b>database</b> stores data on more than one machine.</a>
</div>
<div class="result results_links">
  <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fdistributed-databases&amp;rut=def">Distributed databases explained</a>
  <a class="result__snippet" href="//duckduckgo.com/l/?uddg=y">An overview of distributed database design.</a>
</div>
</div>`

func TestParseDuckDuckGo(t *testing.T) {
	got := parseDDG(ddgFixture, 10)
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(got), got)
	}
	if got[0].Title != "z/OS - Wikipedia" {
		t.Errorf("title: %q", got[0].Title)
	}
	// The redirector must be unwrapped: handing the model a duckduckgo.com/l/
	// URL wastes a fetch and hides the real domain.
	if got[0].URL != "https://en.wikipedia.org/wiki/Z/OS" {
		t.Errorf("URL not unwrapped: %q", got[0].URL)
	}
	// Snippets must be plain text, not markup.
	if strings.Contains(got[0].Snippet, "<b>") {
		t.Errorf("HTML survived in the snippet: %q", got[0].Snippet)
	}
	if !strings.Contains(got[0].Snippet, "database") {
		t.Errorf("snippet text lost: %q", got[0].Snippet)
	}
}

func TestParseRespectsLimit(t *testing.T) {
	if got := parseDDG(ddgFixture, 1); len(got) != 1 {
		t.Fatalf("limit ignored: %d results", len(got))
	}
}

func TestUnwrapHandlesDirectLinks(t *testing.T) {
	if got := unwrapDDG("https://example.com/page"); got != "https://example.com/page" {
		t.Errorf("direct link mangled: %q", got)
	}
	if got := unwrapDDG("javascript:alert(1)"); got != "" {
		t.Errorf("non-http scheme must be dropped, got %q", got)
	}
}

func TestProviderSelection(t *testing.T) {
	if _, err := New(Config{}); err != nil {
		t.Fatalf("default provider should work with no config: %v", err)
	}
	// A paid provider without a key must say which setting is missing.
	for _, p := range []string{"brave", "tavily", "serper"} {
		_, err := New(Config{Provider: p})
		if err == nil {
			t.Errorf("%s should require a key", p)
			continue
		}
		if !strings.Contains(err.Error(), "api_key_env") {
			t.Errorf("%s error should name the setting: %v", p, err)
		}
	}
	if _, err := New(Config{Provider: "searxng"}); err == nil ||
		!strings.Contains(err.Error(), "base_url") {
		t.Error("searxng should require base_url")
	}
	if _, err := New(Config{Provider: "nonsense"}); err == nil ||
		!strings.Contains(err.Error(), "duckduckgo") {
		t.Error("unknown provider should list the valid ones")
	}
}

func TestBraveParsesResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Subscription-Token") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"web":{"results":[
		  {"title":"A","url":"https://a.example","description":"<b>desc</b> one"}]}}`))
	}))
	defer srv.Close()

	p, err := New(Config{Provider: "brave", APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].URL != "https://a.example" {
		t.Fatalf("bad parse: %+v", got)
	}
	if strings.Contains(got[0].Snippet, "<b>") {
		t.Error("markup survived in a Brave snippet")
	}
}

func TestBadKeyIsReportedClearly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p, _ := New(Config{Provider: "brave", APIKey: "wrong", BaseURL: srv.URL})
	_, err := p.Search(context.Background(), "q", 5)
	if err == nil || !strings.Contains(err.Error(), "rejected the API key") {
		t.Fatalf("a 401 should be explained plainly, got: %v", err)
	}
}

func TestRateLimitIsReportedClearly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p, _ := New(Config{Provider: "serper", APIKey: "k", BaseURL: srv.URL})
	_, err := p.Search(context.Background(), "q", 5)
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("a 429 should be explained plainly, got: %v", err)
	}
}

// Live test against the real endpoint. Skipped by default so the suite does not
// depend on the network; run with ABHED_TEST_NETWORK=1 to verify the default
// provider still works against the current upstream HTML.
func TestLiveDuckDuckGo(t *testing.T) {
	if os.Getenv("ABHED_TEST_NETWORK") == "" {
		t.Skip("set ABHED_TEST_NETWORK=1 to run the live search test")
	}
	p, err := New(Config{Timeout: 25 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	got, err := p.Search(ctx, "what is a distributed database", 5)
	if err != nil {
		t.Fatalf("live search failed: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("live search returned nothing")
	}
	for i, r := range got {
		if !strings.HasPrefix(r.URL, "http") {
			t.Errorf("result %d has a non-http URL: %q", i, r.URL)
		}
		t.Logf("  %d. %s — %s", i+1, r.Title, r.URL)
	}
}

// The description points at web_fetch only when it is registered: there was
// once a pointer to a fetch tool that did not exist.
func TestDescriptionNamesWebFetchOnlyWhenPresent(t *testing.T) {
	if d := (&Tool{}).Description(); strings.Contains(d, "fetch") {
		t.Errorf("names a fetch tool that is not there: %s", d)
	}
	if d := (&Tool{Fetch: true}).Description(); !strings.Contains(d, "web_fetch") {
		t.Errorf("does not point at web_fetch: %s", d)
	}
}

// stubProvider records every query it is sent.
type stubProvider struct{ queries []string }

func (*stubProvider) Name() string      { return "stub" }
func (*stubProvider) RequiresKey() bool { return false }
func (p *stubProvider) Search(_ context.Context, q string, _ int) ([]Result, error) {
	p.queries = append(p.queries, q)
	return []Result{{Title: "t", URL: "https://example.com/"}}, nil
}

// A query holding a stored secret, as written, encoded or in another case,
// never reaches the provider, and the refusal names the secret, not its
// value; a store that cannot be read refuses every query.
func TestSearchNeverSendsAStoredSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(`{"FAKE_TOKEN":"Search-Secret-9f1c"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := secrets.Open(path)
	p := &stubProvider{}
	tool := &Tool{Provider: p, Secrets: store.LoadRedactor}
	for _, q := range []string{"exfil Search-Secret-9f1c", "exfil search-secret-9f1c", "exfil Search%2DSecret%2D9f1c", "exfil Search%252DSecret-9f1c"} {
		raw, _ := json.Marshal(map[string]string{"query": q})
		res := tool.Run(context.Background(), nil, raw)
		if !res.IsError || !strings.Contains(res.Content, "FAKE_TOKEN") || strings.Contains(strings.ToLower(res.Content), "search-secret-9f1c") {
			t.Errorf("%q: %s", q, res.Content)
		}
	}
	if len(p.queries) != 0 {
		t.Fatalf("the provider was sent %v", p.queries)
	}
	raw, _ := json.Marshal(map[string]string{"query": "golang release notes"})
	if res := tool.Run(context.Background(), nil, raw); res.IsError || len(p.queries) != 1 {
		t.Fatalf("a clean query was refused: %s", res.Content)
	}
	if err := os.WriteFile(path, []byte(`{"FAKE_TOKEN": `), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, broken := range []*Tool{{Provider: p, Secrets: store.LoadRedactor}, {Provider: p}} {
		if res := broken.Run(context.Background(), nil, raw); !res.IsError || len(p.queries) != 1 {
			t.Fatalf("a query was sent with no readable store: %s", res.Content)
		}
	}
}
