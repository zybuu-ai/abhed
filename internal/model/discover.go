package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Discovery asks a serving endpoint for the window it actually runs a model
// with, where the endpoint will say: Ollama's /api/ps and /api/show,
// Anthropic's and Gemini's model resources, llama.cpp's /props, TGI's /info,
// and the models listing vLLM, SGLang, OpenRouter, Together, Groq and
// Mistral answer with a context field.
//
// It is off until the program turns it on. A library caller or a test that
// builds an adapter never reaches the network for it, and a test can never
// reach a real local server by accident.
var discoveryOn atomic.Bool

// EnableDiscovery lets Discover ask endpoints about themselves. The abhed
// program calls it once at start.
func EnableDiscovery() { discoveryOn.Store(true) }

// discoverTimeout bounds one discovery: a slow answer is not worth holding a
// session's first turn for, and the built-in window stands meanwhile.
const discoverTimeout = 3 * time.Second

// maxDiscoverAttempts bounds how often an endpoint is asked: once at the
// session's start, and once more after the first reply, when a local server
// has the model loaded and can say what window it gave it.
const maxDiscoverAttempts = 2

// Discover asks an adapter's endpoint for its window, at most twice per
// adapter and never for a configured window. It reports whether the window
// changed. A failure leaves the window as it was.
func Discover(ctx context.Context, a Adapter) bool {
	return discover(ctx, a, false)
}

// Rediscover is Discover after a reply, made only while the window is still
// assumed: a local server reports the window of a loaded model.
func Rediscover(ctx context.Context, a Adapter) bool {
	return discover(ctx, a, true)
}

func discover(ctx context.Context, a Adapter, again bool) bool {
	l := limitsOf(a)
	if l == nil || !discoveryOn.Load() {
		return false
	}
	l.mu.Lock()
	skip := l.source == WindowConfig || l.source == WindowLearned || l.attempts >= maxDiscoverAttempts ||
		(again && l.source != WindowFallback) || (!again && l.attempts > 0)
	if !skip {
		l.attempts++
	}
	l.mu.Unlock()
	if skip {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	w, out := probeWindow(ctx, a, l)
	if w <= 0 && out <= 0 {
		return false
	}
	return l.discovered(w, out)
}

// probeWindow asks the endpoint behind a, by its kind.
func probeWindow(ctx context.Context, a Adapter, l *Limits) (window, output int) {
	switch c := a.(type) {
	case *Anthropic:
		if l.typ != "anthropic" {
			return 0, 0 // a cloud gateway's path has no models resource
		}
		return anthropicModel(ctx, c)
	case *Gemini:
		if strings.Contains(c.BaseURL, "aiplatform.googleapis.com") {
			return 0, 0
		}
		return geminiModel(ctx, c)
	case *OpenAICompatible:
		if l.silentTruncation {
			return ollamaWindow(ctx, c)
		}
		if w := openAIModels(ctx, c); w > 0 {
			return w, 0
		}
		if l.typ == "llamacpp" {
			return llamacppProps(ctx, c), 0
		}
		if l.typ == "tgi" {
			return tgiInfo(ctx, c), 0
		}
	}
	return 0, 0
}

// getJSON reads one small JSON answer; any failure is no answer.
func getJSON(ctx context.Context, client *http.Client, method, u string, body io.Reader, hdr map[string]string, into any) bool {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return false
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(into) == nil
}

// anthropicModel reads GET /v1/models/{id}: max_input_tokens and max_tokens.
func anthropicModel(ctx context.Context, c *Anthropic) (int, int) {
	hdr := map[string]string{"anthropic-version": orElse(c.Version, defaultAnthropicVersion)}
	switch {
	case c.Bearer != "":
		hdr["Authorization"] = "Bearer " + c.Bearer
		hdr["anthropic-beta"] = "oauth-2025-04-20"
	case c.APIKey != "":
		hdr["x-api-key"] = c.APIKey
	}
	var m struct {
		MaxInput int `json:"max_input_tokens"`
		MaxOut   int `json:"max_tokens"`
	}
	if !getJSON(ctx, c.HTTP, http.MethodGet, c.BaseURL+"/v1/models/"+url.PathEscape(c.Model), nil, hdr, &m) {
		return 0, 0
	}
	return m.MaxInput, m.MaxOut
}

// geminiModel reads GET models/{id}: inputTokenLimit and outputTokenLimit.
func geminiModel(ctx context.Context, g *Gemini) (int, int) {
	hdr := map[string]string{}
	if g.APIKey != "" {
		hdr["x-goog-api-key"] = g.APIKey
	}
	var m struct {
		In  int `json:"inputTokenLimit"`
		Out int `json:"outputTokenLimit"`
	}
	name := strings.TrimPrefix(g.Model, "models/")
	if !getJSON(ctx, g.HTTP, http.MethodGet, g.BaseURL+"/models/"+url.PathEscape(name), nil, hdr, &m) {
		return 0, 0
	}
	return m.In, m.Out
}

// authHeader is the bearer an OpenAI-compatible endpoint takes.
func (c *OpenAICompatible) authHeader() map[string]string {
	if c.APIKey == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + c.APIKey}
}

// openAIModels reads GET /models, finding this model's entry and the first of
// the context fields the servers that report one use.
func openAIModels(ctx context.Context, c *OpenAICompatible) int {
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if !getJSON(ctx, c.HTTP, http.MethodGet, c.BaseURL+"/models", nil, c.authHeader(), &list) {
		return 0
	}
	for _, m := range list.Data {
		if id, _ := m["id"].(string); id != c.Model {
			continue
		}
		for _, k := range []string{"max_model_len", "context_length", "context_window", "max_context_length"} {
			if n := number(m[k]); n > 0 {
				return n
			}
		}
		if top, ok := m["top_provider"].(map[string]any); ok { // OpenRouter
			if n := number(top["context_length"]); n > 0 {
				return n
			}
		}
	}
	return 0
}

// root is an OpenAI-compatible base URL without its /v1, where a local
// server's own API lives.
func (c *OpenAICompatible) root() string {
	return strings.TrimSuffix(strings.TrimSuffix(c.BaseURL, "/"), "/v1")
}

// llamacppProps reads llama.cpp's GET /props: the n_ctx it was started with.
func llamacppProps(ctx context.Context, c *OpenAICompatible) int {
	var p struct {
		Settings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
		NCtx int `json:"n_ctx"`
	}
	if !getJSON(ctx, c.HTTP, http.MethodGet, c.root()+"/props", nil, c.authHeader(), &p) {
		return 0
	}
	if p.NCtx > 0 {
		return p.NCtx
	}
	return p.Settings.NCtx
}

// tgiInfo reads TGI's GET /info: the input it accepts.
func tgiInfo(ctx context.Context, c *OpenAICompatible) int {
	var p struct {
		MaxInput int `json:"max_input_tokens"`
		MaxTotal int `json:"max_total_tokens"`
	}
	if !getJSON(ctx, c.HTTP, http.MethodGet, c.root()+"/info", nil, c.authHeader(), &p) {
		return 0
	}
	if p.MaxInput > 0 {
		return p.MaxInput
	}
	return p.MaxTotal
}

var numCtxParam = regexp.MustCompile(`(?m)^\s*num_ctx\s+([0-9]+)`)

// ollamaWindow is the window an Ollama server runs the model with: from
// /api/ps when the model is loaded, which reports the running context, else
// the num_ctx its Modelfile sets. Over its OpenAI-compatible endpoint nothing
// else changes it, so with neither the server's own default applies, which
// the API does not report: the window stays assumed. The window the model
// was trained for comes back as the output cap's ceiling only.
func ollamaWindow(ctx context.Context, c *OpenAICompatible) (int, int) {
	var ps struct {
		Models []struct {
			Name          string `json:"name"`
			Model         string `json:"model"`
			ContextLength int    `json:"context_length"`
		} `json:"models"`
	}
	if getJSON(ctx, c.HTTP, http.MethodGet, c.root()+"/api/ps", nil, nil, &ps) {
		for _, m := range ps.Models {
			if (m.Name == c.Model || m.Model == c.Model || m.Name == c.Model+":latest") && m.ContextLength > 0 {
				return m.ContextLength, 0
			}
		}
	}
	var show struct {
		Parameters string         `json:"parameters"`
		ModelInfo  map[string]any `json:"model_info"`
	}
	body, _ := json.Marshal(map[string]string{"model": c.Model})
	if !getJSON(ctx, c.HTTP, http.MethodPost, c.root()+"/api/show", strings.NewReader(string(body)), nil, &show) {
		return 0, 0
	}
	if m := numCtxParam.FindStringSubmatch(show.Parameters); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n, 0
		}
	}
	return 0, 0
}

// number reads a JSON number that may have arrived as a float or a string.
func number(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}
