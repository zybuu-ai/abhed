package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/hooks", Help: "list the hooks by layer, with their events and status",
		Group: "mode", Order: 40, ReadOnly: true, Run: slashHooks})
}

// cliHooks are the extension hooks at the loop's own points: a message
// submitted, a call about to be asked, a run ending. A refusal is shown to
// the person; nothing a hook says approves anything.
type cliHooks struct {
	host *extension.Host
	st   *cliState
}

var _ agent.Hooks = cliHooks{}

func (h cliHooks) PromptSubmitted(ctx context.Context, sessionID, text string) string {
	why := h.host.Veto(ctx, extension.EvUserPromptSubmit, extension.Request{SessionID: sessionID, Content: text})
	if why != "" {
		h.st.say(ui.Block{Kind: ui.BlockError, Text: "your message was not sent: " + why})
	} else if down := h.host.NotRunning(extension.EvUserPromptSubmit); len(down) > 0 {
		// A prompt hook fails open; the person is told it did not look.
		h.st.say(ui.Block{Kind: ui.BlockNotice, Text: "not screened: the user_prompt_submit hook " +
			strings.Join(down, ", ") + " is not running"})
	}
	return why
}

func (h cliHooks) PermissionRequested(ctx context.Context, pol *policy.Engine, sessionID, tool string, args json.RawMessage, reason string) string {
	return h.host.Veto(ctx, extension.EvPermissionRequest, extension.Request{
		SessionID: sessionID, Tool: tool, Args: args, Content: reason, Policy: pol,
	})
}

func (h cliHooks) Observe(ctx context.Context, event, sessionID, tool, detail string) {
	h.host.Observe(ctx, extension.Event(event), extension.Request{SessionID: sessionID, Tool: tool, Content: detail})
}

// attachHooks gives a conversation's loop the session's hooks, and has each
// hook that fires recorded in that conversation as hook.fired.
func (c *cliState) attachHooks(loop *agent.Loop) {
	c.hookRecorder.Store(loop.Recorder)
	if c.hooks == nil || c.hooks.Len() == 0 {
		return
	}
	loop.Hooks = cliHooks{host: c.hooks, st: c}
	recordFired(c.hooks, c.hookRecorder.Load)
}

// recordFired records each hook that fires as hook.fired, in the record
// rec returns at the time; a -p run and the terminal share it.
func recordFired(host *extension.Host, rec func() *agent.Recorder) {
	host.SetOnFired(func(f extension.Fired) {
		r := rec()
		if r == nil {
			return
		}
		if _, err := r.Record(agent.EvHookFired, agent.ActorSystem, agent.Trusted, agent.HookFired{
			Extension: f.Extension, Event: string(f.Event), Verdict: f.Verdict,
		}); err != nil {
			warnf("could not record %s: %v", agent.EvHookFired, err)
		}
	})
}

// say shows a block on the session's surface, or prints it.
func (c *cliState) say(b ui.Block) {
	if c.surface != nil {
		c.surface.Append(b)
		return
	}
	fmt.Printf("  %s\n", ui.VisibleLine(b.Text))
}

// slashHooks is /hooks: each configured extension with the layer it came
// from, the events it takes, what it matches, and whether it still runs.
func slashHooks(ctx context.Context, e *cmdEnv, _ []string) (bool, error) {
	cfg := e.st.appCfg
	rows := [][]string{{"extension", "layer", "events", "match", "status"}}
	running := map[string]*extension.Extension{}
	for _, x := range e.st.hooks.Extensions() {
		running[x.Name()] = x
	}
	for _, x := range cfg.Extensions {
		layer := cfg.ExtensionLayer(x.Name)
		toolsOnly := cfg.Hooks.Disabled || cfg.Hooks.ManagedOnly && layer != config.LayerManaged
		if layer == config.LayerManaged {
			layer = "managed (locked)"
		}
		events := "all"
		if len(x.Events) > 0 {
			events = strings.Join(x.Events, ", ")
		}
		if x.Async {
			events += " (async)"
		}
		status := "not started"
		if ext, ok := running[x.Name]; ok {
			status = "running"
			if !ext.Running() {
				status = "stopped: " + ext.LastError()
			}
			if toolsOnly {
				status += " (tools only)"
			}
		} else if cfg.Hooks.Disabled {
			status = "off: hooks are disabled"
		} else if toolsOnly {
			status = "off: only the managed configuration's extensions take hooks"
		}
		rows = append(rows, []string{x.Name, layer, events, strings.Join(x.Match, ", "), status})
	}
	body := []ui.Block{{Kind: ui.BlockTable, Rows: rows}}
	if len(rows) == 1 {
		body = []ui.Block{{Kind: ui.BlockNotice, Text: "no hooks are configured"}}
	}
	note := "Hooks can block a call or a message, or force a call to be asked; they never approve one."
	if cfg.Hooks.Disabled {
		note = "The managed configuration disables hooks: extensions keep only the tools they provide. " + note
	}
	if !cfg.Workspace.Trusted && slices.ContainsFunc(cfg.Workspace.Ignored, func(k config.IgnoredKey) bool { return strings.HasPrefix(k.Key, "extensions") }) {
		note += " The untrusted workspace file's extensions were not started."
	}
	if cfg.Hooks.ManagedOnly && !cfg.Hooks.Disabled {
		note = "The managed configuration limits hooks to its own extensions: the rest keep only the tools they provide. " + note
	}
	body = append(body, ui.Block{Kind: ui.BlockNotice, Text: note})
	return false, e.ui.Panel(ctx, ui.PanelSpec{Title: "Hooks", Body: body})
}
