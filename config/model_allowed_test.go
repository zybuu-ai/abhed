package config

import (
	"errors"
	"testing"
)

func TestModelAllowed(t *testing.T) {
	c := Default()
	c.Model.Providers["mine"] = ProviderConfig{Type: "openai-compatible", BaseURL: "http://127.0.0.1:1/v1", Model: "m"}
	c.Model.Default = "mine"
	if err := c.ModelAllowed("mine"); err != nil {
		t.Fatal(err)
	}
	if err := c.ModelAllowed("nosuch"); !errors.Is(err, ErrNoSuchModel) {
		t.Fatalf("unknown: %v", err)
	}
	c.Model.Providers["other"] = ProviderConfig{Type: "openai-compatible", BaseURL: "http://127.0.0.1:1/v1", Model: "o"}
	if err := c.ModelAllowed("other"); err != nil || c.EndpointPinnedAway("x") != nil {
		t.Fatalf("unmanaged: %v", err)
	}
	c.ManagedKeys = []string{"model.default"}
	var pinned *ManagedError
	if err := c.ModelAllowed("other"); !errors.As(err, &pinned) {
		t.Fatalf("pinned away: %v", err)
	}
	if err := c.ModelAllowed("mine"); err != nil {
		t.Fatalf("the pinned model: %v", err)
	}
	if err := c.EndpointPinnedAway("openai-compatible m"); !errors.As(err, &pinned) {
		t.Fatalf("an endpoint under the pin: %v", err)
	}
}
