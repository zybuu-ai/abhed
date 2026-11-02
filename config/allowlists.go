package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/mcp"
)

// MCPMatch is one entry of mcp.allowed or mcp.denied. A server matches when
// every field the entry sets matches it: Name its configured name, Command
// the program its command resolves to (a stdio server), Digest the sha256
// its configuration pins and Abhed checks before starting it (a stdio
// server), Host its URL's host (a remote server). Name and Command take *
// for any run of characters, Host a leading *. for any subdomain.
type MCPMatch struct {
	Name    string `json:"name,omitempty"`
	Command string `json:"command,omitempty"`
	Digest  string `json:"digest,omitempty"`
	Host    string `json:"host,omitempty"`
}

// UnmarshalJSON refuses an entry with a field it does not know, so a
// misspelt field cannot widen an allow entry to every server.
func (m *MCPMatch) UnmarshalJSON(data []byte) error {
	type plain MCPMatch
	var p plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("an mcp.allowed or mcp.denied entry: %w", err)
	}
	*m = MCPMatch(p)
	return nil
}

func (m MCPMatch) String() string {
	var parts []string
	for _, kv := range [][2]string{{"name", m.Name}, {"command", m.Command}, {"digest", m.Digest}, {"host", m.Host}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+Printable(kv[1]))
		}
	}
	return "{" + strings.Join(parts, " ") + "}"
}

// MCPServer is what the lists judge of one server: its name, the program a
// stdio server's command resolves to on its PATH (as found and with links
// followed; none when it cannot be found), the digest its configuration
// pins, and a remote server's URL.
type MCPServer struct {
	Name     string
	Programs []string
	Digest   string
	URL      string
}

// MCPServerOf is what the lists judge of a configured server.
func MCPServerOf(s MCPServerConfig) MCPServer {
	out := MCPServer{Name: s.Name, Digest: s.Digest, URL: s.URL}
	if s.URL == "" && s.Command != "" {
		out.Programs = mcp.Programs(s.Command, s.Env)
	}
	return out
}

// allowLayer is one configuration file's model.allowed, mcp.allowed and
// mcp.denied, as it wrote them.
type allowLayer struct {
	file                       string
	models                     []string
	mcpAllowed, mcpDenied      []MCPMatch
	setsModels, setsMCPAllowed bool
	setsMCPDenied              bool
}

// noteAllowLayer keeps what the file just merged says of the allow lists.
// Lists are replaced wholesale as files merge, so each file's own is kept to
// be applied together: a lower layer can only narrow the managed lists.
func noteAllowLayer(cfg *Config, file string, keys []string) {
	noteAllowLayerFrom(cfg, cfg, file, keys)
}

// noteAllowLayerFrom is noteAllowLayer with the lists read from src: an
// untrusted workspace's own, which are never merged into cfg.
func noteAllowLayerFrom(cfg, src *Config, file string, keys []string) {
	l := allowLayer{file: file}
	for _, k := range keys {
		switch {
		case k == "model.allowed" || strings.HasPrefix(k, "model.allowed."):
			l.setsModels = true
		case k == "mcp.allowed" || strings.HasPrefix(k, "mcp.allowed."):
			l.setsMCPAllowed = true
		case k == "mcp.denied" || strings.HasPrefix(k, "mcp.denied."):
			l.setsMCPDenied = true
		}
	}
	if !l.setsModels && !l.setsMCPAllowed && !l.setsMCPDenied {
		return
	}
	if l.setsModels {
		l.models = slices.Clone(src.Model.Allowed)
	}
	if l.setsMCPAllowed {
		l.mcpAllowed = slices.Clone(src.MCP.Allowed)
	}
	if l.setsMCPDenied {
		l.mcpDenied = slices.Clone(src.MCP.Denied)
	}
	cfg.allowLayers = append(slices.Clone(cfg.allowLayers), l)
}

// layersOrSelf are the allow layers loading kept, or, for a Config built by
// hand, its own lists as one layer.
func (c Config) layersOrSelf() []allowLayer {
	if len(c.allowLayers) > 0 {
		return c.allowLayers
	}
	l := allowLayer{file: "config", models: c.Model.Allowed, mcpAllowed: c.MCP.Allowed, mcpDenied: c.MCP.Denied,
		setsModels: c.Model.Allowed != nil, setsMCPAllowed: c.MCP.Allowed != nil, setsMCPDenied: c.MCP.Denied != nil}
	return []allowLayer{l}
}

// validateAllowLists refuses a malformed list in any layer: a list that
// cannot be read is not guessed at, so the engine does not start on it.
func (c Config) validateAllowLists() error {
	for _, l := range c.layersOrSelf() {
		where := Printable(l.file)
		if l.setsModels {
			if len(l.models) == 0 {
				return fmt.Errorf("%s: model.allowed is empty, which would refuse every model; remove it or list the models", where)
			}
			for _, p := range l.models {
				if err := checkModelPattern(p); err != nil {
					return fmt.Errorf("%s: model.allowed %q: %w", where, Printable(p), err)
				}
			}
		}
		for list, entries := range map[string][]MCPMatch{"mcp.allowed": l.mcpAllowed, "mcp.denied": l.mcpDenied} {
			for _, m := range entries {
				if err := checkMCPMatch(m); err != nil {
					return fmt.Errorf("%s: %s entry %s: %w", where, list, m, err)
				}
			}
		}
	}
	return nil
}

func checkModelPattern(p string) error {
	switch {
	case strings.TrimSpace(p) != p || strings.ContainsAny(p, " \t\r\n"):
		return errors.New("a pattern holds no whitespace")
	case !strings.Contains(p, "/"):
		return errors.New("want provider-type/model, such as anthropic/claude-* or ollama/qwen3:*")
	case strings.HasPrefix(p, "/"):
		return errors.New("the provider type is missing before the /")
	case Printable(p) != p:
		return errors.New("a pattern holds only printable characters")
	}
	return nil
}

func checkMCPMatch(m MCPMatch) error {
	if m == (MCPMatch{}) {
		return errors.New("it sets none of name, command, digest or host, so it would match every server")
	}
	for _, v := range []string{m.Name, m.Command, m.Digest, m.Host} {
		if v != strings.TrimSpace(v) || Printable(v) != v {
			return errors.New("a value holds no surrounding space and only printable characters")
		}
	}
	if m.Command != "" && !strings.HasPrefix(m.Command, "/") {
		return errors.New("command is the program's absolute path")
	}
	if m.Digest != "" && !digestForm(strings.ToLower(m.Digest)) {
		return errors.New("digest is sha256:<64 hex digits>")
	}
	if m.Host != "" && (strings.ContainsAny(m.Host, "/:@ ") || strings.Contains(strings.TrimPrefix(m.Host, "*."), "*")) {
		return errors.New("host is a host name, optionally *.example.com, with no scheme, port or path")
	}
	if (m.Command != "" || m.Digest != "") && m.Host != "" {
		return errors.New("command and digest match a server Abhed starts, host one it reaches; an entry cannot be both")
	}
	return nil
}

func digestForm(d string) bool {
	h, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(h) != 64 {
		return false
	}
	return strings.Trim(h, "0123456789abcdef") == ""
}

// glob reports whether s matches pattern, where * stands for any run of
// characters, / included, and every other character for itself.
func glob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// ModelID is what model.allowed patterns are matched against: the provider's
// type and the model it serves, as type/model.
func ModelID(p ProviderConfig) string { return p.Type + "/" + p.Model }

// ModelListed is the refusal of a model no model.allowed list admits, or
// nil. Every layer that sets the list must admit it, so a lower layer can
// only narrow the managed one. name says which configured provider it is.
func (c Config) ModelListed(name string, p ProviderConfig) error {
	id := ModelID(p)
	for _, l := range c.layersOrSelf() {
		if !l.setsModels {
			continue
		}
		if !slices.ContainsFunc(l.models, func(pat string) bool { return glob(pat, id) }) {
			who := listOwner(l.file)
			value := id
			if name != "" {
				value = name + " (" + id + ")"
			}
			return &ManagedError{Key: "model.allowed", Value: value, File: l.file,
				Reason: who + "'s model.allowed admits only " + strings.Join(l.models, ", ")}
		}
	}
	return nil
}

// ModelsAllowed are the model.allowed patterns in force, each list on its
// own, as `abhed doctor` and the policy views show them; nil when none is.
func (c Config) ModelsAllowed() []AllowList {
	var out []AllowList
	for _, l := range c.layersOrSelf() {
		if l.setsModels {
			out = append(out, AllowList{File: l.file, Layer: c.listLayer(l.file), Managed: l.file == managed.ConfigFile, Entries: slices.Clone(l.models)})
		}
	}
	return out
}

// AllowList is one layer's allow or deny list, as shown: the file and
// layer (managed, user, settings or workspace) that set it.
type AllowList struct {
	File    string
	Layer   string
	Managed bool
	Denied  bool
	Entries []string
}

// listLayer is the layer the file that set a list was read as.
func (c Config) listLayer(file string) string {
	if file == managed.ConfigFile {
		return LayerManaged
	}
	if l := c.layerOf(file); l != "" {
		return l
	}
	return LayerUser
}

// MCPLists are the mcp.allowed and mcp.denied lists in force, by layer.
func (c Config) MCPLists() []AllowList {
	var out []AllowList
	for _, l := range c.layersOrSelf() {
		add := func(entries []MCPMatch, denied bool) {
			var s []string
			for _, m := range entries {
				s = append(s, m.String())
			}
			out = append(out, AllowList{File: l.file, Layer: c.listLayer(l.file), Managed: l.file == managed.ConfigFile, Denied: denied, Entries: s})
		}
		if l.setsMCPAllowed {
			add(l.mcpAllowed, false)
		}
		if l.setsMCPDenied {
			add(l.mcpDenied, true)
		}
	}
	return out
}

// HasMCPLists reports whether any mcp.allowed or mcp.denied list is in force.
func (c Config) HasMCPLists() bool { return len(c.MCPLists()) > 0 }

// matches reports whether m matches the server s.
func (m MCPMatch) matches(s MCPServer) bool {
	if m.Name != "" && !glob(m.Name, s.Name) {
		return false
	}
	if m.Command != "" || m.Digest != "" {
		if s.URL != "" {
			return false
		}
		if m.Command != "" && !slices.ContainsFunc(s.Programs, func(p string) bool { return glob(path.Clean(m.Command), p) }) {
			return false
		}
		if m.Digest != "" && !strings.EqualFold(strings.TrimSpace(s.Digest), m.Digest) {
			return false
		}
	}
	if m.Host != "" {
		u, err := url.Parse(s.URL)
		if s.URL == "" || err != nil || u.Hostname() == "" {
			return false
		}
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		want := strings.ToLower(m.Host)
		if sub, ok := strings.CutPrefix(want, "*."); ok {
			if !strings.HasSuffix(host, "."+sub) {
				return false
			}
		} else if host != want {
			return false
		}
	}
	return true
}

// MCPAdmitted is the refusal of an MCP server a deny list names or an allow
// list leaves out, or nil. Every layer's allow list must admit it and no
// layer's deny list may name it, so a lower layer can only narrow.
func (c Config) MCPAdmitted(s MCPServer) error {
	layers := c.layersOrSelf()
	for _, l := range layers {
		for _, m := range l.mcpDenied {
			if m.matches(s) {
				return &ManagedError{Key: "mcp.denied", Value: s.Name, File: l.file,
					Reason: fmt.Sprintf("%s lists it in mcp.denied (%s)", listOwner(l.file), m)}
			}
		}
	}
	for _, l := range layers {
		if l.setsMCPAllowed && !slices.ContainsFunc(l.mcpAllowed, func(m MCPMatch) bool { return m.matches(s) }) {
			return &ManagedError{Key: "mcp.allowed", Value: s.Name, File: l.file,
				Reason: listOwner(l.file) + "'s mcp.allowed list does not match it"}
		}
	}
	return nil
}

func listOwner(file string) string {
	if file == managed.ConfigFile {
		return "the managed configuration"
	}
	return Printable(file)
}

// narrowToAllowLists applies the lists to what loading gathered: an MCP
// server the lists refuse is taken out, a fallback the model lists refuse is
// dropped, and a suggestion or critic model they refuse is left unset. Each is
// set aside with why, so the session records it. The default model is left
// as it is: resolving it refuses it, with the reason, where it would run.
func narrowToAllowLists(cfg *Config) {
	layerOf := func(err error) string {
		var me *ManagedError
		if errors.As(err, &me) && me.File == managed.ConfigFile {
			return LayerManaged
		}
		return cfg.layerOf(fileOf(err))
	}
	aside := func(key, value string, err error) {
		var me *ManagedError
		reason := err.Error()
		if errors.As(err, &me) {
			reason = me.Reason
		}
		cfg.SetAside = append(cfg.SetAside, SetAsideKey{File: fileOf(err), Layer: layerOf(err), Key: key, Value: value, Reason: reason})
	}
	if cfg.HasMCPLists() {
		kept := cfg.MCP.Servers[:0:0]
		for _, s := range cfg.MCP.Servers {
			if err := cfg.MCPAdmitted(MCPServerOf(s)); err != nil {
				aside("mcp.servers", s.Name, err)
				continue
			}
			kept = append(kept, s)
		}
		cfg.MCP.Servers = kept
	}
	if len(cfg.ModelsAllowed()) == 0 {
		return
	}
	listed := func(name string) error {
		p, ok := cfg.Model.Providers[name]
		if !ok {
			return nil // reported as unconfigured elsewhere
		}
		return cfg.ModelListed(name, p)
	}
	if m := cfg.Monitor; m.Enabled && m.Judge == "model" && m.Model != "" {
		// Not cleared: a judge that may not be asked fails each call closed.
		if err := cfg.ModelListed("monitor", ProviderConfig{Type: "openai-compatible", Model: m.Model}); err != nil {
			aside("monitor.model", m.Model, err)
		}
	}
	var fallback []string
	for _, n := range cfg.Model.Fallback {
		if err := listed(n); err != nil {
			aside("model.fallback", n, err)
			continue
		}
		fallback = append(fallback, n)
	}
	if len(fallback) < len(cfg.Model.Fallback) {
		cfg.Model.Fallback = fallback
	}
	for key, ptr := range map[string]*string{"model.completion": &cfg.Model.Completion, "model.next_edit": &cfg.Model.NextEdit,
		"model.commit": &cfg.Model.Commit, "suggest.model": &cfg.Suggest.Model, "verify.critic.model": &cfg.Verify.Critic.Model} {
		if *ptr == "" {
			continue
		}
		if err := listed(*ptr); err != nil {
			aside(key, *ptr, err)
			// The suggester and the critic then use the session's own model;
			// an editor feature is refused where it asks for its model.
			if key == "suggest.model" || key == "verify.critic.model" {
				*ptr = ""
			}
		}
	}
}

func fileOf(err error) string {
	var me *ManagedError
	if errors.As(err, &me) {
		return me.File
	}
	return ""
}
