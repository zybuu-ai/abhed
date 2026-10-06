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
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// configKey is a setting /config shows and may change in the person's own
// file. widens says whether moving from old to new lets the agent do more,
// which needs a confirmation; both are in get's form, and c is the session's
// configuration, for what a name refers to. check refuses a value that
// cannot be used and returns it parsed.
type configKey struct {
	path   string
	get    func(config.Config) string
	check  func(config.Config, string) (any, error)
	widens func(c config.Config, old, new string) bool
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
		// A hosted model is sent the code, so moving to one asks.
		widens: func(c config.Config, o, n string) bool {
			return n != o && hostedLabel(c.Model.Providers[n].BaseURL) == "hosted"
		}},
	{path: "permissions.mode",
		get: func(c config.Config) string { return orDefault(c.Permissions.Mode, "default") },
		check: func(_ config.Config, v string) (any, error) {
			if !validMode(v) {
				return nil, fmt.Errorf("unknown mode %q", v)
			}
			return v, nil
		},
		widens: func(_ config.Config, o, n string) bool { return rank(modeOrder, n) > rank(modeOrder, o) }},
	{path: "sandbox.allow_network",
		get:    func(c config.Config) string { return strconv.FormatBool(c.Sandbox.AllowNetwork) },
		check:  func(_ config.Config, v string) (any, error) { return strconv.ParseBool(v) },
		widens: func(_ config.Config, o, n string) bool { return n == "true" && o != "true" }},
	{path: "limits.max_turns",
		get: func(c config.Config) string { return strconv.Itoa(c.Limits.MaxTurns) },
		check: func(_ config.Config, v string) (any, error) {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%q is not a number of turns", v)
			}
			return n, nil
		},
		widens: func(_ config.Config, o, n string) bool {
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
		widens: func(_ config.Config, o, n string) bool { return rank(syntaxOrder, n) > rank(syntaxOrder, o) }},
	{path: "statusline.command",
		get:   func(c config.Config) string { return c.Statusline.Command },
		check: func(_ config.Config, v string) (any, error) { return v, nil },
		// A command is a process run on every redraw.
		widens: func(_ config.Config, o, n string) bool { return n != "" && n != o }},
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
	refused := func(err error) error {
		recordSetRefused(e.st, path, value, err.Error())
		return err
	}
	if config.WebKey(path) {
		return refused(fmt.Errorf("%s is managed only: only the managed configuration turns web search or web fetch on "+
			"or says where they go; ask your administrator, or run `sudo abhed admin web-search on`", path))
	}
	i := slices.IndexFunc(configKeys, func(k configKey) bool { return k.path == path })
	if i < 0 {
		var names []string
		for _, k := range configKeys {
			names = append(names, k.path)
		}
		return refused(fmt.Errorf("/config set takes %s; edit ~/.abhed/config.json for the rest", strings.Join(names, ", ")))
	}
	k := configKeys[i]
	if c.ManagedSets(path) {
		return refused(fmt.Errorf("%s is set by the managed configuration and cannot be changed here", path))
	}
	v, err := k.check(c, value)
	if err != nil {
		return refused(fmt.Errorf("%s: %w", path, err))
	}
	// Held across the read, the question and the write, so two sessions
	// cannot lose each other's change.
	unlock, err := lockUserConfig()
	if err != nil {
		return err
	}
	defer unlock()
	// Judged against the person's own file, not this session: a session
	// already widened by a flag or a workspace must not make that permanent
	// unasked. The parsed value is judged, so 1 or T is true as true is.
	own, err := userFileConfig()
	if err != nil {
		return err
	}
	if k.widens(c, k.get(own), fmt.Sprint(v)) {
		ans, err := e.ui.Dialog(ctx, ui.DialogSpec{Kind: ui.DialogConfirm,
			Title: fmt.Sprintf("Set %s to %s in your own configuration?", path, config.Printable(value)),
			Why:   "this lets the agent do more than it does now"})
		if err != nil || ans != ui.ChoiceYes {
			return refused(fmt.Errorf("%s not changed", path))
		}
	}
	file, err := writeUserSetting(path, v)
	if err != nil {
		return err
	}
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: fmt.Sprintf("%s set in %s; it applies from the next session", path, config.Printable(file))})
	return nil
}

// recordSetRefused records a refused /config set with who asked for it.
func recordSetRefused(st *cliState, path, value, why string) {
	home, _ := os.UserHomeDir()
	st.recordCLI(agent.EvConfigRefused, agent.ConfigAttempt{Layer: "command", Source: "/config set " + config.Printable(filepath.Join(home, ".abhed", "config.json")),
		Key: config.Printable(path), Value: config.RedactValue(path, value), Decision: "refused", Reason: why,
		Principal: toolset.LocalPrincipal(inAgentCommand())})
}

// lockUserConfig locks ~/.abhed/config.json against another /config set.
func lockUserConfig() (func(), error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".abhed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return config.LockFile(filepath.Join(dir, "config.json.lock"))
}

// userFileConfig is the defaults with only the person's own file laid over them.
func userFileConfig() (config.Config, error) {
	cfg := config.Default()
	home, err := os.UserHomeDir()
	if err != nil {
		return cfg, err
	}
	file := filepath.Join(home, ".abhed", "config.json")
	data, err := os.ReadFile(file) // #nosec G304 -- the person's own ~/.abhed/config.json
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("%s is not valid JSON, so it was not changed: %w", file, err)
	}
	return cfg, nil
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
	if data, err := os.ReadFile(file); err == nil { // #nosec G304 -- the person's own ~/.abhed/config.json
		if err := json.Unmarshal(data, &doc); err != nil {
			return file, fmt.Errorf("%s is not valid JSON, so it was not changed: %w", file, err)
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
	// A fresh name each time: a fixed one could be a planted link.
	tmp, err := os.CreateTemp(filepath.Dir(file), "config.json.*.tmp")
	if err != nil {
		return file, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return file, err
	}
	if err := tmp.Close(); err != nil {
		return file, err
	}
	return file, os.Rename(tmp.Name(), file)
}
