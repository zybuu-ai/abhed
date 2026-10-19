package defaults

import "testing"

// The defaults are Ollama's own; tests elsewhere move them, never this one.
func TestDefaultsAreOllamas(t *testing.T) {
	if LocalModelURL != "http://localhost:11434/v1" || OllamaURL != "http://127.0.0.1:11434/v1" {
		t.Fatalf("%s %s", LocalModelURL, OllamaURL)
	}
}
