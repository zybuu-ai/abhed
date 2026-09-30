package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/permissions", Args: "[allow|ask|deny|remove <rule> | explain <tool> <what>]",
		Help:  "show the rules by layer, add or remove this session's own, or explain a decision",
		Group: "mode", Order: 20, Run: slashPermissions})
}

// slashPermissions is /permissions.
func slashPermissions(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
	if len(args) == 0 {
		return false, e.ui.Panel(ctx, ui.PanelSpec{Title: "Permission rules", Body: permissionsView(e.st.appCfg, e.pol)})
	}
	rest := strings.TrimSpace(strings.Join(args[1:], " "))
	switch args[0] {
	case policy.ListAllow, policy.ListAsk, policy.ListDeny:
		return false, addSessionRule(ctx, e, args[0], rest)
	case "remove", "rm":
		return false, removeSessionRule(e, rest)
	case "explain":
		if len(args) < 3 {
			return false, errors.New("usage: /permissions explain <tool> <command, path or url>")
		}
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: explainDecision(e, args[1], strings.Join(args[2:], " "))})
		return false, nil
	}
	return false, fmt.Errorf("unknown /permissions action %q: want allow, ask, deny, remove or explain", args[0])
}

// permissionsView is the rules in force, one row each, with the layer that
// set them: managed rules first, since they win, then the person's, the
// workspace's and the session's own.
func permissionsView(cfg config.Config, pol *policy.Engine) []ui.Block {
	rows := [][]string{{"layer", "list", "rule"}}
	add := func(list string, rules []string, layer func(string) string) {
		for _, r := range rules {
			rows = append(rows, []string{layer(r), list, r})
		}
	}
	configured := func(list string) func(string) string {
		return func(r string) string {
			l := cfg.RuleLayer(list, r)
			if l == config.LayerManaged {
				return "managed (locked)"
			}
			if l == config.LayerWorkspace && !cfg.Workspace.Trusted {
				return "workspace (untrusted: tightens only)"
			}
			return l
		}
	}
	for _, list := range []string{policy.ListDeny, policy.ListAsk, policy.ListAllow} {
		var rules []string
		switch list {
		case policy.ListDeny:
			rules = cfg.Permissions.Deny
		case policy.ListAsk:
			rules = cfg.Permissions.Ask
		default:
			rules = cfg.Permissions.Allow
		}
		sorted := slices.Clone(rules)
		rank := map[string]int{"managed (locked)": 0, config.LayerUser: 1, config.LayerWorkspace: 2, config.LayerFlag: 3, config.LayerDefault: 4}
		slices.SortStableFunc(sorted, func(a, b string) int {
			return rank[configured(list)(a)] - rank[configured(list)(b)]
		})
		add(list, sorted, configured(list))
	}
	deny, ask, allow := pol.Session.SessionRules()
	session := func(string) string { return "session" }
	add(policy.ListDeny, deny, session)
	add(policy.ListAsk, ask, session)
	add(policy.ListAllow, allow, session)
	pinned, why := pol.Session.PinnedRules()
	for i, r := range pinned {
		rows = append(rows, []string{"session (" + why[i] + ")", policy.ListDeny, r})
	}
	out := []ui.Block{{Kind: ui.BlockTable, Rows: rows}}
	var ignored []string
	for _, k := range cfg.Workspace.Ignored {
		if strings.HasPrefix(k.Key, "permissions.") {
			ignored = append(ignored, k.Key+" "+k.Value)
		}
	}
	if len(ignored) > 0 {
		out = append(out, ui.Block{Kind: ui.BlockNotice, Text: "ignored from the untrusted workspace file: " + strings.Join(ignored, "; ")})
	}
	out = append(out, ui.Block{Kind: ui.BlockNotice, Text: "mode: " + string(pol.Mode) +
		". Deny rules win in every mode; session rules end with /clear or /resume."})
	return out
}

// addSessionRule adds a rule for this session only. Deny and ask rules only
// tighten, so they are added as asked; an allow rule is asked about, twice
// when it approves every call to a tool, and refused where the managed
// configuration sets the permissions.
func addSessionRule(ctx context.Context, e *cmdEnv, list, rule string) error {
	if rule == "" {
		return fmt.Errorf("usage: /permissions %s <rule>, e.g. bash(go test*)", list)
	}
	if _, err := policy.ParseRule(rule); err != nil {
		return err
	}
	if e.pol.Session == nil {
		return errors.New("this session keeps no rules of its own")
	}
	if list == policy.ListAllow {
		cfg := e.st.appCfg
		if cfg.ManagedSets("permissions") {
			return fmt.Errorf("the managed configuration sets the permission rules, so this session may not add allow rules (set in %s)", managed.ConfigFile)
		}
		if policy.NeverAllows(rule) {
			return fmt.Errorf("allow rule %s holds shell control syntax, so it could never match", rule)
		}
		if !confirmRule(ctx, e, "Allow "+rule+" for this session?",
			"Calls it matches run without asking until /clear. Deny rules, ask rules, destructive commands and plan mode still apply.") {
			return nil
		}
		if policy.IsBroad(rule) && !confirmRule(ctx, e, "Allow every call to "+ruleTool(rule)+"?",
			rule+" approves every call to "+ruleTool(rule)+", not one command or path. Confirm again to add it.") {
			return nil
		}
	}
	added, err := e.pol.Session.Add(list, rule)
	if err != nil || !added {
		if err == nil {
			e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: rule + " is already a session " + list + " rule"})
		}
		return err
	}
	e.st.recordCLI(agent.EvPermissionChanged, agent.PermissionChanged{
		Op: "add", List: list, Rule: rule, Scope: "session", By: agent.ByUser,
	})
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: fmt.Sprintf("session %s rule added: %s (until /clear)", list, rule)})
	return nil
}

// removeSessionRule takes a rule out of whichever session list holds it.
func removeSessionRule(e *cmdEnv, rule string) error {
	for _, list := range []string{policy.ListAllow, policy.ListAsk, policy.ListDeny} {
		removed, err := e.pol.Session.Remove(list, rule)
		if err != nil {
			return err
		}
		if removed {
			e.st.recordCLI(agent.EvPermissionChanged, agent.PermissionChanged{
				Op: "remove", List: list, Rule: rule, Scope: "session", By: agent.ByUser,
			})
			e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: fmt.Sprintf("session %s rule removed: %s", list, rule)})
			return nil
		}
	}
	return fmt.Errorf("%s is not one of this session's rules; configured rules are changed in their file", rule)
}

// confirmRule asks a yes-or-no question whose default is no.
func confirmRule(ctx context.Context, e *cmdEnv, title, why string) bool {
	answer, err := e.ui.Dialog(ctx, ui.DialogSpec{Kind: ui.DialogConfirm, Title: title,
		Body: []ui.Block{{Kind: ui.BlockNotice, Text: why}}, Why: "asked by /permissions"})
	if err != nil || answer != ui.ChoiceYes {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "not added"})
		return false
	}
	return true
}

// ruleTool is the tool a rule names.
func ruleTool(rule string) string {
	if i := strings.Index(rule, "("); i > 0 {
		return rule[:i]
	}
	return rule
}

// explainDecision is a dry run of policy for one call: what it would decide,
// at which step, by which rule and why. Hooks are not asked, since asking one
// would show it a call that is not being made.
func explainDecision(e *cmdEnv, tool, what string) string {
	key := "path"
	switch tool {
	case "bash":
		key = "command"
	case "web_fetch":
		key = "url"
	case "web_search":
		key = "query"
	}
	args, _ := json.Marshal(map[string]string{key: what})
	dry := *e.pol
	dry.Hooks = nil
	res := dry.Evaluate(tool, mutatesTool(e, tool, args), args)
	rule := res.Rule
	if rule == "" {
		rule = "no rule"
	}
	return fmt.Sprintf("%s · step %s · %s · %s (mode %s; hooks not consulted)", res.Decision, res.Step, rule, res.Reason, e.pol.Mode)
}

// mutatesTool reports whether a call can change things, from the session's
// tools when a conversation is open, else from the built-in ones.
func mutatesTool(e *cmdEnv, tool string, args json.RawMessage) bool {
	if e.st != nil && e.st.loop != nil && e.st.loop.Tools != nil {
		if t, ok := e.st.loop.Tools.Get(tool); ok {
			return tools.MutatesCall(t, args)
		}
	}
	switch tool {
	case "read", "glob", "grep", "ls", "web_fetch", "web_search", "recall":
		return false
	}
	return true
}
