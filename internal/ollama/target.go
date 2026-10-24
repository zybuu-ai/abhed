// Package ollama gets a local Ollama model working when a person chooses
// one: it finds whether Ollama is installed, whether its server answers and
// whether the model is pulled, and, only when the person agrees each time
// (or, for starting the server, has said "always"), installs Ollama, starts
// its server or pulls the model. Every failure comes back as a Problem: one
// plain message and the numbered steps the person can run themselves.
//
// Nothing here is reachable by the agent. The callers are the person's own
// model selection in the CLI, the workbench, the console and Studio.
package ollama

import (
	"net"
	"net/url"
	"strings"
)

// DefaultPort is the port Ollama listens on unless OLLAMA_HOST says otherwise.
const DefaultPort = "11434"

// Target is one configured provider that a local Ollama serves.
type Target struct {
	// Provider is the configured name the person chose.
	Provider string `json:"provider"`
	// Model is the Ollama model tag, such as qwen3:8b.
	Model string `json:"model"`
	// API is Ollama's own API root, without /v1: http://127.0.0.1:11434.
	API string `json:"api"`
	// Host and Port are the API's address as configured.
	Host string `json:"-"`
	Port string `json:"-"`
	// Loopback is set when the API is on this machine; only then may Abhed
	// start, install or pull anything.
	Loopback bool `json:"loopback"`
}

// Default reports whether the target is Ollama's default address, the only
// one the macOS app listens on.
func (t Target) Default() bool { return t.Loopback && t.Port == DefaultPort }

// Resolve says whether a provider of type typ at base, for model, is served
// by Ollama, and where. It is when the type is ollama, or when an
// OpenAI-compatible base is on Ollama's port or is the address OLLAMA_HOST
// names. A base that cannot be parsed is not a target.
func Resolve(provider, typ, base, model, defaultBase, ollamaHost string) (Target, bool) {
	typ = strings.ToLower(strings.TrimSpace(typ))
	if base == "" && typ == "ollama" {
		base = defaultBase
	}
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return Target{}, false
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	switch typ {
	case "ollama":
	case "openai-compatible", "openai":
		if port != DefaultPort && !sameHost(u, ollamaHost) {
			return Target{}, false
		}
	default:
		return Target{}, false
	}
	api := u.Scheme + "://" + u.Host
	host := u.Hostname()
	return Target{Provider: provider, Model: strings.TrimSpace(model), API: api, Host: host, Port: port, Loopback: loopback(host)}, true
}

// sameHost reports whether u is the address OLLAMA_HOST names.
func sameHost(u *url.URL, ollamaHost string) bool {
	h := strings.TrimSpace(ollamaHost)
	if h == "" {
		return false
	}
	if !strings.Contains(h, "://") {
		h = "http://" + h
	}
	o, err := url.Parse(h)
	if err != nil || o.Host == "" {
		return false
	}
	op := o.Port()
	if op == "" {
		op = DefaultPort
	}
	up := u.Port()
	if up == "" {
		up = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return strings.EqualFold(o.Hostname(), u.Hostname()) && op == up
}

// loopback reports whether host is this machine.
func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// serveHost is the OLLAMA_HOST a server Abhed starts listens on: the
// target's own port on 127.0.0.1, never another interface, whatever the
// person's environment says.
func (t Target) serveHost() string {
	host := t.Host
	if !loopback(host) || strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), t.Port)
}

// hostPrefix is what a manual command needs in front of it to reach a
// target on another port than the default: OLLAMA_HOST=127.0.0.1:PORT.
func (t Target) hostPrefix() string {
	if t.Port == DefaultPort {
		return ""
	}
	return "OLLAMA_HOST=" + t.serveHost() + " "
}

// PullCommand is the person's own command to pull the target's model.
func (t Target) PullCommand() string { return t.hostPrefix() + "ollama pull " + t.Model }
