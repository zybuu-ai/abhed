package app

import (
	"encoding/json"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/toolset"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// The person opens the network to a workspace's commands from Studio, where
// it is off: for the workspace (kept), one host (kept), or this chat alone.
// The default stays off; where the managed file holds the network nothing
// is offered. A command that failed for the network is told to Studio
// (_abhed/sandbox/network_refused) so it can offer the three choices.

func init() {
	liveFeatures = append(liveFeatures, "network")
	handle(map[string]func(*acpConn, rpcMessage){"_abhed/sandbox/network": (*acpConn).sandboxNetwork})
}

// sandboxNetwork opens the network on the person's word: to this session at
// once, to this process's other open sessions of the workspace for a kept
// grant, and, kept, to every later session there.
func (c *acpConn) sandboxNetwork(msg rpcMessage) {
	var p struct {
		Scope string `json:"scope"`
		Host  string `json:"host"`
	}
	_ = json.Unmarshal(msg.Params, &p)
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
		c.reply(msg.ID, nil, refusal(errPolicy, "opening the network is refused inside an agent's command (%s)", why))
		return
	}
	if parts.Set == nil || parts.Set.Network == nil {
		c.reply(msg.ID, nil, refusal(errRefused, "this session has no sandbox to open"))
		return
	}
	opened, err := parts.Set.Network.Open(p.Scope, p.Host, true)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errPolicy, "%s", err.Error()))
		return
	}
	scope, rule := "session", "network(any host)"
	if p.Host != "" {
		rule = "network(" + strings.ToLower(strings.TrimSpace(p.Host)) + ")"
	}
	if p.Scope != "once" {
		scope = "workspace"
		root := grantsKey(parts.Session.Root)
		for _, other := range c.openSessions() {
			if other == s || !other.inner || other.parts.Session == nil || other.parts.Set == nil || other.parts.Set.Network == nil ||
				grantsKey(other.parts.Session.Root) != root {
				continue
			}
			_, _ = other.parts.Set.Network.Open(p.Scope, p.Host, false)
		}
	}
	s.record(agent.EvPermissionChanged, agent.PermissionChanged{Op: "add", List: "allow", Rule: rule, Scope: scope, By: agent.ByUser})
	c.reply(msg.ID, map[string]any{"opened": opened}, nil)
}

// networkRefused tells Studio, once per turn, that a command failed because
// the network is off, with whether the person can open it here.
func (c *acpConn) networkRefused(s *acpSession, ev abhed.Event) {
	if ev.Type != agent.EvObservation || !s.inner || s.parts.Set == nil || s.parts.Set.Network == nil {
		return
	}
	var o agent.Observation
	if json.Unmarshal(ev.Payload, &o) != nil || o.Tool != "bash" || !strings.Contains(o.Content, toolset.NetworkOfferText) {
		return
	}
	s.mu.Lock()
	told := s.networkTold
	s.networkTold = true
	s.mu.Unlock()
	if told {
		return
	}
	canOpen, why := s.parts.Set.Network.Offer()
	c.notification("_abhed/sandbox/network_refused", map[string]any{"sessionId": s.id, "canOpen": canOpen, "why": why,
		"message": "A command could not reach the network: it is off for this workspace's commands.",
		"choices": []string{"workspace", "host", "once"}})
}
