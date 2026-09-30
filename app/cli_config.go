package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// configKey is a setting /config shows and may change in the person's own
// file. widens says whether moving from old to new lets the agent do more,
// which needs a confirmation; check refuses a value that cannot be used.
type configKey struct {
	path   string
	get    func(config.Config) string
	check  func(config.Config, string) (any, error)
	widens func(old, new string) bool
}

var modeOrder = []string{"plan", "default", "accept-edits", "auto", "bypass"}
var syntaxOrder = []string{"refuse", "report", "off"}

func rank(order []string, v string) int { return slices.Index(order, v) }

var configKeys = []configKey{
	{path: "model.default",
		get: func(c config.Config) string { return c.Model.Default },
		check: func(c config.Config, v string) (any, error) {
			if !slices.Contains(toolset.OfferedModels(c), v) {
				return nil, fmt.Errorf("%q is not a configured provider", v)
			}
			return v, nil
		},
		widens: func(string, string) bool { return false }},
	{path: "permissions.mode",
		get: func(c config.Config) string { return orDefault(c.Permissions.Mode, "default") },
		check: func(_ config.Config, v string) (any, error) {
			if !validMode(v) {
				return nil, fmt.Errorf("unknown mode %q", v)
			}
			return v, nil
		},
		widens: func(o, n string) bool { return rank(modeOrder, n) > rank(modeOrder, o) }},
	{path: "sandbox.allow_network",
		get:    func(c config.Config) string { return strconv.FormatBool(c.Sandbox.AllowNetwork) },
		check:  func(_ config.Config, v string) (any, error) { return strconv.ParseBool(v) },
		widens: func(o, n string) bool { return n == "true" && o != "true" }},
	{path: "limits.max_turns",
		get: func(c config.Config) string { return strconv.Itoa(c.Limits.MaxTurns) },
		check: func(_ config.Config, v string) (any, error) {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%q is not a number of turns", v)
			}
			return n, nil
		},
		widens: func(o, n string) bool {
			a, _ := strconv.Atoi(o)
			b, _ := strconv.Atoi(n)
			return (b == 0 && a != 0) || (a != 0 && b > a)
		}},
	{path: "tools.syntax_check",
		get: func(c config.Config) string { return orDefault(c.Tools.SyntaxCheck, "refuse") },
		check: func(_ config.Config, v string) (any, error) {
			if rank(syntaxOrder, v) < 0 {
				return nil, fmt.Errorf("use refuse, report or off")
			}
			return v, nil
		},
		widens: func(o, n string) bool { return rank(syntaxOrder, n) > rank(syntaxOrder, o) }},
	{path: "statusline.command",
		get:   func(c config.Config) string { return c.Statusline.Command },
		check: func(_ config.Config, v string) (any, error) { return v, nil },
		// A command is a process run on every redraw.
		widens: func(o, n string) bool { return n != "" && n != o }},
}

// source says which layer made a setting.
func source(c config.Config, path string) string {
	switch {
	case c.ManagedSets(path):
		return "managed"
	case c.Sets(path):
		return "a configuration file"
	}
	return "default"
}

// slashConfig is /config: the settings and where each comes from, and
// /config set to change the person's own ~/.abhed/config.json.
func slashConfig(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
	if len(args) == 0 {
		return false, e.ui.Panel(ctx, configPanel(e.st.appCfg))
	}
	if args[0] != "set" || len(args) < 3 {
		return false, errors.New("usage: /config, or /config set <key> <value>")
	}
	return false, configSet(ctx, e, args[1], strings.Join(args[2:], " "))
}

func configPanel(c config.Config) ui.PanelSpec {
	home, _ := os.UserHomeDir()
	files := [][]string{{"file", "state"}}
	if _, err := os.Stat(managed.ConfigFile); err == nil {
		files = append(files, []string{managed.ConfigFile, "managed: it binds every other layer"})
	}
	files = append(files, []string{filepath.Join(home, ".abhed", "config.json"), "yours: /config set writes here"})
	if c.Workspace.File != "" {
		files = append(files, []string{c.Workspace.File, trustLine(c.Workspace)})
	}
	rows := [][]string{{"setting", "value", "from"}}
	for _, k := range configKeys {
		v := k.get(c)
		if k.path == "statusline.command" && v != "" {
			v = config.Printable(v)
		}
		rows = append(rows, []string{k.path, orDefault(v, "(none)"), source(c, k.path)})
	}
	body := []ui.Block{{Kind: ui.BlockTable, Rows: files}, {Kind: ui.BlockTable, Rows: rows},
		{Kind: ui.BlockNotice, Text: "/config set <setting> <value> changes your own file for the next session; a change that lets the agent do more asks first"}}
	return ui.PanelSpec{Title: "Configuration", Body: body}
}

// configSet writes one setting into the person's own file. A setting the
// managed configuration makes is refused, and one that widens what the agent
// may do needs a confirmation, which a surface with no answers never gives.
func configSet(ctx context.Context, e *cmdEnv, path, value string) error {
	c := e.st.appCfg
	i := slices.IndexFunc(configKeys, func(k configKey) bool { return k.path == path })
	if i < 0 {
		var names []string
		for _, k := range configKeys {
			names = append(names, k.path)
		}
		return fmt.Errorf("/config set takes %s; edit ~/.abhed/config.json for the rest", strings.Join(names, ", "))
	}
	k := configKeys[i]
	if c.ManagedSets(path) {
		return fmt.Errorf("%s is set by the managed configuration and cannot be changed here", path)
	}
	v, err := k.check(c, value)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if k.widens(k.get(c), value) {
		ans, err := e.ui.Dialog(ctx, ui.DialogSpec{Kind: ui.DialogConfirm,
			Title: fmt.Sprintf("Set %s to %s in your own configuration?", path, config.Printable(value)),
			Why:   "this lets the agent do more than it does now"})
		if err != nil || ans != ui.ChoiceYes {
			return fmt.Errorf("%s not changed", path)
		}
	}
	file, err := writeUserSetting(path, v)
	if err != nil {
		return err
	}
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: fmt.Sprintf("%s set in %s; it applies from the next session", path, config.Printable(file))})
	return nil
}

// writeUserSetting sets one dotted path in ~/.abhed/config.json, keeping
// everything else in the file.
func writeUserSetting(path string, v any) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	file := filepath.Join(home, ".abhed", "config.json")
	doc := map[string]any{}
	if data, err := os.ReadFile(file); err == nil {
		if err := json.Unmarshal(data, &doc); err != nil {
			return file, fmt.Errorf("%s is not valid JSON, so it was not changed: %v", file, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return file, err
	}
	parts := strings.Split(path, ".")
	m := doc
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return file, err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return file, err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return file, err
	}
	return file, os.Rename(tmp, file)
}
