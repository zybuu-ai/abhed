package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// mcpPromptSource offers the connected MCP servers' prompts as
// /mcp__<server>__<prompt>. It asks the gateway on each lookup, so a
// restarted server's prompts follow; the registry keeps built-ins first.
type mcpPromptSource struct{ st *cliState }

func (mcpPromptSource) Source() string { return sourceMCP }

func (s mcpPromptSource) SlashCommands() []slashCmd {
	gw := s.gateway()
	if gw == nil {
		return nil
	}
	var out []slashCmd
	for _, sp := range gw.Prompts() {
		server, p := sp.Server, sp.Prompt
		var hint []string
		for _, a := range p.Arguments {
			if a.Required {
				hint = append(hint, "<"+a.Name+">")
			} else {
				hint = append(hint, "["+a.Name+"]")
			}
		}
		help := oneLine(config.Printable(p.Description), 60)
		if help == "" {
			help = "MCP prompt"
		}
		out = append(out, slashCmd{
			Name: "/mcp__" + server + "__" + p.Name, Args: strings.Join(hint, " "),
			Help: help + " (mcp: " + server + ")", Group: "custom",
			Run: func(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
				return false, runMCPPrompt(ctx, e, server, p, args)
			},
		})
	}
	return out
}

func (s mcpPromptSource) gateway() *mcp.Gateway {
	if s.st == nil || s.st.set == nil {
		return nil
	}
	return s.st.set.Gateway
}

// promptArgs maps the typed words to a prompt's arguments in order; the
// last argument takes the rest of the line.
func promptArgs(name string, p mcp.PromptDef, words []string) (map[string]string, error) {
	if len(p.Arguments) == 0 {
		if len(words) > 0 {
			return nil, fmt.Errorf("%s takes no arguments", name)
		}
		return nil, nil
	}
	vals := map[string]string{}
	for i, a := range p.Arguments {
		switch {
		case i < len(words) && i == len(p.Arguments)-1:
			vals[a.Name] = strings.Join(words[i:], " ")
		case i < len(words):
			vals[a.Name] = words[i]
		case a.Required:
			return nil, fmt.Errorf("%s needs %s", name, a.Name)
		}
	}
	return vals, nil
}

// runMCPPrompt fetches a server's prompt because the person typed its name,
// shows it escaped, records it, and sends exactly what was shown as their
// next message. Nothing a server offers reaches the model unasked.
func runMCPPrompt(ctx context.Context, e *cmdEnv, server string, p mcp.PromptDef, args []string) error {
	st, sf := e.st, e.ui
	name := "/mcp__" + server + "__" + p.Name
	if err := st.turnFree(); err != nil {
		return err
	}
	vals, err := promptArgs(name, p, args)
	if err != nil {
		return err
	}
	gw := mcpPromptSource{st}.gateway()
	if gw == nil {
		return errors.New("no MCP servers are connected")
	}
	if err := ensureConversation(ctx, st); err != nil {
		return err
	}
	text, err := gw.GetPrompt(ctx, server, p.Name, vals)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	// Escaped once, so the person reads exactly what the model will.
	shown := strings.TrimSpace(config.PrintableText(text))
	if shown == "" {
		return fmt.Errorf("%s returned no text", name)
	}
	// The server wrote the text, so the person reads it and says yes before
	// it goes out as their message; Enter alone sends nothing.
	answer, err := sf.Dialog(ctx, ui.DialogSpec{Kind: ui.DialogConfirm, Title: "Prompt from the MCP server " + server,
		Body: []ui.Block{{Kind: ui.BlockToolOut, Text: shown}},
		Ask:  "Send this as your message? The server wrote it."})
	if answer != ui.ChoiceYes {
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: name + " was not sent"})
		if errors.Is(err, ui.ErrNoAnswer) {
			return nil
		}
		return err
	}
	sum := sha256.Sum256([]byte(shown))
	if _, err := st.loop.Recorder.Record(agent.EvCommandInvoked, agent.ActorUser, agent.Trusted, agent.CommandInvoked{
		Name: name, Source: sourceMCP, SHA256: hex.EncodeToString(sum[:]), Args: strings.Join(args, " "),
	}); err != nil {
		return err
	}
	sf.Append(ui.Block{Kind: ui.BlockNotice, Text: "prompt from the MCP server " + server + ", sent as your message:"})
	sf.Append(ui.Block{Kind: ui.BlockToolOut, Text: shown})
	return st.sendTurn(agent.Message{Text: shown}, nil)
}
