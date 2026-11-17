package agent

// EvPluginLoaded lists the approved plugins in force for this run, once,
// before the first message is recorded: what their skills, agents,
// commands, hooks and servers that the record shows came from.
const EvPluginLoaded EventType = "plugin.loaded"

// PluginInForce is one plugin as EvPluginLoaded names it.
type PluginInForce struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	// Basis is why it is trusted: signature, pin, org-pin or org-source.
	Basis  string `json:"basis"`
	Signer string `json:"signer,omitempty"`
}

// PluginsLoaded is EvPluginLoaded's payload.
type PluginsLoaded struct {
	Plugins []PluginInForce `json:"plugins"`
}

// notePlugins records the plugins in force once per loop.
func (l *Loop) notePlugins() error {
	if len(l.Config.Plugins) == 0 {
		return nil
	}
	l.startupMu.Lock()
	defer l.startupMu.Unlock()
	if l.pluginsNoted {
		return nil
	}
	if _, err := l.Recorder.Record(EvPluginLoaded, ActorSystem, Trusted, PluginsLoaded{Plugins: l.Config.Plugins}); err != nil {
		return err
	}
	l.pluginsNoted = true
	return nil
}
