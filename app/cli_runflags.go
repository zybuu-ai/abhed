package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/agentdefs"
	"github.com/zybuu-ai/abhed/internal/customcmd"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// maxFlagFile bounds a file a flag names: -settings, -mcp-config.
const maxFlagFile = 1 << 20

// flagFiles reads the files the run's flags name. One inside the workspace
// is content the agent can write between runs, so, as the workspace's own
// configuration, it is read only when the workspace is trusted for this run.
type flagFiles struct {
	workspace string
	trusted   bool
}

func newFlagFiles(workspace string, trust config.TrustChoice) flagFiles {
	v := os.Getenv(config.TrustEnv)
	return flagFiles{workspace: workspace, trusted: trust == config.TrustGranted || v == "1" || strings.EqualFold(v, "true")}
}

// read is a flag's JSON: inline when it starts with {, otherwise the regular
// file it names. name says which, for warnings and the record.
func (ff flagFiles) read(flagName, v string) (data []byte, name string, err error) {
	if strings.HasPrefix(strings.TrimSpace(v), "{") {
		return []byte(v), flagName + " (inline)", nil
	}
	if abs, err := filepath.Abs(v); err == nil && ff.workspace != "" && !ff.trusted && customcmd.Inside(ff.workspace, abs) {
		home, _ := os.UserHomeDir()
		// A workspace holding the home directory holds every file; only the flag's own consent is left.
		if home == "" || !customcmd.Inside(ff.workspace, home) {
			return nil, "", fmt.Errorf("%s: %s is inside the workspace, which the agent can change; "+
				"pass -trust-workspace to use it, or keep it outside the workspace", flagName, config.Printable(v))
		}
	}
	f, err := os.Open(v) // #nosec G304 -- a file the person named on the command line
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", flagName, err)
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%s: %s is not a regular file", flagName, config.Printable(v))
	}
	data, err = io.ReadAll(io.LimitReader(f, maxFlagFile+1))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", flagName, err)
	}
	if len(data) > maxFlagFile {
		return nil, "", fmt.Errorf("%s: %s is larger than %d bytes", flagName, config.Printable(v), maxFlagFile)
	}
	return data, v, nil
}

// flagSource is what the record keeps of a file a flag named.
type flagSource struct {
	Source  string   `json:"source"`
	SHA256  string   `json:"sha256"`
	Servers []string `json:"servers,omitempty"`
}

func sourceOf(name string, data []byte) flagSource {
	sum := sha256.Sum256(data)
	return flagSource{Source: name, SHA256: hex.EncodeToString(sum[:])}
}

// mcpFileServer is one server in the common mcpServers file shape.
type mcpFileServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// parseMCPConfig reads an -mcp-config file: {"mcpServers": {name: {...}}},
// or Abhed's own {"mcp": {"servers": [...]}}. Naming the file is consent, so
// each server is enabled unless the file says "enabled": false.
func parseMCPConfig(data []byte, name string) ([]config.MCPServerConfig, error) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("-mcp-config %s: %w", config.Printable(name), err)
	}
	var out []config.MCPServerConfig
	for key, raw := range top {
		switch key {
		case "mcpServers":
			var servers map[string]mcpFileServer
			d := json.NewDecoder(bytes.NewReader(raw))
			d.DisallowUnknownFields()
			if err := d.Decode(&servers); err != nil {
				return nil, fmt.Errorf("-mcp-config %s: mcpServers: %w", config.Printable(name), err)
			}
			for n, s := range servers {
				switch s.Type {
				case "", "stdio", "http", "sse", "streamable-http":
				default:
					return nil, fmt.Errorf("-mcp-config %s: server %s: type %s is not stdio or http", config.Printable(name), config.Printable(n), config.Printable(s.Type))
				}
				c := config.MCPServerConfig{Name: n, Command: s.Command, Args: s.Args, URL: s.URL, Headers: s.Headers, Enabled: true}
				for _, k := range sortedKeys(s.Env) {
					c.Env = append(c.Env, k+"="+s.Env[k])
				}
				out = append(out, c)
			}
		case "mcp":
			var m struct {
				Servers []json.RawMessage `json:"servers"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, fmt.Errorf("-mcp-config %s: mcp: %w", config.Printable(name), err)
			}
			for _, r := range m.Servers {
				c := config.MCPServerConfig{Enabled: true}
				if err := json.Unmarshal(r, &c); err != nil {
					return nil, fmt.Errorf("-mcp-config %s: mcp.servers: %w", config.Printable(name), err)
				}
				out = append(out, c)
			}
		default:
			return nil, fmt.Errorf("-mcp-config %s: unknown key %s; want mcpServers or mcp", config.Printable(name), config.Printable(key))
		}
	}
	for _, s := range out {
		switch {
		case !mcpServerName.MatchString(s.Name):
			return nil, fmt.Errorf("-mcp-config %s: server name %s: letters, digits, _ and - only, up to 64", config.Printable(name), config.Printable(s.Name))
		case (s.Command == "") == (s.URL == ""):
			return nil, fmt.Errorf("-mcp-config %s: server %s needs a command or a url, not both", config.Printable(name), s.Name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// mcpFlags applies -mcp-config and -strict-mcp-config. The servers named are
// added, or with strict are the only ones; a managed mcp section refuses
// added servers, and strict then keeps the organisation's.
func mcpFlags(cfg config.Config, ff flagFiles, files []string, strict bool) (config.Config, []flagSource, error) {
	var added []config.MCPServerConfig
	var sources []flagSource
	for _, v := range files {
		data, name, err := ff.read("-mcp-config", v)
		if err != nil {
			return cfg, nil, err
		}
		servers, err := parseMCPConfig(data, name)
		if err != nil {
			return cfg, nil, err
		}
		src := sourceOf(name, data)
		for _, s := range servers {
			if slices.ContainsFunc(added, func(a config.MCPServerConfig) bool { return a.Name == s.Name }) {
				return cfg, nil, fmt.Errorf("-mcp-config: server %s is named twice", s.Name)
			}
			added = append(added, s)
			src.Servers = append(src.Servers, s.Name)
		}
		sources = append(sources, src)
	}
	if !strict && len(added) == 0 {
		return cfg, nil, nil
	}
	if cfg.ManagedSets("mcp") {
		if len(added) > 0 {
			return cfg, nil, &config.ManagedError{Key: "mcp.servers", Value: "-mcp-config", File: managed.ConfigFile,
				Reason: "the managed configuration sets the MCP servers"}
		}
		warnf("-strict-mcp-config: the managed configuration sets the MCP servers, so they stay")
		return cfg, nil, nil
	}
	var servers []config.MCPServerConfig
	if !strict {
		for _, s := range cfg.MCP.Servers {
			if !slices.ContainsFunc(added, func(a config.MCPServerConfig) bool { return a.Name == s.Name }) {
				servers = append(servers, s)
			}
		}
	}
	servers = append(servers, added...)
	cfg.MCP.Servers = servers
	return cfg, sources, nil
}

// sessionAgents parses -agents. A definition that is refused ends the run:
// a script that named it expects it.
func sessionAgents(cfg config.Config, ff flagFiles, v string) ([]*agent.Definition, *flagSource, error) {
	if v == "" {
		return nil, nil, nil
	}
	data, name, err := ff.read("-agents", v)
	if err != nil {
		return nil, nil, err
	}
	defs, warns, errs := agentdefs.ParseSession(data, toolset.OfferedModels(cfg))
	for _, w := range warns {
		warnf("%s", w)
	}
	if len(errs) > 0 {
		return nil, nil, errors.Join(errs...)
	}
	src := sourceOf(name, data)
	for _, d := range defs {
		src.Servers = append(src.Servers, d.Name)
	}
	return defs, &src, nil
}

// roleFor finds the definition -agent names among every one the session
// would offer. A worktree role is refused: it runs only as a subagent.
func roleFor(cfg config.Config, session []*agent.Definition, name string) (*agent.Definition, error) {
	if name == "" {
		return nil, nil
	}
	defs := toolset.LoadAgents(cfg, cfg.Workspace, session, func(string, ...any) {})
	def, ok := defs.Get(name)
	if !ok {
		return nil, fmt.Errorf("-agent %s: no such agent type; available: %s", config.Printable(name), strings.Join(defs.Names(), ", "))
	}
	if def.Isolation == "worktree" {
		return nil, fmt.Errorf("-agent %s works in its own worktree, so it runs only as a subagent", name)
	}
	return def, nil
}

// roleSection is a role's instructions as the session's prompt carries them.
func roleSection(def *agent.Definition) string {
	if def == nil || strings.TrimSpace(def.Instruction) == "" {
		return ""
	}
	return "\n\n## Role\nThis session runs as the " + def.Name + " agent.\n" + def.Instruction
}

// recordRunFlags puts what -settings, -mcp-config, -agents and -agent
// brought into the session's start event, by name and hash.
func recordRunFlags(start map[string]any, cfg config.Config, mcp []flagSource, strict bool, agents *flagSource, role *agent.Definition) {
	if cfg.Settings.Name != "" {
		start["settings"] = flagSource{Source: cfg.Settings.Name, SHA256: cfg.Settings.SHA256}
	}
	if len(mcp) > 0 {
		start["mcp_config"] = mcp
	}
	if strict {
		start["strict_mcp_config"] = true
	}
	if agents != nil {
		start["agents"] = agents
	}
	if role != nil {
		start["agent"] = map[string]any{"name": role.Name, "source": role.Source, "sha256": role.SHA256}
	}
}
