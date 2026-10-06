package app

import (
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// inAgentCommand says why this process runs inside an agent's command, or "";
// replaced in tests.
var inAgentCommand = sandbox.InAgentCommand

// selfAdmin names what rest (a subcommand and its arguments) would change in
// Abhed's own configuration, accounts, secrets or record, or "" when it only
// reads. An agent must not administer the harness that confines it.
func selfAdmin(rest []string) string {
	if len(rest) == 0 {
		return ""
	}
	verb := ""
	if len(rest) > 1 {
		verb = rest[1]
	}
	switch rest[0] {
	case "init":
		return "abhed init"
	case "trust":
		if verb == "grant" {
			return "abhed trust grant"
		}
	case "record":
		if verb == "prune" {
			return "abhed record prune"
		}
	case "user":
		if slices.Contains([]string{"add", "passwd", "remove", "rm", "import"}, verb) {
			return "abhed user " + verb
		}
	case "secret":
		if slices.Contains([]string{"set", "rm", "remove"}, verb) {
			return "abhed secret " + verb
		}
	case "mcp":
		if slices.Contains([]string{"add", "remove", "rm"}, verb) {
			return "abhed mcp " + verb
		}
	case "migrate":
		return "abhed migrate"
	case "admin":
		return "abhed admin"
	}
	for _, a := range rest[1:] {
		if strings.HasPrefix(strings.TrimLeft(a, "-"), "trust-workspace") {
			return "-trust-workspace"
		}
	}
	return ""
}

// escalation names a top-level flag that would widen what a nested session
// may do, or "". -settings and -mcp-config are refused whatever they hold:
// either can name an endpoint the session sends the code or its queries to,
// so judging their content key by key would trail every new key that can.
// A nested run narrows with -mode plan, -disallowedTools or -max-turns.
func (f *cliFlags) escalation() string {
	switch {
	case f.settings != "":
		return "-settings"
	case len(f.mcpConfig) > 0:
		return "-mcp-config"
	case f.trustWS:
		return "-trust-workspace"
	case f.skipPerms:
		return "-dangerously-skip-permissions"
	case f.mode == string(policy.ModeBypass):
		return "bypass mode"
	}
	return ""
}

// widened names what the merged -settings, -agents, -mcp-config and rule
// flags add or remove against the configuration alone that lets a nested
// session do more, or "".
func widened(base, eff config.Config) string {
	if eff.Permissions.Mode == string(policy.ModeBypass) && base.Permissions.Mode != string(policy.ModeBypass) {
		return "bypass mode from the command line's settings"
	}
	for _, r := range eff.Permissions.Allow {
		if !slices.Contains(base.Permissions.Allow, r) {
			return fmt.Sprintf("an allow rule added on the command line (%s)", r)
		}
	}
	// Settings replace lists wholesale, so an empty list would drop rules.
	for _, r := range base.Permissions.Deny {
		if !slices.Contains(eff.Permissions.Deny, r) {
			return fmt.Sprintf("a deny rule removed on the command line (%s)", r)
		}
	}
	for _, r := range base.Permissions.Ask {
		if !slices.Contains(eff.Permissions.Ask, r) {
			return fmt.Sprintf("an ask rule removed on the command line (%s)", r)
		}
	}
	// The fence's settings choose what confines this run's commands.
	if eff.Sandbox.Tier != base.Sandbox.Tier {
		return fmt.Sprintf("sandbox.tier changed on the command line (%q)", eff.Sandbox.Tier)
	}
	if eff.Fence != base.Fence {
		return "a fence setting changed on the command line"
	}
	for _, x := range eff.Permissions.GitExtensions {
		if !slices.Contains(base.Permissions.GitExtensions, x) {
			return fmt.Sprintf("a git extension added on the command line (%s)", x)
		}
	}
	// Managed only, so a flag should never get here; checked all the same.
	if !reflect.DeepEqual(base.WebSearch, eff.WebSearch) || !reflect.DeepEqual(base.WebFetch, eff.WebFetch) {
		return "a web_search or web_fetch change on the command line"
	}
	return ""
}

// refuseInAgent refuses what, when this process runs inside an agent's
// command, and says so; it reports whether it refused.
func refuseInAgent(what string) bool {
	if what == "" {
		return false
	}
	why := inAgentCommand()
	if why == "" {
		return false
	}
	fmt.Fprintf(os.Stderr, "abhed: %s is refused inside an agent's command (%s); run it in your own terminal\n", what, why)
	return true
}
