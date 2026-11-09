package config

import (
	"encoding/json"
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
