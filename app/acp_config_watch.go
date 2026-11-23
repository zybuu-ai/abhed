package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// When the person's own ~/.abhed/config.json changes while chats are open,
// Studio is told (_abhed/config/changed) that the change applies to new
// chats, and what of it can be applied to an open chat now: models added,
// and a new default model to switch to (_abhed/config/apply). Nothing that
// widens what the agent may do is applied to an open chat; it takes a new
// chat, or the ordinary approval when the agent asks.

func init() {
	liveFeatures = append(liveFeatures, "config_watch")
	handle(map[string]func(*acpConn, rpcMessage){"_abhed/config/apply": (*acpConn).configApply})
}

// configWatchEvery is how often the person's file is looked at; tests set it.
var configWatchEvery = 2 * time.Second

// userConfigFile is the person's own configuration file; tests set it.
var userConfigFile = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".abhed", "config.json")
}

// openChatChange is what changed in the person's file, sorted into what an
// open chat can take and what waits for a new one.
type openChatChange struct {
	// Models are providers added; Default is a new model.default.
	Models  []string `json:"models,omitempty"`
	Default string   `json:"default,omitempty"`
	// NewChats are the other settings changed, by dotted path: they apply
	// to new chats only.
	NewChats []string `json:"newChats,omitempty"`
}

func (c openChatChange) empty() bool {
	return len(c.Models) == 0 && c.Default == "" && len(c.NewChats) == 0
}

// diffUserConfig sorts the change from one version of the file to another.
func diffUserConfig(before, after []byte) openChatChange {
	var a, b map[string]any
	_ = json.Unmarshal(before, &a)
	_ = json.Unmarshal(after, &b)
	flat := func(m map[string]any) map[string]string {
		out := map[string]string{}
		var walk func(prefix string, v any, depth int)
		walk = func(prefix string, v any, depth int) {
			if mm, ok := v.(map[string]any); ok && depth < 3 {
				for k, vv := range mm {
					p := k
					if prefix != "" {
						p = prefix + "." + k
					}
					// A provider is one setting: its fields change together.
					if strings.HasPrefix(p, "model.providers.") && strings.Count(p, ".") == 2 {
						j, _ := json.Marshal(vv)
						out[p] = string(j)
						continue
					}
					walk(p, vv, depth+1)
				}
				return
			}
			j, _ := json.Marshal(v)
			out[prefix] = string(j)
		}
		walk("", m, 0)
		return out
	}
	fa, fb := flat(a), flat(b)
	var ch openChatChange
	keys := map[string]bool{}
	for k := range fa {
		keys[k] = true
	}
	for k := range fb {
		keys[k] = true
	}
	for k := range keys {
		if fa[k] == fb[k] {
			continue
		}
		switch {
		case strings.HasPrefix(k, "model.providers.") && fa[k] == "" && fb[k] != "":
			ch.Models = append(ch.Models, strings.TrimPrefix(k, "model.providers."))
		case k == "model.default" && fb[k] != "":
			var d string
			_ = json.Unmarshal([]byte(fb[k]), &d)
			ch.Default = d
		default:
			ch.NewChats = append(ch.NewChats, k)
		}
	}
	sort.Strings(ch.Models)
	sort.Strings(ch.NewChats)
	return ch
}

// message says the change as Studio shows it.
func (c openChatChange) message() string {
	var parts []string
	if len(c.NewChats) > 0 {
		parts = append(parts, "Your configuration changed ("+strings.Join(c.NewChats, ", ")+"): it applies to new chats. "+
			"An open chat keeps what it started with; anything that lets the agent do more is never applied to it.")
	}
	switch {
	case len(c.Models) > 0 && c.Default != "":
		parts = append(parts, fmt.Sprintf("Models added (%s) and a new default (%s) can be used in this chat now.", strings.Join(c.Models, ", "), c.Default))
	case len(c.Models) > 0:
		parts = append(parts, fmt.Sprintf("Models added (%s) can be offered in this chat now.", strings.Join(c.Models, ", ")))
	case c.Default != "":
		parts = append(parts, fmt.Sprintf("This chat can switch to the new default model, %s, now.", c.Default))
	}
	return strings.Join(parts, " ")
}

// watchUserConfig looks at the person's file until the connection ends,
// and tells Studio of each change, for every open chat.
func (c *acpConn) watchUserConfig() {
	file := userConfigFile()
	if file == "" {
		return
	}
	last, _ := os.ReadFile(file) // #nosec G304 -- the person's own ~/.abhed/config.json
	tick := time.NewTicker(configWatchEvery)
	defer tick.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-tick.C:
		}
		now, err := os.ReadFile(file) // #nosec G304 -- the person's own ~/.abhed/config.json
		if err != nil && !os.IsNotExist(err) || bytes.Equal(now, last) {
			continue
		}
		// A file half written is read again on the next look.
		if len(now) > 0 && !json.Valid(now) {
			continue
		}
		ch := diffUserConfig(last, now)
		last = now
		if ch.empty() {
			continue
		}
		var ids []string
		for _, s := range c.openSessions() {
			ids = append(ids, s.id)
		}
		if len(ids) == 0 {
			continue
		}
		c.notification("_abhed/config/changed", map[string]any{"sessionIds": ids, "change": ch, "message": ch.message(),
			"canApply": len(ch.Models) > 0 || ch.Default != ""})
	}
}

// configApply gives an open chat what of the changed file it can take: the
// models added, offered in its model list, and, when asked, a switch to the
// new default. Nothing else of the file applies to it.
func (c *acpConn) configApply(msg rpcMessage) {
	var p struct {
		SwitchDefault bool `json:"switchDefault"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if why := inAgentCommand(); why != "" {
		c.reply(msg.ID, nil, refusal(errPolicy, "applying configuration is refused inside an agent's command (%s)", why))
		return
	}
	adder, ok := s.agent.(interface {
		AddModels(config.Config) []string
	})
	if !ok || !s.inner {
		c.reply(msg.ID, nil, refusal(errRefused, "this session cannot take new models"))
		return
	}
	fresh, err := config.LoadWith(s.cwd, config.LoadOptions{Quiet: true})
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "the configuration cannot be read: %v", err))
		return
	}
	added := adder.AddModels(fresh)
	res := map[string]any{"added": added}
	if p.SwitchDefault && fresh.Model.Default != "" {
		m, ok := s.agent.(modelSwitcher)
		if !ok {
			c.reply(msg.ID, nil, refusal(errRefused, "this session cannot switch models"))
			return
		}
		if err := m.SwitchModelNamed(fresh.Model.Default); err != nil {
			c.reply(msg.ID, nil, refusal(errRefused, "%s", err.Error()))
			return
		}
		res["switchedTo"] = fresh.Model.Default
	}
	if opts := c.configOptions(s); len(opts) > 0 && (len(added) > 0 || res["switchedTo"] != nil) {
		c.sessionUpdate(s.id, map[string]any{"sessionUpdate": "config_option_update", "configOptions": opts})
	}
	if len(added) > 0 || res["switchedTo"] != nil {
		s.record(agent.EvConfigApplied, map[string]any{"models_added": added, "switched_to": res["switchedTo"]})
	}
	c.reply(msg.ID, res, nil)
}

// configChangedNote is what to tell the person at the terminal when their
// file changed since this session last looked, or "". The first call only
// takes the file as it is.
func (st *cliState) configChangedNote() string {
	file := userConfigFile()
	if file == "" {
		return ""
	}
	now, err := os.ReadFile(file) // #nosec G304 -- the person's own ~/.abhed/config.json
	if err != nil && !os.IsNotExist(err) || len(now) > 0 && !json.Valid(now) {
		return ""
	}
	if !st.cfgSeenSet {
		st.cfgSeen, st.cfgSeenSet = now, true
		return ""
	}
	if bytes.Equal(now, st.cfgSeen) {
		return ""
	}
	ch := diffUserConfig(st.cfgSeen, now)
	st.cfgSeen = now
	if ch.empty() {
		return ""
	}
	note := ch.message()
	if len(ch.Models) > 0 {
		note += " /config apply offers the added models here; /model switches."
	} else if ch.Default != "" {
		note += " /model " + ch.Default + " switches this session."
	}
	return note
}

// configApplyCLI offers this session the models added to the person's
// configuration since it started; nothing else of the file applies to it.
func configApplyCLI(e *cmdEnv) error {
	fresh, err := config.LoadWith(e.st.workspace, config.LoadOptions{Quiet: true})
	if err != nil {
		return fmt.Errorf("the configuration cannot be read: %w", err)
	}
	cur := e.st.appCfg
	next := make(map[string]config.ProviderConfig, len(cur.Model.Providers))
	for n, p := range cur.Model.Providers {
		next[n] = p
	}
	var added []string
	for n, p := range fresh.Model.Providers {
		if _, have := next[n]; have || fresh.ModelAllowed(n) != nil || cur.EndpointRefused(p) != nil {
			continue
		}
		next[n] = p
		added = append(added, n)
	}
	if len(added) == 0 {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "no models were added to your configuration since this session started; other changes apply to new sessions"})
		return nil
	}
	sort.Strings(added)
	e.st.appCfg.Model.Providers = next
	if e.st.loop != nil {
		e.st.recordCLI(agent.EvConfigApplied, map[string]any{"models_added": added})
	}
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "offered here now: " + strings.Join(added, ", ") + "; /model switches to one"})
	return nil
}
