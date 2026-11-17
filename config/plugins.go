package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"

	"github.com/zybuu-ai/abhed/plugin"
)

// PluginsConfig governs plugin bundles. Managed only: the managed file or
// the signed org policy sets it, and any other layer's is set aside, so a
// workspace can never widen which plugins load.
type PluginsConfig struct {
	// Disabled installs and loads no plugin.
	Disabled bool `json:"disabled,omitempty"`
	// RequireSignature refuses a bundle trusted by a digest pin alone.
	RequireSignature bool `json:"require_signature,omitempty"`
	// AllowedPublishers and AllowedBundles, when either is set, are the
	// signers and manifest digests a bundle must match: one of them signed it,
	// or its digest is listed. The org policy's lists are a layer of their
	// own, which a bundle must pass too, so neither layer widens the other.
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

// orgPlugins is the org policy's plugins section, kept apart from the
// machine's managed file: each layer must admit a bundle.
type orgPlugins struct {
	PluginsConfig
	// pinned is whether the machine pins the org's signing keys. Only then
	// may the org's lists and source grant trust; otherwise they only narrow.
	pinned bool
}

// PluginPolicy is the plugin policy in the plugin package's terms: the
// machine's managed file and the org policy each a layer, with the org
// server as a source when plugins.org_source says so.
func (c Config) PluginPolicy() plugin.Policy {
	p := c.Plugins
	pol := plugin.Policy{Disabled: p.Disabled, RequireSignature: p.RequireSignature}
	for _, s := range p.Sources {
		s.Trusted = true
		pol.Sources = append(pol.Sources, s)
	}
	if len(p.AllowedPublishers) > 0 || len(p.AllowedBundles) > 0 {
		pol.Lists = append(pol.Lists, plugin.AllowList{Layer: "managed configuration's", Publishers: p.AllowedPublishers,
			Bundles: p.AllowedBundles, Grants: true})
	}
	orgSource, orgSourceTrusted := p.OrgSource, p.OrgSource
	if o := c.orgPlugins; o != nil {
		pol.Disabled = pol.Disabled || o.Disabled
		pol.RequireSignature = pol.RequireSignature || o.RequireSignature
		if len(o.AllowedPublishers) > 0 || len(o.AllowedBundles) > 0 {
			pol.Lists = append(pol.Lists, plugin.AllowList{Layer: "org policy's", Publishers: o.AllowedPublishers,
				Bundles: o.AllowedBundles, Grants: o.pinned})
		}
		if o.OrgSource {
			orgSource = true
			orgSourceTrusted = orgSourceTrusted || o.pinned
		}
	}
	if orgSource {
		server := c.OrgPolicy.URL
		if server == "" {
			server = c.Org.URL
		}
		if server = strings.TrimRight(server, "/"); server != "" {
			pol.Sources = append(pol.Sources, plugin.Source{Name: OrgSourceName, URL: server + OrgPluginIndex, Trusted: orgSourceTrusted})
		}
	}
	return pol
}

// PluginRoots are the Sigstore trusted roots signatures are checked
// against. The org root, which alone counts toward an allow list or
// require_signature, is the managed file's plugins.trusted_root, else the
// public-good root this build carries; never one the person chose. The
// person's root, for their own publishers, is their own copy (s's), else
// the org root.
func (c Config) PluginRoots(s *plugin.Store) (plugin.Roots, error) {
	r := plugin.Roots{Org: plugin.PublicGoodRoot()}
	if p := c.Plugins.TrustedRoot; p != "" {
		root, err := plugin.ReadTrustedRoot(p)
		if err != nil {
			return plugin.Roots{}, fmt.Errorf("plugins.trusted_root: %w", err)
		}
		r.Org = root
	}
	r.Person = r.Org
	if s != nil {
		if _, err := os.Stat(s.TrustedRootFile()); err == nil {
			root, err := plugin.ReadTrustedRoot(s.TrustedRootFile())
			if err != nil {
				return r, err
			}
			r.Person = root
		}
	}
	return r, nil
}

// LoadedPlugins are the approved plugins this load added, in name order.
// Experimental: its element type is package plugin's, which may change in a
// minor release until 2.0.
func (c Config) LoadedPlugins() []plugin.Loaded { return c.loadedPlugins }

// validatePlugins refuses a plugins section that could not be applied as written.
func (p PluginsConfig) validate() error {
	for _, pub := range p.AllowedPublishers {
		if err := pub.Valid(); err != nil {
			return fmt.Errorf("plugins.allowed_publishers: %w", err)
		}
	}
	for _, d := range p.AllowedBundles {
		if len(d) != 64 || strings.Trim(d, "0123456789abcdef") != "" {
			return fmt.Errorf("plugins.allowed_bundles: %q is not a manifest's SHA-256 (64 lower-case hex digits)", d)
		}
	}
	for _, s := range p.Sources {
		if u, err := url.Parse(s.URL); s.Name == "" || err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("plugins.sources: %q needs a name and an https URL", s.Name)
		}
	}
	return nil
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
// plugin's, and its agents, skills and commands to loadedPlugins for their
// loaders. A plugin whose names clash, or whose hooks would not run as
// written, is left out whole.
func applyPlugins(cfg *Config, w warnSink) {
	s, err := pluginStore()
	if err != nil {
		return
	}
	roots, err := cfg.PluginRoots(s)
	if err != nil {
		m := "plugins: a Sigstore trusted root does not read: " + err.Error()
		w.emit("plugin\x00"+m, m)
	}
	loaded, warns := plugin.LoadAll(s, cfg.PluginPolicy(), runningVersion(), roots)
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
		cfg.loadedPlugins = append(cfg.loadedPlugins, l)
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
	for _, l := range c.loadedPlugins {
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
	if len(c.loadedPlugins) == 0 {
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
	c.loadedPlugins = nil
}
