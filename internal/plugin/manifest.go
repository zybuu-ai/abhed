// Package plugin reads, verifies and keeps plugin bundles: agents, skills,
// slash commands, simple hooks, MCP server definitions and docs under one
// manifest. A bundle adds content, never policy, and nothing in it runs at
// install. See docs/architecture/plugins.md.
package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ManifestName is the manifest's file name at a bundle's root.
const ManifestName = "abhed-plugin.json"

// SignatureName is the Sigstore bundle over the manifest, when signed.
const SignatureName = ManifestName + ".sigstore.json"

// RootVar is replaced with the installed folder in hook and server commands.
const RootVar = "${ABHED_PLUGIN_ROOT}"

// Manifest is what a bundle says it is and holds.
type Manifest struct {
	Schema      int    `json:"schema"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Publisher   string `json:"publisher"`
	Description string `json:"description,omitempty"`
	// Abhed is the oldest Abhed version the bundle runs on, as 1.3.0.
	Abhed        string       `json:"abhed,omitempty"`
	Files        []File       `json:"files"`
	Hooks        []Hook       `json:"hooks,omitempty"`
	MCPServers   []MCPServer  `json:"mcp_servers,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
}

// File is one file of the bundle and its SHA-256, hex.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Hook is a simple hook: veto-only, run in the session's sandbox.
type Hook struct {
	Name      string   `json:"name"`
	Events    []string `json:"events"`
	Command   string   `json:"command,omitempty"`
	URL       string   `json:"url,omitempty"`
	Match     []string `json:"match,omitempty"`
	TimeoutMS int      `json:"timeout_ms,omitempty"`
	Async     bool     `json:"async,omitempty"`
}

// MCPServer is an MCP server definition. It never runs with the host's
// files and never carries a credential: headers come from the environment.
type MCPServer struct {
	Name       string            `json:"name"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	URL        string            `json:"url,omitempty"`
	HeadersEnv map[string]string `json:"headers_env,omitempty"`
	AllowTools []string          `json:"allow_tools,omitempty"`
	Digest     string            `json:"digest,omitempty"`
}

// Capabilities are what a bundle requests. Its content may need no more.
type Capabilities struct {
	// Tools are the tool names its agents and commands list.
	Tools []string `json:"tools,omitempty"`
	// Egress are the hosts its hooks and URL servers reach.
	Egress []string `json:"egress,omitempty"`
	// Hooks are the events its hooks take.
	Hooks []string `json:"hooks,omitempty"`
	// MCPServers are the names of the servers it defines.
	MCPServers []string `json:"mcp_servers,omitempty"`
}

var (
	nameRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}$`)
	partRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,23}$`)
	versionRE   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]{1,40})?$`)
	hexRE       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	envNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	hookEvents  = []string{"tool_call", "permission_request", "user_prompt_submit", "stop", "tool_result", "tool_failure", "turn_end", "subagent_start", "subagent_end", "post_compact", "notification"}
	maxManifest = 1 << 20
)

// ParseManifest reads a manifest strictly: unknown keys are refused, so a
// bundle cannot carry a setting a reader would skip.
func ParseManifest(data []byte) (*Manifest, error) {
	if len(data) > maxManifest {
		return nil, fmt.Errorf("%s is over %d bytes", ManifestName, maxManifest)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestName, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s: more than one JSON value", ManifestName)
	}
	return &m, m.validate()
}

func (m *Manifest) validate() error {
	if m.Schema != 1 {
		return fmt.Errorf("%s: schema %d is not one this build reads (1)", ManifestName, m.Schema)
	}
	if !nameRE.MatchString(m.Name) {
		return fmt.Errorf("name %q: lower-case letters, digits and '-', up to 39", m.Name)
	}
	if !versionRE.MatchString(m.Version) {
		return fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", m.Version)
	}
	if strings.TrimSpace(m.Publisher) == "" || len(m.Publisher) > 200 {
		return errors.New("publisher is required, up to 200 characters")
	}
	if m.Abhed != "" && !versionRE.MatchString(strings.TrimPrefix(m.Abhed, "v")) {
		return fmt.Errorf("abhed %q is not MAJOR.MINOR.PATCH", m.Abhed)
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		if err := checkPath(f.Path); err != nil {
			return err
		}
		if !hexRE.MatchString(f.SHA256) {
			return fmt.Errorf("files %s: sha256 must be 64 lower-case hex digits", f.Path)
		}
		key := strings.ToLower(f.Path)
		if seen[key] {
			return fmt.Errorf("files lists %s twice", f.Path)
		}
		seen[key] = true
	}
	names := map[string]bool{}
	for _, h := range m.Hooks {
		if !partRE.MatchString(h.Name) || names["hook:"+h.Name] {
			return fmt.Errorf("hook %q: a unique name of letters, digits, '_' and '-', up to 24", h.Name)
		}
		names["hook:"+h.Name] = true
		if (h.Command == "") == (h.URL == "") {
			return fmt.Errorf("hook %s: set one of command and url", h.Name)
		}
		if h.URL != "" {
			if _, err := hostOf(h.URL); err != nil {
				return fmt.Errorf("hook %s: %w", h.Name, err)
			}
		}
		if len(h.Events) == 0 {
			return fmt.Errorf("hook %s: list the events it takes", h.Name)
		}
		for _, ev := range h.Events {
			if !slices.Contains(hookEvents, ev) {
				return fmt.Errorf("hook %s: %q is not a hook event; use %s", h.Name, ev, strings.Join(hookEvents, ", "))
			}
		}
		if h.TimeoutMS < 0 || h.TimeoutMS > 60000 {
			return fmt.Errorf("hook %s: timeout_ms is 0 to 60000", h.Name)
		}
	}
	for _, s := range m.MCPServers {
		if !partRE.MatchString(s.Name) || names["mcp:"+s.Name] {
			return fmt.Errorf("mcp server %q: a unique name of letters, digits, '_' and '-', up to 24", s.Name)
		}
		names["mcp:"+s.Name] = true
		if (s.Command == "") == (s.URL == "") {
			return fmt.Errorf("mcp server %s: set one of command and url", s.Name)
		}
		if s.URL != "" {
			if _, err := hostOf(s.URL); err != nil {
				return fmt.Errorf("mcp server %s: %w", s.Name, err)
			}
		}
		for k, v := range s.HeadersEnv {
			if k == "" || !envNameRE.MatchString(v) {
				return fmt.Errorf("mcp server %s: headers_env maps a header to an environment variable's name", s.Name)
			}
		}
	}
	return nil
}

// checkPath refuses a path that is not a plain relative file name under one
// of the folders a bundle may hold.
func checkPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return fmt.Errorf("file path %q must be relative and use '/'", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("file path %q has an empty, '.' or '..' part", p)
		}
	}
	if p == ManifestName || p == SignatureName {
		return fmt.Errorf("files must not list %s", p)
	}
	top, _, nested := strings.Cut(p, "/")
	switch {
	case nested && slices.Contains([]string{"agents", "skills", "commands", "hooks", "mcp", "docs"}, top):
		return nil
	case !nested && (strings.HasPrefix(p, "README") || strings.HasPrefix(p, "LICENSE")):
		return nil
	}
	return fmt.Errorf("file %q is outside agents/, skills/, commands/, hooks/, mcp/, docs/, README and LICENSE", p)
}

// hostOf is an http or https URL's host, lower-cased.
func hostOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return "", fmt.Errorf("%q is not an http or https URL", raw)
	}
	return strings.ToLower(u.Hostname()), nil
}

// Digest is the SHA-256 of b, hex.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// AtLeast reports whether version have is at least want; a build with no
// release version ("dev", "") is taken to meet any.
func AtLeast(have, want string) bool {
	if want == "" {
		return true
	}
	h, ok := parseVersion(have)
	if !ok {
		return true
	}
	w, ok := parseVersion(want)
	if !ok {
		return false
	}
	for i := range 3 {
		if h[i] != w[i] {
			return h[i] > w[i]
		}
	}
	return true
}

func parseVersion(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
