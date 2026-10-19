// Package defaults holds the model endpoints Abhed falls back to when a
// configuration names none. They are variables only so this module's tests
// can point them at a stub; nothing outside the module can move them.
package defaults

// LocalModelURL is the default configuration's model endpoint: Ollama on
// this machine, as Ollama's own default is reached.
var LocalModelURL = "http://localhost:11434/v1"

// OllamaURL is the base URL an "ollama" provider without one is given.
var OllamaURL = "http://127.0.0.1:11434/v1"
