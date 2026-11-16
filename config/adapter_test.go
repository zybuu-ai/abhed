package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// max_concurrent_requests reaches the model spec from the file.
func TestProviderMaxConcurrentRequests(t *testing.T) {
	var p ProviderConfig
	if err := json.Unmarshal([]byte(`{"type":"vllm","base_url":"http://127.0.0.1:8000/v1","model":"m","max_concurrent_requests":3}`), &p); err != nil {
		t.Fatal(err)
	}
	if got := p.Spec().MaxConcurrent; got != 3 {
		t.Fatalf("spec carries %d, want 3", got)
	}
	if got := (ProviderConfig{Type: "ollama"}).Spec().MaxConcurrent; got != 0 {
		t.Fatalf("an unset bound became %d; 0 keeps the type's default", got)
	}
}

// max_concurrent_requests is refused at startup on any provider whose type
// ignores it, not only the default.
func TestMaxConcurrentRefusedOnEveryProvider(t *testing.T) {
	c := Default()
	c.Model.Default = "local"
	c.Model.Providers = map[string]ProviderConfig{
		"local":  {Type: "vllm", BaseURL: "http://127.0.0.1:8000/v1", Model: "m", MaxConcurrentRequests: 2},
		"hosted": {Type: "anthropic", Model: "m", MaxConcurrentRequests: 1},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), `provider "hosted"`) {
		t.Fatalf("got %v, want the non-default provider refused", err)
	}
	c.Model.Providers["hosted"] = ProviderConfig{Type: "anthropic", Model: "m"}
	if err := c.Validate(); err != nil {
		t.Fatalf("a valid configuration was refused: %v", err)
	}
	c.Model.Providers["local"] = ProviderConfig{Type: "vllm", BaseURL: "http://127.0.0.1:8000/v1", Model: "m", MaxConcurrentRequests: -1}
	if err := c.Validate(); err == nil {
		t.Fatal("a negative bound was accepted")
	}
}
