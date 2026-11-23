package app

import (
	"encoding/json"

	"github.com/zybuu-ai/abhed/internal/machinegrant"
)

// What the person let Abhed use of their own machine setup is theirs, not a
// session's: these need no session, and change no session's own approvals.

func init() {
	liveFeatures = append(liveFeatures, "machine")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/machine/list":   (*acpConn).machineList,
		"_abhed/machine/forget": (*acpConn).machineForget,
	})
}

func (c *acpConn) machineList(msg rpcMessage) {
	gs, err := machinegrant.List()
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "the kept grants cannot be read: %v", err))
		return
	}
	out := []map[string]any{}
	for _, g := range gs {
		// Where a kept login goes and which secret it reads, never the token or its certificate.
		out = append(out, map[string]any{"kind": g.Kind, "name": g.Name, "what": g.What, "grantedAt": g.GrantedAt})
	}
	c.reply(msg.ID, map[string]any{"grants": out}, nil)
}

func (c *acpConn) machineForget(msg rpcMessage) {
	var p struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if json.Unmarshal(msg.Params, &p) != nil || !machinegrant.ValidKind(p.Kind) || p.Name == "" {
		c.reply(msg.ID, nil, refusal(errParams, "kind (k8s-context, ssh-host or k8s-login) and name are required"))
		return
	}
	if why := inAgentCommand(); why != "" {
		c.reply(msg.ID, nil, refusal(errPolicy, "forgetting a grant is refused inside an agent's command (%s)", why))
		return
	}
	found, err := machinegrant.Forget(p.Kind, p.Name)
	switch {
	case err != nil:
		c.reply(msg.ID, nil, refusal(errRefused, "the grant could not be forgotten: %v", err))
	case !found:
		c.reply(msg.ID, nil, refusal(errParams, "no %s %q is kept", p.Kind, p.Name))
	default:
		c.reply(msg.ID, map[string]any{"forgotten": true, "kind": p.Kind, "name": p.Name}, nil)
	}
}
