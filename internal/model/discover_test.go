package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// withDiscovery turns discovery on for one test. Every endpoint it reaches
// is an httptest stub; no test contacts a real local server.
func withDiscovery(t *testing.T) {
	t.Helper()
	was := discoveryOn.Swap(true)
	t.Cleanup(func() { discoveryOn.Store(was) })
}

// An Ollama server reports the running window of a loaded model on
// /api/ps, and a Modelfile's num_ctx on /api/show. Before the model is
// loaded and with no num_ctx, the window stays assumed; after the first
// reply, a second look finds it.
func TestDiscoverOllama(t *testing.T) {
	withDiscovery(t)
	var loaded atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			models := []map[string]any{}
			if loaded.Load() {
				models = append(models, map[string]any{"name": "qwen3:8b", "model": "qwen3:8b", "context_length": 16384})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{"parameters": "temperature 0.6", "model_info": map[string]any{"qwen3.context_length": 40960}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	// Ollama on its own port is how it is recognised; the stub is reached
	// through the ollama type instead.
	a, err := New(Spec{Type: "ollama", Model: "qwen3:8b", BaseURL: srv.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if Discover(context.Background(), a) {
		t.Fatal("nothing to discover before the model is loaded")
	}
	if p := a.Profile(); p.WindowSource != WindowFallback {
		t.Fatalf("%+v", p)
	}
	loaded.Store(true)
	if Discover(context.Background(), a) {
		t.Fatal("Discover is once; the second look is Rediscover")
	}
	if !Rediscover(context.Background(), a) {
		t.Fatal("the loaded model's window is found")
	}
	if p := a.Profile(); p.ContextWindow != 16384 || p.WindowSource != WindowDiscovered {
		t.Fatalf("%+v", p)
	}
	if Rediscover(context.Background(), a) {
		t.Fatal("no more than two looks")
	}
}

func TestDiscoverOllamaNumCtx(t *testing.T) {
	withDiscovery(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			_ = json.NewEncoder(w).Encode(map[string]any{"parameters": "num_ctx                        65536\nstop \"<|im_end|>\""})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{}})
	}))
	defer srv.Close()
	a, _ := New(Spec{Type: "ollama", Model: "gemma4:26b", BaseURL: srv.URL + "/v1"})
	if !Discover(context.Background(), a) || a.Profile().ContextWindow != 65536 {
		t.Fatalf("%+v", a.Profile())
	}
}

// The hosted model resources and the OpenAI-compatible listings with a
// context field are read; a configured window is never replaced.
func TestDiscoverEndpoints(t *testing.T) {
	withDiscovery(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models/claude-sonnet-5-5":
			if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
				w.WriteHeader(401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"max_input_tokens": 200000, "max_tokens": 64000})
		case r.URL.Path == "/v1beta/models/gemini-2.5-pro":
			_ = json.NewEncoder(w).Encode(map[string]any{"inputTokenLimit": 1048576, "outputTokenLimit": 65536})
		case r.URL.Path == "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"id": "other", "max_model_len": 1},
				map[string]any{"id": "served", "max_model_len": 24576},
				map[string]any{"id": "routed", "top_provider": map[string]any{"context_length": 400000}},
			}})
		case r.URL.Path == "/props":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_generation_settings": map[string]any{"n_ctx": 8192}})
		case r.URL.Path == "/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"max_input_tokens": 4095, "max_total_tokens": 4096})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	check := func(s Spec, want int) {
		t.Helper()
		a, err := New(s)
		if err != nil {
			t.Fatal(err)
		}
		Discover(context.Background(), a)
		if got := a.Profile().ContextWindow; got != want {
			t.Errorf("%s %s: window %d, want %d (%s)", s.Type, s.Model, got, want, a.Profile().WindowSource)
		}
	}
	check(Spec{Type: "anthropic", Model: "claude-sonnet-5-5", BaseURL: srv.URL, APIKey: "k"}, 200000)
	check(Spec{Type: "gemini", Model: "gemini-2.5-pro", BaseURL: srv.URL + "/v1beta", APIKey: "k"}, 1048576)
	check(Spec{Type: "vllm", Model: "served", BaseURL: srv.URL + "/v1"}, 24576)
	check(Spec{Type: "openai-compatible", Model: "routed", BaseURL: srv.URL + "/v1"}, 400000)
	check(Spec{Type: "llamacpp", Model: "x", BaseURL: srv.URL + "/v1"}, 8192)
	check(Spec{Type: "tgi", Model: "y", BaseURL: srv.URL + "/v1"}, 4095)
	check(Spec{Type: "vllm", Model: "served", BaseURL: srv.URL + "/v1", ContextWindow: 1000}, 1000)
	// Bedrock's path has no models resource: never asked.
	check(Spec{Type: "bedrock-anthropic", Model: "claude-sonnet-5-5", BaseURL: srv.URL, APIKey: "k"}, 1_000_000)
}

// Off, as it is for a library caller or any test that does not turn it on,
// discovery reaches nothing.
func TestDiscoverOff(t *testing.T) {
	hits := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	a, _ := New(Spec{Type: "vllm", Model: "m", BaseURL: srv.URL + "/v1"})
	if Discover(context.Background(), a) || hits.Load() != 0 {
		t.Fatal("discovery ran while off")
	}
	if !strings.HasPrefix(a.Profile().WindowSource, WindowFallback) {
		t.Fatal(a.Profile().WindowSource)
	}
}
