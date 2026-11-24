package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/network", Args: "[workspace | host <name> | once [name]]",
		Help:  "what commands can reach; open the network to them for this workspace, one host, or this session (asks first)",
		Group: "status", Order: 36, Run: slashNetwork})
}

// slashNetwork is the terminal's way for the person to open the network to
// commands where it is off, as Studio's offer is: asked first, and kept for
// the workspace except for this session alone. The agent cannot do it.
func slashNetwork(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
	set := e.st.set
	if set == nil || set.Network == nil {
		return false, errors.New("this session has no sandbox to open")
	}
	n := set.Network
	if len(args) == 0 {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "network: " + n.State()})
		return false, nil
	}
	if why := inAgentCommand(); why != "" {
		return false, fmt.Errorf("opening the network is refused inside an agent's command (%s)", why)
	}
	scope, host := args[0], ""
	if len(args) > 1 {
		host = args[1]
	}
	if ok, why := n.Offer(); !ok {
		return false, errors.New("the network cannot be opened here: " + why)
	}
	what := map[string]string{"workspace": "any host, for this workspace (kept)", "host": host + ", for this workspace (kept)", "once": "any host, for this session only"}[scope]
	if scope == "once" && host != "" {
		what = host + ", for this session only"
	}
	if what == "" || scope == "host" && host == "" {
		return false, errors.New("usage: /network workspace | host <name> | once [name]")
	}
	ans, err := e.ui.Dialog(ctx, ui.DialogSpec{Kind: ui.DialogConfirm, Title: "Let commands reach " + what + "?",
		Why: "commands, the agent's too, then reach it over http and https through the session's proxy; ssh and ping still fail"})
	if err != nil || ans != ui.ChoiceYes {
		return false, errors.New("the network was not opened")
	}
	opened, err := n.Open(scope, host, true)
	if err != nil {
		return false, err
	}
	rule := "network(any host)"
	if host != "" {
		rule = "network(" + strings.ToLower(host) + ")"
	}
	s := "workspace"
	if scope == "once" {
		s = "session"
	}
	e.st.recordCLI(agent.EvPermissionChanged, agent.PermissionChanged{Op: "add", List: "allow", Rule: rule, Scope: s, By: agent.ByUser})
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "opened " + opened})
	return false, nil
}
