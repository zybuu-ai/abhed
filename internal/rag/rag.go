// Package rag connects Abhed to a retrieval system it does not own.
//
// Abhed already builds a local index (internal/index) over the workspace, but
// that only answers questions about code the agent can see. An enterprise has
// its own corpus — runbooks, incident history, product documentation, an
// existing vector database — behind an HTTP endpoint that already exists.
//
// This is deliberately schema-agnostic rather than a set of per-vendor
// clients. Every retrieval API is the same shape underneath (send a query, get
// back passages) and differs only in field names, so a small path mapping
// covers Elasticsearch, OpenSearch, Vespa, Qdrant, Weaviate, or an internal
// service, without Abhed carrying a client library for each and a release
// every time one changes.
package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// Passage is one retrieved chunk.
type Passage struct {
	Text   string  `json:"text"`
	Source string  `json:"source,omitempty"`
	Title  string  `json:"title,omitempty"`
	Score  float64 `json:"score,omitempty"`
}

// Config describes how to call one retrieval endpoint.
type Config struct {
	// Name distinguishes several corpora; it becomes part of the tool name.
	Name string `json:"name"`
	// Description tells the model what is in this corpus and when to search
	// it. This matters more than it looks: with no description the model
	// either ignores the tool or searches it for everything.
	Description string `json:"description,omitempty"`
	URL         string `json:"url"`
	Method      string `json:"method,omitempty"` // default POST

	// Headers are sent on every request. HeadersEnv reads from the
	// environment instead, so an API key need not sit in a config file the
	// agent itself can read.
	Headers    map[string]string `json:"headers,omitempty"`
	HeadersEnv map[string]string `json:"headers_env,omitempty"`

	// QueryField names the JSON field carrying the query, for POST bodies.
	// QueryParam names the URL parameter instead, for GET endpoints.
	QueryField string `json:"query_field,omitempty"` // default "query"
	QueryParam string `json:"query_param,omitempty"`
	// TopKField carries the result count, when the endpoint accepts one.
	TopKField string `json:"top_k_field,omitempty"`
	TopK      int    `json:"top_k,omitempty"`
	// Body is merged into the request, for endpoints that need fixed
	// parameters — an index name, a collection, a filter.
	Body map[string]any `json:"body,omitempty"`

	// ResultsPath locates the passage array in the response, as a dotted
	// path: "results", "hits.hits", "data.passages". Empty means the response
	// is itself an array.
	ResultsPath string `json:"results_path,omitempty"`
	// Field mappings within one result object. Each is a dotted path so
	// nested shapes (Elasticsearch's _source.body) work without special
	// casing.
	TextField   string `json:"text_field,omitempty"` // default: first string-ish field
	SourceField string `json:"source_field,omitempty"`
	TitleField  string `json:"title_field,omitempty"`
	ScoreField  string `json:"score_field,omitempty"`

	Timeout time.Duration `json:"-"`
	Client  *http.Client  `json:"-"`
}

// Retriever queries one configured corpus.
type Retriever struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) (*Retriever, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("rag: name is required")
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("rag: url is required for %q", cfg.Name)
	}
	if !strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
		return nil, fmt.Errorf("rag: %s url must be http or https", cfg.Name)
	}
	if cfg.Method == "" {
		cfg.Method = http.MethodPost
	}
	if cfg.QueryField == "" && cfg.QueryParam == "" {
		cfg.QueryField = "query"
	}
	if cfg.TopK <= 0 {
		cfg.TopK = 5
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	client := cfg.Client
	if client == nil {
		// Judged by the egress allowlist when one is in force.
		client = &http.Client{Timeout: cfg.Timeout, Transport: &egress.Transport{Kind: egress.KindRAG}}
	}
	return &Retriever{cfg: cfg, client: client}, nil
}

func (r *Retriever) Name() string        { return r.cfg.Name }
func (r *Retriever) Description() string { return r.cfg.Description }

// Search queries the corpus.
func (r *Retriever) Search(ctx context.Context, query string, topK int) ([]Passage, error) {
	if topK <= 0 {
		topK = r.cfg.TopK
	}
	req, err := r.buildRequest(ctx, query, topK)
	if err != nil {
		return nil, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s is unreachable: %w", r.cfg.Name, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", r.cfg.Name, err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%s rejected the credentials (%s)", r.cfg.Name, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s: %s", r.cfg.Name, resp.Status,
			truncate(strings.TrimSpace(string(body)), 300))
	}

	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("%s returned a response that is not JSON: %w", r.cfg.Name, err)
	}
	return r.extract(decoded, topK)
}

func (r *Retriever) buildRequest(ctx context.Context, query string, topK int) (*http.Request, error) {
	url := r.cfg.URL
	var body io.Reader

	if r.cfg.QueryParam != "" {
		sep := "?"
		if strings.Contains(url, "?") {
			sep = "&"
		}
		url += sep + r.cfg.QueryParam + "=" + queryEscape(query)
		if r.cfg.TopKField != "" {
			url += "&" + r.cfg.TopKField + "=" + strconv.Itoa(topK)
		}
	} else {
		payload := map[string]any{}
		for k, v := range r.cfg.Body {
			payload[k] = v
		}
		setPath(payload, r.cfg.QueryField, query)
		if r.cfg.TopKField != "" {
			setPath(payload, r.cfg.TopKField, topK)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, r.cfg.Method, url, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range r.cfg.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// extract pulls passages out of whatever shape the endpoint returned.
func (r *Retriever) extract(decoded any, topK int) ([]Passage, error) {
	node := decoded
	if r.cfg.ResultsPath != "" {
		found, ok := getPath(decoded, r.cfg.ResultsPath)
		if !ok {
			return nil, fmt.Errorf("%s: no field %q in the response — check results_path "+
				"against what the endpoint actually returns", r.cfg.Name, r.cfg.ResultsPath)
		}
		node = found
	}

	items, ok := node.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: %s is not a list of results",
			r.cfg.Name, orDefault(r.cfg.ResultsPath, "the response"))
	}

	out := make([]Passage, 0, len(items))
	for _, item := range items {
		p := r.passageFrom(item)
		if strings.TrimSpace(p.Text) == "" {
			continue // a result with no text is not usable as context
		}
		out = append(out, p)
		if len(out) >= topK {
			break
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (r *Retriever) passageFrom(item any) Passage {
	var p Passage
	if s, ok := item.(string); ok {
		// Some endpoints return bare strings.
		p.Text = s
		return p
	}
	obj, ok := item.(map[string]any)
	if !ok {
		return p
	}

	if r.cfg.TextField != "" {
		p.Text = stringAt(obj, r.cfg.TextField)
	} else {
		// No mapping configured: try the names retrieval APIs actually use,
		// so a simple endpoint works with no configuration at all.
		for _, k := range []string{"text", "content", "passage", "chunk", "body", "document"} {
			if v := stringAt(obj, k); v != "" {
				p.Text = v
				break
			}
		}
	}
	p.Source = firstNonEmpty(obj, r.cfg.SourceField, "source", "url", "path", "id", "_id")
	p.Title = firstNonEmpty(obj, r.cfg.TitleField, "title", "name", "heading")
	if r.cfg.ScoreField != "" {
		p.Score = floatAt(obj, r.cfg.ScoreField)
	} else {
		p.Score = floatAt(obj, "score")
		if p.Score == 0 {
			p.Score = floatAt(obj, "_score")
		}
	}
	return p
}

// ---------------------------------------------------------------- paths

// getPath walks a dotted path through decoded JSON.
func getPath(node any, path string) (any, bool) {
	if path == "" {
		return node, true
	}
	cur := node
	for _, part := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// setPath writes a value at a dotted path, creating objects as needed, so a
// nested request shape can be expressed without a struct per vendor.
func setPath(obj map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	for i, part := range parts {
		if i == len(parts)-1 {
			obj[part] = value
			return
		}
		next, ok := obj[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			obj[part] = next
		}
		obj = next
	}
}

func stringAt(obj map[string]any, path string) string {
	v, ok := getPath(obj, path)
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case []any:
		// Some endpoints return the text split into lines.
		var b strings.Builder
		for _, e := range t {
			if s, ok := e.(string); ok {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

func floatAt(obj map[string]any, path string) float64 {
	v, ok := getPath(obj, path)
	if !ok {
		return 0
	}
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func firstNonEmpty(obj map[string]any, configured string, fallbacks ...string) string {
	if configured != "" {
		return stringAt(obj, configured)
	}
	for _, k := range fallbacks {
		if v := stringAt(obj, k); v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func queryEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '~':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('+')
		default:
			for _, c := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		}
	}
	return b.String()
}
