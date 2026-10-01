package app

import (
	"encoding/json"
	"errors"
	"fmt"

	abhed "github.com/zybuu-ai/abhed/sdk"
)

// Choosing the model from the editor.
//
// Follows ACP schema v1.23.0: session config options, a "select" option of
// category "model" set with session/set_config_option, whose reply carries
// every option; config_option_update is only for a change the agent makes
// itself, and it makes none. Editors built on the earlier unstable API (schema
// v0.6.0: "models" in session/new and session/set_model) are answered too.
// The value is a configured provider's name, looked up in the configuration;
// nothing from the editor is ever used as an endpoint.

// modelSwitcher is an agent whose model can be chosen by configured name; the
// SDK's agent is one.
type modelSwitcher interface {
	Models() []abhed.Model
	SwitchModelNamed(name string) error
}

// modelConfigID is the model selector's id among the session's config options.
const modelConfigID = "model"

// modelDescription says what a configured model is without its endpoint, key
// or the variable holding the key.
func modelDescription(m abhed.Model) string {
	return m.Model + " (" + m.Type + ")"
}

func currentModel(models []abhed.Model) string {
	for _, m := range models {
		if m.Current {
			return m.Name
		}
	}
	return ""
}

// modelConfigOptions is the session's full set of config options: the model selector.
func modelConfigOptions(models []abhed.Model) []any {
	opts := make([]any, 0, len(models))
	for _, m := range models {
		opts = append(opts, map[string]any{"value": m.Name, "name": m.Name, "description": modelDescription(m)})
	}
	return []any{map[string]any{
		"id": modelConfigID, "name": "Model", "description": "The configured model this session runs on",
		"category": "model", "type": "select", "currentValue": currentModel(models), "options": opts,
	}}
}

// legacyModelState is the same list in the unstable SessionModelState shape.
func legacyModelState(models []abhed.Model) map[string]any {
	avail := make([]any, 0, len(models))
	for _, m := range models {
		avail = append(avail, map[string]any{"modelId": m.Name, "name": m.Name, "description": modelDescription(m)})
	}
	return map[string]any{"currentModelId": currentModel(models), "availableModels": avail}
}

// switchModel moves a session to the configured model name, or says why not.
func (c *acpConn) switchModel(sessionID, name string) (modelSwitcher, *rpcError) {
	s := c.session(sessionID)
	if s == nil {
		return nil, &rpcError{Code: errParams, Message: "unknown session"}
	}
	m, ok := s.agent.(modelSwitcher)
	if !ok {
		return nil, &rpcError{Code: errNoMethod, Message: "this session's model cannot be changed"}
	}
	s.mu.Lock()
	running := s.cancel != nil
	s.mu.Unlock()
	if running {
		return nil, &rpcError{Code: errRefused, Message: "a prompt is running in this session; change the model once it ends"}
	}
	switch err := m.SwitchModelNamed(name); {
	case err == nil:
		return m, nil
	case errors.Is(err, abhed.ErrSwitchDuringRun):
		return nil, &rpcError{Code: errRefused, Message: "a prompt is running in this session; change the model once it ends"}
	case errors.Is(err, abhed.ErrUnknownModel):
		return nil, &rpcError{Code: errParams, Message: fmt.Sprintf("no model named %q is configured for this session", name)}
	default:
		return nil, &rpcError{Code: errRefused, Message: err.Error()}
	}
}

func (c *acpConn) setConfigOption(msg rpcMessage) {
	var p struct {
		SessionID string          `json:"sessionId"`
		ConfigID  string          `json:"configId"`
		Value     json.RawMessage `json:"value"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	var value string
	switch p.ConfigID {
	case modelConfigID, "mode":
	default:
		c.reply(msg.ID, nil, &rpcError{Code: errParams, Message: fmt.Sprintf("unknown config option %q", p.ConfigID)})
		return
	}
	if json.Unmarshal(p.Value, &value) != nil {
		c.reply(msg.ID, nil, &rpcError{Code: errParams, Message: "the " + p.ConfigID + " option takes a name"})
		return
	}
	if p.ConfigID == "mode" {
		s := c.session(p.SessionID)
		if s == nil {
			c.reply(msg.ID, nil, &rpcError{Code: errParams, Message: "unknown session"})
			return
		}
		if e := c.changeMode(s, value); e != nil {
			c.reply(msg.ID, nil, e)
			return
		}
		c.reply(msg.ID, map[string]any{"configOptions": c.configOptions(s)}, nil)
		return
	}
	if _, e := c.switchModel(p.SessionID, value); e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	c.reply(msg.ID, map[string]any{"configOptions": c.configOptions(c.session(p.SessionID))}, nil)
}

// setModel answers the unstable session/set_model; its result has no fields,
// so the model now current is in _meta.
func (c *acpConn) setModel(msg rpcMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
		ModelID   string `json:"modelId"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	m, e := c.switchModel(p.SessionID, p.ModelID)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	c.reply(msg.ID, map[string]any{"_meta": map[string]any{acpMetaKey: map[string]any{
		"currentModelId": currentModel(m.Models())}}}, nil)
}
