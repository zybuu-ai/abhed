// Package websearch gives the agent access to the public web.
//
// Abhed is built to run air-gapped, so this is the one component that
// deliberately crosses the boundary — and it is OFF by default. When enabled,
// every query and every result is recorded in the event stream, and results are
// tagged untrusted like any other tool output: a search result is attacker-
// influenceable text, and the whole prompt-injection posture depends on never
// treating it as instruction (docs/architecture/03-security.md).
//
// Provider choice follows the brief: a free-forever default that needs no
// account, plus a clean seam for a paid API when a customer wants better
// results, higher volume, or a contractual SLA.
package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// Result is one search hit.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Provider performs a search. Implementations must not panic on a malformed
// upstream response — the web is not a well-behaved dependency.
type Provider interface {
	Name() string
	Search(ctx context.Context, query string, limit int) ([]Result, error)
	// RequiresKey reports whether this provider needs credentials, so
	// `abhed doctor` can say why a provider is unavailable.
	RequiresKey() bool
}

// Config selects and configures a provider.
type Config struct {
	// Provider is duckduckgo (default, free) | brave | tavily | serper | searxng.
	Provider string
	APIKey   string
	// BaseURL is required for searxng, which is self-hosted; it also allows
	// pointing any provider at an internal mirror or egress broker.
	BaseURL string
	// MaxResults caps what reaches the model's context. Ten results of prose
	// is already a meaningful slice of the context budget.
	MaxResults int
	Timeout    time.Duration
	// UserAgent identifies Abhed to upstream services. Some refuse an empty one.
	UserAgent  string
	HTTPClient *http.Client
}

func (c *Config) applyDefaults() {
	if c.Provider == "" {
		c.Provider = "duckduckgo"
	}
	if c.MaxResults <= 0 {
		c.MaxResults = 5
	}
	if c.Timeout == 0 {
		c.Timeout = 20 * time.Second
	}
	if c.UserAgent == "" {
		c.UserAgent = "Mozilla/5.0 (compatible; Abhed/0.1; +https://github.com/zybuu-ai/abhed)"
	}
	if c.HTTPClient == nil {
		// Judged and recorded by the egress guard under the allowlist.
		c.HTTPClient = &http.Client{Timeout: c.Timeout, Transport: &egress.Transport{Kind: egress.KindWebSearch}}
	}
}

// New builds the configured provider.
func New(cfg Config) (Provider, error) {
	cfg.applyDefaults()
	switch strings.ToLower(cfg.Provider) {
	case "duckduckgo", "ddg":
		return &duckDuckGo{cfg: cfg}, nil
	case "brave":
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("brave search needs an API key (set web_search.api_key_env)")
		}
		return &brave{cfg: cfg}, nil
	case "tavily":
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("tavily needs an API key (set web_search.api_key_env)")
		}
		return &tavily{cfg: cfg}, nil
	case "serper":
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("serper needs an API key (set web_search.api_key_env)")
		}
		return &serper{cfg: cfg}, nil
	case "searxng":
		if cfg.BaseURL == "" {
			return nil, fmt.Errorf("searxng needs base_url pointing at your instance")
		}
		return &searxng{cfg: cfg}, nil
	}
	return nil, fmt.Errorf("unknown web search provider %q "+
		"(want duckduckgo, brave, tavily, serper or searxng)", cfg.Provider)
}

// ---------------------------------------------------------------- DuckDuckGo

// duckDuckGo scrapes the HTML endpoint.
//
// The default because it is the only option that is genuinely free forever with
// no account, no key and no quota — which matters when Abhed is installed
// somewhere nobody will be signing up for an API.
//
// The tradeoff is honest: it parses HTML, so a markup change upstream breaks it.
// That is why the error says so explicitly rather than reporting "no results",
// and why the paid providers exist alongside it.
type duckDuckGo struct{ cfg Config }

func (d *duckDuckGo) Name() string      { return "duckduckgo" }
func (d *duckDuckGo) RequiresKey() bool { return false }

func (d *duckDuckGo) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	endpoint := "https://html.duckduckgo.com/html/"
	if d.cfg.BaseURL != "" {
		endpoint = d.cfg.BaseURL
	}
	form := url.Values{"q": {query}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", d.cfg.UserAgent)

	resp, err := d.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search returned %s", resp.Status)
	}

	body, err := readAllLimited(resp, 4<<20)
	if err != nil {
		return nil, err
	}
	results := parseDDG(string(body), limit)
	if len(results) == 0 {
		return nil, fmt.Errorf("no results parsed — DuckDuckGo's HTML may have " +
			"changed. Configure a paid provider (web_search.provider) if this persists")
	}
	return results, nil
}

// ---------------------------------------------------------------- Brave

// brave is a paid API with a free tier. Good quality, official, rate-limited.
type brave struct{ cfg Config }

func (b *brave) Name() string      { return "brave" }
func (b *brave) RequiresKey() bool { return true }

func (b *brave) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	base := orDefault(b.cfg.BaseURL, "https://api.search.brave.com/res/v1/web/search")
	u := base + "?q=" + url.QueryEscape(query) + "&count=" + itoa(limit)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.cfg.APIKey)

	var payload struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := doJSON(b.cfg.HTTPClient, req, &payload); err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(payload.Web.Results))
	for _, r := range payload.Web.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: stripTags(r.Description)})
	}
	return trim(out, limit), nil
}

// ---------------------------------------------------------------- Tavily

// tavily is built for LLM consumption: it returns cleaned prose rather than
// snippets, which costs fewer tokens for the same information.
type tavily struct{ cfg Config }

func (t *tavily) Name() string      { return "tavily" }
func (t *tavily) RequiresKey() bool { return true }

func (t *tavily) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	base := orDefault(t.cfg.BaseURL, "https://api.tavily.com/search")
	body, _ := json.Marshal(map[string]any{
		"api_key": t.cfg.APIKey, "query": query,
		"max_results": limit, "search_depth": "basic",
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")

	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := doJSON(t.cfg.HTTPClient, req, &payload); err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(payload.Results))
	for _, r := range payload.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return trim(out, limit), nil
}

// ---------------------------------------------------------------- Serper

// serper fronts Google results. The best quality of the paid options, and the
// one to choose when result relevance actually matters.
type serper struct{ cfg Config }

func (s *serper) Name() string      { return "serper" }
func (s *serper) RequiresKey() bool { return true }

func (s *serper) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	base := orDefault(s.cfg.BaseURL, "https://google.serper.dev/search")
	body, _ := json.Marshal(map[string]any{"q": query, "num": limit})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-KEY", s.cfg.APIKey)

	var payload struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic"`
	}
	if err := doJSON(s.cfg.HTTPClient, req, &payload); err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(payload.Organic))
	for _, r := range payload.Organic {
		out = append(out, Result{Title: r.Title, URL: r.Link, Snippet: r.Snippet})
	}
	return trim(out, limit), nil
}

// ---------------------------------------------------------------- SearXNG

// searxng is self-hosted metasearch. The right answer for an enclave that has
// brokered egress: run it inside the perimeter, point Abhed at it, and the
// agent never talks to the public internet directly.
type searxng struct{ cfg Config }

func (x *searxng) Name() string      { return "searxng" }
func (x *searxng) RequiresKey() bool { return false }

func (x *searxng) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	u := strings.TrimSuffix(x.cfg.BaseURL, "/") + "/search?format=json&q=" + url.QueryEscape(query)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", x.cfg.UserAgent)

	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := doJSON(x.cfg.HTTPClient, req, &payload); err != nil {
		return nil, fmt.Errorf("%w — check that your instance has the JSON format "+
			"enabled in settings.yml", err)
	}
	out := make([]Result, 0, len(payload.Results))
	for _, r := range payload.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return trim(out, limit), nil
}

// ---------------------------------------------------------------- helpers

func doJSON(c *http.Client, req *http.Request, out any) error {
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("search provider rejected the API key (%s)", resp.Status)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("search provider rate limit reached (%s)", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("search returned %s", resp.Status)
	}
	body, err := readAllLimited(resp, 8<<20)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode search response: %w", err)
	}
	return nil
}

func trim(r []Result, limit int) []Result {
	if limit > 0 && len(r) > limit {
		return r[:limit]
	}
	return r
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
