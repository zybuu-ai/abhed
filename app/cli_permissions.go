package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func init() {
	registerSlash(slashCmd{Name: "/add-dir", Args: "<dir>", Help: "let this session reach another directory, read-only or read-write",
		Group: "mode", Order: 30, Run: slashAddDir})
}

// Access an added directory is given, as workspace.dir_added records it.
const (
	accessRead      = "read"
	accessReadWrite = "read-write"
)

// slashAddDir is /add-dir: bound by the managed configuration as -add-dir
// is, it shows the resolved folder and asks for read-only or read-write
// access, and adds only the folder it checked.
func slashAddDir(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
	if len(args) == 0 {
		return false, errors.New("usage: /add-dir <dir>")
	}
	typed := strings.Join(args, " ")
	applied, err := e.st.appCfg.Apply(config.Overrides{AdditionalDirs: []string{typed}})
	if err != nil {
		return false, err
	}
	canonical, err := canonicalDir(typed)
	if err != nil {
		return false, err
	}
	if _, err := e.sess.CheckRoot(canonical); err != nil {
		return false, err
	}
	if err := refusedDir(e.st.appCfg, canonical); err != nil {
		return false, err
	}
	// Rules have no escape for * and ?, so a read-only rule could not name it.
	if strings.ContainsAny(canonical, "*?") {
		return false, fmt.Errorf("refusing %s: a path with * or ? cannot be named exactly by a rule", canonical)
	}
	for _, root := range e.sess.PolicyRoots() {
		if within(canonical, tools.RealPath(root)) {
			return false, fmt.Errorf("%s is already reachable, inside %s", canonical, root)
		}
	}
	if e.pol.Session == nil {
		return false, errors.New("this session keeps no rules of its own, so a directory cannot be added read-only")
	}
	body := "resolves to " + canonical
	if canonical == typed {
		body = canonical
	}
	answer := answered(e.ui.Dialog(ctx, ui.DialogSpec{
		Kind:  ui.DialogChoice,
		Title: "Let this session reach " + canonical + "?",
		Body: []ui.Block{{Kind: ui.BlockNotice, Text: body + "\nRead-only keeps edit and write out of it; " +
			"commands run by bash are asked about as they are anywhere."}},
		Choices: []ui.Choice{
			{ID: accessRead, Label: "Yes, read only", Key: 'r'},
			{ID: accessReadWrite, Label: "Yes, read and write", Key: 'w', Widening: true},
			{ID: ui.ChoiceNo, Label: "No", Key: 'n'},
		},
		Default: ui.ChoiceNo,
		Why:     "asked by /add-dir",
	}))
	if answer != accessRead && answer != accessReadWrite {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "not added"})
		return false, nil
	}
	if answer == accessRead {
		// The rules go in before the directory, so there is no moment it is writable.
		glob := filepath.ToSlash(canonical) + "/**"
		if err := e.pol.Session.Pin("read-only "+canonical, "write("+glob+")", "edit("+glob+")"); err != nil {
			return false, err
		}
	}
	// What was checked and shown is what is added: a path that now leads
	// elsewhere is refused.
	if _, err := e.sess.AddRootAs(canonical, canonical); err != nil {
		return false, err
	}
	e.st.appCfg = applied
	added := agent.WorkspaceDirAdded{Path: typed, Canonical: canonical, Access: answer, By: agent.ByUser}
	e.st.addedDirs = append(e.st.addedDirs, added)
	e.st.recordCLI(agent.EvWorkspaceDirAdded, added)
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: fmt.Sprintf("added %s (%s) for this session", canonical, answer)})
	return false, nil
}

// canonicalDir is dir absolute, with ~ expanded and links resolved.
func canonicalDir(dir string) (string, error) {
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%s: %w", dir, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	return real, nil
}

// refusedDir adds the CLI's own refusals to the session's: anything inside
// ~/.abhed, and the record directory.
func refusedDir(cfg config.Config, dir string) error {
	if home, err := os.UserHomeDir(); err == nil {
		if p := tools.RealPath(filepath.Join(home, tools.StateDir)); within(dir, p) {
			return fmt.Errorf("refusing %s: it is inside %s, Abhed's own state", dir, p)
		}
	}
	if cfg.Record.Dir != "" && within(dir, tools.RealPath(cfg.Record.Dir)) {
		return fmt.Errorf("refusing %s: it is inside the record directory", dir)
	}
	return nil
}

// within reports whether path is dir or inside it; both are absolute and clean.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel) || rel == "."
}
