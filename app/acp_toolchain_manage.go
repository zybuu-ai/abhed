package app

import (
	"encoding/json"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/toolchain"
)

// Managing installed toolchains from Studio (studio-acp-contract.md §3.2b,
// feature toolchain.manage): remove one language's, clean the whole cache
// after asking the person, and open an installed language's registries.
// Each is the person's own act, refused inside an agent's command, recorded.

// toolchainRemove removes a language's installed toolchains for the
// session's platform: the version named, else every one.
func (c *acpConn) toolchainRemove(msg rpcMessage) {
	s, _, tc, spec, e := c.toolchainSession(msg)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if why := inAgentCommand(); why != "" {
		c.reply(msg.ID, nil, toolchainError(errPolicy, "in_agent", "removing a toolchain is refused inside an agent's command ("+why+")"))
		return
	}
	p := tc.Platform()
	var targets []toolchain.Installed
	for _, in := range tc.Cache.List(p, spec.Lang) {
		if toolchain.Matches(in.Version, spec.Version) {
			targets = append(targets, in)
		}
	}
	if len(targets) == 0 {
		c.reply(msg.ID, nil, toolchainError(errParams, "not_installed", spec.String()+" is not installed for "+p.String()))
		return
	}
	removed := []string{}
	for _, in := range targets {
		err := tc.Cache.Remove(p, in.Lang, in.Version)
		ev := agent.ToolchainAction{Lang: in.Lang, Version: in.Version, Platform: p.String(), Outcome: agent.ToolchainRemoved,
			By: agent.ByUser, Surface: "acp", Consent: "studio"}
		if err != nil {
			ev.Outcome, ev.Error = agent.ToolchainFailed, err.Error()
		} else {
			removed = append(removed, in.Version)
		}
		s.record(agent.EvToolchainInstall, ev)
		if err != nil {
			c.reply(msg.ID, nil, toolchainError(errRefused, "remove_failed", err.Error()))
			return
		}
	}
	c.reply(msg.ID, map[string]any{"language": spec.Lang, "platform": p.String(), "removed": removed}, nil)
}

// toolchainClean removes the whole toolchain cache, every toolchain and
// extra for every platform, once the person says so in Studio's own prompt.
func (c *acpConn) toolchainClean(msg rpcMessage) {
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	parts, e := innerOf(s)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if why := inAgentCommand(); why != "" {
		c.reply(msg.ID, nil, toolchainError(errPolicy, "in_agent", "cleaning the toolchain cache is refused inside an agent's command ("+why+")"))
		return
	}
	if parts.Set == nil || parts.Set.Toolchains == nil {
		c.reply(msg.ID, nil, toolchainError(errPolicy, "unavailable", "toolchains are not available in this session"))
		return
	}
	root := parts.Set.Toolchains.Cache.Root
	bind := "clean-" + acpID()
	res, err := c.call(c.root(), "session/request_permission", map[string]any{
		"sessionId": s.id,
		"toolCall": map[string]any{"toolCallId": bind, "title": "Remove every toolchain in " + root, "kind": "delete", "status": "pending",
			"rawInput": map[string]any{"root": root},
			"_meta": map[string]any{acpMetaKey: map[string]any{"tool": "toolchain.clean", "step": "person", "destructive": true,
				"reason": "removes every installed toolchain, debugger and language server, for every platform"}}},
		"options": []map[string]any{{"optionId": "once:" + bind, "name": "Remove them all", "kind": "allow_once"},
			{"optionId": "reject:" + bind, "name": "Keep them", "kind": "reject_once"}},
	})
	var out struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err != nil || json.Unmarshal(res, &out) != nil || out.Outcome.Outcome != "selected" || out.Outcome.OptionID != "once:"+bind {
		s.record(agent.EvToolchainInstall, agent.ToolchainAction{Lang: "*", Outcome: agent.ToolchainDeclined, By: agent.ByUser, Surface: "acp", Consent: "studio"})
		c.reply(msg.ID, map[string]any{"cleaned": false, "root": root}, nil)
		return
	}
	ev := agent.ToolchainAction{Lang: "*", Outcome: agent.ToolchainRemoved, By: agent.ByUser, Surface: "acp", Consent: "studio"}
	if err := (toolchain.Cache{Root: root}).Clean(); err != nil {
		ev.Outcome, ev.Error = agent.ToolchainFailed, err.Error()
		s.record(agent.EvToolchainInstall, ev)
		c.reply(msg.ID, nil, toolchainError(errRefused, "clean_failed", err.Error()))
		return
	}
	s.record(agent.EvToolchainInstall, ev)
	c.reply(msg.ID, map[string]any{"cleaned": true, "root": root}, nil)
}

// toolchainRegistries opens an installed language's package registries to
// the session's commands, as an install's agreed.registries does: only the
// hosts status offered, named back as the person saw them.
func (c *acpConn) toolchainRegistries(msg rpcMessage) {
	s, _, tc, spec, e := c.toolchainSession(msg)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if why := inAgentCommand(); why != "" {
		c.reply(msg.ID, nil, toolchainError(errPolicy, "in_agent", "opening registries is refused inside an agent's command ("+why+")"))
		return
	}
	var p struct {
		Registries *[]string `json:"registries"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	installed := false
	for _, in := range tc.Active(tc.Workspace) {
		installed = installed || in.Lang == spec.Lang
	}
	if !installed {
		c.reply(msg.ID, nil, toolchainError(errRefused, "not_installed", spec.Lang+" is not installed for the session's commands; install it first"))
		return
	}
	hosts, why := registriesOffer(tc, spec.Lang)
	switch {
	case why != "":
		c.reply(msg.ID, nil, toolchainError(errPolicy, "registries_closed", why))
		return
	case p.Registries == nil || !sameHosts(hosts, *p.Registries):
		c.reply(msg.ID, nil, toolchainError(errRefused, "changed", "the registries agreed to are not the ones offered; ask for the offer again"))
		return
	}
	note := tc.OpenRegistriesFor(spec.Lang, "acp", func(r agent.ToolchainRegistry) { s.record(agent.EvToolchainRegistry, r) })
	c.reply(msg.ID, map[string]any{"language": spec.Lang, "hosts": hosts, "registries": note}, nil)
}
