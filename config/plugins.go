package config

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/zybuu-ai/abhed/internal/plugin"
)

// PluginsConfig governs plugin bundles. Managed only: the managed file or
// the signed org policy sets it, and any other layer's is set aside, so a
// workspace can never widen which plugins load.
type PluginsConfig struct {
	// Disabled installs and loads no plugin.
	Disabled bool `json:"disabled,omitempty"`
	// RequireSignature refuses a bundle trusted by a digest pin alone.
	RequireSignature bool `json:"require_signature,omitempty"`
	// AllowedPublishers and AllowedBundles, when either is set, are the only
	// signers and manifest digests a bundle may match; the person's own
	// publishers are then not consulted.
	AllowedPublishers []plugin.Publisher `json:"allowed_publishers,omitempty"`
	AllowedBundles    []string           `json:"allowed_bundles,omitempty"`
	// Sources are the organisation's plugin indexes. When set, a URL install
	// must come from one of them. The machine's managed file names them.
	Sources []plugin.Source `json:"sources,omitempty"`
	// OrgSource adds the org server this machine takes its signed policy
	// from as the source "org" (its /v1/plugins/index). An org policy may
	// set it, as it names no server of its own.
	OrgSource bool `json:"org_source,omitempty"`
	// TrustedRoot is a Sigstore trusted_root.json signatures are checked against.
	TrustedRoot string `json:"trusted_root,omitempty"`
}

// OrgSourceName is the source plugins.org_source adds.
const OrgSourceName = "org"

// OrgPluginIndex is where an org server serves its plugin index.
const OrgPluginIndex = "/v1/plugins/index"

// PluginPolicy is the managed plugin policy in the plugin package's terms,
// with the org server as a source when plugins.org_source says so.
func (c Config) PluginPolicy() plugin.Policy {
	p := c.Plugins
	pol := plugin.Policy{Disabled: p.Disabled, RequireSignature: p.RequireSignature,
		AllowedPublishers: p.AllowedPublishers, AllowedBundles: p.AllowedBundles, Sources: slices.Clone(p.Sources)}
	if p.OrgSource {
		server := c.OrgPolicy.URL
		if server == "" {
			server = c.Org.URL
		}
		if server = strings.TrimRight(server, "/"); server != "" {
			pol.Sources = append(pol.Sources, plugin.Source{Name: OrgSourceName, URL: server + OrgPluginIndex})
		}
	}
	return pol
}

// PluginLayer is the layer a plugin's hooks are credited to.
func PluginLayer(name string) string { return "plugin " + name }

// IsPluginLayer reports a layer PluginLayer made.
func IsPluginLayer(layer string) bool { return strings.HasPrefix(layer, "plugin ") }

var abhedVersion atomic.Value

// SetVersion tells loading which Abhed version runs, for a plugin's minimum.
func SetVersion(v string) { abhedVersion.Store(v) }

func runningVersion() string {
	v, _ := abhedVersion.Load().(string)
	return v
}

// pluginStore is where loading finds the person's plugins; tests move it.
var pluginStore = plugin.DefaultStore

// applyPlugins adds each approved plugin that today's policy admits: its
// hooks and MCP servers into the configuration, under names carrying the
// plugin's, and its agents, skills and commands to LoadedPlugins for their
// loaders. A plugin whose names clash, or whose hooks would not run as
// written, is left out whole.
func applyPlugins(cfg *Config, w warnSink) {
	s, err := pluginStore()
	if err != nil {
		return
	}
	loaded, warns := plugin.LoadAll(s, cfg.PluginPolicy(), runningVersion())
	for _, m := range warns {
		w.emit("plugin\x00"+m, m)
	}
	for _, l := range loaded {
		if why := pluginClash(*cfg, l); why != "" {
			m := fmt.Sprintf("plugin %s is not loaded: %s", l.Name, why)
			w.emit("plugin\x00"+m, m)
			continue
		}
		var hooks []HookConfig
		for _, h := range l.Hooks {
			hooks = append(hooks, HookConfig{Name: h.Name, Events: h.Events, Command: h.Command, URL: h.URL,
				Match: h.Match, TimeoutMS: h.TimeoutMS, Async: h.Async, Layer: PluginLayer(l.Name)})
		}
		if err := (Config{Hooks: HooksConfig{Run: hooks}}).validateHooks(); err != nil {
			m := fmt.Sprintf("plugin %s is not loaded: %v", l.Name, err)
			w.emit("plugin\x00"+m, m)
			continue
		}
		cfg.Hooks.Run = append(cfg.Hooks.Run, hooks...)
		for _, sv := range l.MCPServers {
			// Never with the host's files, and no credential but the environment's.
			cfg.MCP.Servers = append(cfg.MCP.Servers, MCPServerConfig{Name: sv.Name, Command: sv.Command, Args: sv.Args,
				URL: sv.URL, HeadersEnv: sv.HeadersEnv, Enabled: true, AllowTools: sv.AllowTools, Digest: sv.Digest})
		}
		cfg.LoadedPlugins = append(cfg.LoadedPlugins, l)
	}
}

// pluginClash is why l's hooks or servers cannot take their names, or "".
func pluginClash(cfg Config, l plugin.Loaded) string {
	for _, h := range l.Hooks {
		for _, x := range cfg.Hooks.Run {
			if x.Name == h.Name {
				return fmt.Sprintf("a hook named %s comes from the %s configuration", h.Name, orLayer(x.Layer))
			}
		}
		for _, e := range cfg.Extensions {
			if e.Name == h.Name {
				return fmt.Sprintf("an extension is named %s", h.Name)
			}
		}
	}
	for _, sv := range l.MCPServers {
		for _, x := range cfg.MCP.Servers {
			if strings.EqualFold(x.Name, sv.Name) {
				return fmt.Sprintf("an MCP server named %s is already configured", sv.Name)
			}
		}
	}
	return ""
}

// PluginServer names the plugin an MCP server came from, or "".
func (c Config) PluginServer(name string) string {
	for _, l := range c.LoadedPlugins {
		for _, s := range l.MCPServers {
			if s.Name == name {
				return l.Name
			}
		}
	}
	return ""
}

// DropPlugins leaves every plugin out, as a bare run does.
func (c *Config) DropPlugins() {
	if len(c.LoadedPlugins) == 0 {
		return
	}
	kept := c.Hooks.Run[:0:0]
	for _, h := range c.Hooks.Run {
		if !IsPluginLayer(h.Layer) {
			kept = append(kept, h)
		}
	}
	c.Hooks.Run = kept
	servers := c.MCP.Servers[:0:0]
	for _, s := range c.MCP.Servers {
		if c.PluginServer(s.Name) == "" {
			servers = append(servers, s)
		}
	}
	c.MCP.Servers = servers
	c.LoadedPlugins = nil
}
