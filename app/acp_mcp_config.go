package app

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// MCP servers from Studio (studio-acp-contract.md §6.4). Adding one widens the
// harness, so it is never done over ACP (§2.1, §2.3): Studio checks it here,
// asks in a modal whose default is No, and its main process runs the engine's
// own `abhed mcp add -confirm-fd 3 -sha256 …`. Removing one narrows.

func init() {
	liveFeatures = append(liveFeatures, "mcp.check")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/mcp/check":  (*acpConn).mcpCheck,
		"_abhed/mcp/remove": (*acpConn).mcpRemoveServer,
	})
}

// mcpServerParams is a server as Studio sends it. env holds names only: the
// server gets the person's own value of each, so no secret is written.
type mcpServerParams struct {
	SessionID  string            `json:"sessionId"`
	Name       string            `json:"name"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	URL        string            `json:"url,omitempty"`
	Env        []string          `json:"env,omitempty"`
	HeadersEnv map[string]string `json:"headersEnv,omitempty"`
	AllowTools []string          `json:"allowTools,omitempty"`
}

// strictParams decodes params into v, refusing unknown fields (§2.1).
func strictParams(msg rpcMessage, v any) *rpcError {
	dec := json.NewDecoder(strings.NewReader(string(msg.Params)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badParams("", err)
	}
	return nil
}

// mcpServerFrom reads and checks the server a request names.
func mcpServerFrom(msg rpcMessage) (config.MCPServerConfig, *rpcError) {
	var p mcpServerParams
	if e := strictParams(msg, &p); e != nil {
		return config.MCPServerConfig{}, e
	}
	if (p.Command == "") == (p.URL == "") {
		return config.MCPServerConfig{}, refusal(errParams, "a server has a command or a url, not both")
	}
	for _, e := range p.Env {
		if strings.Contains(e, "=") {
			return config.MCPServerConfig{}, refusal(errParams, "env takes names only: the server gets your own value of each, so no value is written to the file")
		}
	}
	target, args := p.Command, p.Args
	if p.URL != "" {
		target = p.URL
	}
	var headers []string
	for k, v := range p.HeadersEnv {
		headers = append(headers, k+"="+v)
	}
	s, err := newMCPServer(p.Name, target, args, p.Env, headers, p.AllowTools)
	if err != nil {
		return s, refusal(errParams, "%s", err)
	}
	if p.Command != "" && s.URL != "" {
		return s, refusal(errParams, "the command is a URL; send it as url")
	}
	return s, nil
}

// mcpRefusal is why a server cannot be added or removed: the managed
// configuration's say is errPolicy, anything else errRefused.
func mcpRefusal(err error) *rpcError {
	var me *config.ManagedError
	if errors.As(err, &me) {
		return refusal(errPolicy, "%s", err)
	}
	return refusal(errRefused, "%s", err)
}

// recordMCPRefused records an attempt to add a server that policy or an
// agent's command refused, with who asked, as /config set does.
func recordMCPRefused(s *acpSession, srv config.MCPServerConfig, why string) {
	_, sum := mcpEntrySum(srv)
	s.record(agent.EvConfigRefused, agent.ConfigAttempt{Layer: "command", Source: "studio mcp check",
		Key: "mcp.servers", Value: config.Printable(srv.Name) + " sha256:" + sum, Decision: "refused", Reason: why,
		Principal: toolset.LocalPrincipal(inAgentCommand())})
}

// mcpCheck says whether a server could be added, how it reads, and the entry's
// transport and SHA-256, which Studio passes to `abhed mcp add -sha256`. It
// writes nothing.
func (c *acpConn) mcpCheck(msg rpcMessage) {
	sess, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	s, e := mcpServerFrom(msg)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if why := inAgentCommand(); why != "" {
		recordMCPRefused(sess, s, "inside an agent's command ("+why+")")
		c.reply(msg.ID, nil, refusal(errPolicy, "adding an MCP server is refused inside an agent's command (%s)", why))
		return
	}
	file, err := mcpAddable(s)
	if err != nil {
		var me *config.ManagedError
		if errors.As(err, &me) {
			recordMCPRefused(sess, s, err.Error())
		}
		c.reply(msg.ID, nil, mcpRefusal(err))
		return
	}
	kind, sum := mcpEntrySum(s)
	c.reply(msg.ID, map[string]any{"file": file, "description": describeServer(s), "kind": kind, "sha256": sum}, nil)
}

// mcpRemoveServer takes a server out of the person's own file; it narrows,
// so Studio need not ask.
func (c *acpConn) mcpRemoveServer(msg rpcMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
		Name      string `json:"name"`
	}
	if e := strictParams(msg, &p); e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	sess, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if why := inAgentCommand(); why != "" {
		c.reply(msg.ID, nil, refusal(errPolicy, "removing an MCP server is refused inside an agent's command (%s)", why))
		return
	}
	if !mcpServerName.MatchString(p.Name) {
		c.reply(msg.ID, nil, refusal(errParams, "name is a server's name"))
		return
	}
	file, removed, err := removeMCPServerEntry(p.Name, "studio mcp remove")
	if err != nil {
		c.reply(msg.ID, nil, mcpRefusal(err))
		return
	}
	kind, sum := mcpEntrySum(removed)
	sess.record(agent.EvMCPStatus, agent.MCPStatus{Server: p.Name, Op: "remove", Status: "removed", Kind: kind, SHA256: sum, By: agent.ByUser})
	c.reply(msg.ID, map[string]any{"removed": true, "file": file}, nil)
}
