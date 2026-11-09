package app

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// Adding and removing MCP servers from Studio (studio-acp-contract.md §6.4):
// the same checks and the same file as `abhed mcp add|remove`, with Studio's
// modal as the person's agreement in place of the terminal's [y/N].

func init() {
	liveFeatures = append(liveFeatures, "mcp.add")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/mcp/check":  (*acpConn).mcpCheck,
		"_abhed/mcp/add":    (*acpConn).mcpAddServer,
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

// mcpServerFrom reads and checks the server a request names.
func mcpServerFrom(msg rpcMessage) (config.MCPServerConfig, *rpcError) {
	var p mcpServerParams
	dec := json.NewDecoder(strings.NewReader(string(msg.Params)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return config.MCPServerConfig{}, badParams("", err)
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

// mcpSession is the request's session, refused inside an agent's command.
func (c *acpConn) mcpSession(msg rpcMessage, what string) (*acpSession, *rpcError) {
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		return nil, e
	}
	if why := inAgentCommand(); why != "" {
		return nil, refusal(errPolicy, "%s is refused inside an agent's command (%s)", what, why)
	}
	return s, nil
}

// mcpCheck says whether a server could be added, and how it reads, before
// Studio asks the person.
func (c *acpConn) mcpCheck(msg rpcMessage) {
	if _, e := c.mcpSession(msg, "adding an MCP server"); e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	s, e := mcpServerFrom(msg)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	file, err := mcpAddable(s)
	if err != nil {
		c.reply(msg.ID, nil, mcpRefusal(err))
		return
	}
	c.reply(msg.ID, map[string]any{"file": file, "description": describeServer(s)}, nil)
}

// mcpAddServer writes the server once the person agreed in Studio.
func (c *acpConn) mcpAddServer(msg rpcMessage) {
	sess, e := c.mcpSession(msg, "adding an MCP server")
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	s, e := mcpServerFrom(msg)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	file, err := writeMCPServer(s, "studio mcp add")
	if err != nil {
		c.reply(msg.ID, nil, mcpRefusal(err))
		return
	}
	sess.record(agent.EvMCPStatus, agent.MCPStatus{Server: s.Name, Op: "add", Status: "added", By: agent.ByUser})
	c.reply(msg.ID, map[string]any{"added": true, "file": file}, nil)
}

// mcpRemoveServer takes a server out of the person's own file; it narrows,
// so Studio need not ask.
func (c *acpConn) mcpRemoveServer(msg rpcMessage) {
	sess, e := c.mcpSession(msg, "removing an MCP server")
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	if !mcpServerName.MatchString(p.Name) {
		c.reply(msg.ID, nil, refusal(errParams, "name is a server's name"))
		return
	}
	file, err := removeMCPServer(p.Name, "studio mcp remove")
	if err != nil {
		c.reply(msg.ID, nil, mcpRefusal(err))
		return
	}
	sess.record(agent.EvMCPStatus, agent.MCPStatus{Server: p.Name, Op: "remove", Status: "removed", By: agent.ByUser})
	c.reply(msg.ID, map[string]any{"removed": true, "file": file}, nil)
}
