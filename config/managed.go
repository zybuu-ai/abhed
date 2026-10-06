package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/internal/managed"
)

// LoadManaged is the built-in defaults with only the managed configuration
// applied: what a program that keeps no config file of its own runs under.
func LoadManaged() (Config, error) {
	cfg := Default()
	if err := mergeManaged(&cfg); err != nil {
		return cfg, err
	}
	warnUnknown(cfg.Unknown)
	warnNeverAllows(cfg.Permissions.Allow)
	warnNotYetInEffect(cfg)
	return cfg, nil
}

// mergeManaged applies the managed file, if there is one, over cfg and
// records which settings it made.
func mergeManaged(cfg *Config) error {
	path := managed.ConfigFile
	// Only nothing at the path means unmanaged; a file that cannot be read,
	// or a link to nothing, is an error.
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	data, err := readMerge(cfg, path)
	if err != nil {
		return err
	}
	if data == nil {
		return fmt.Errorf("managed configuration %s is a link to nothing", path)
	}
	cfg.Managed = true
	cfg.ManagedKeys = managedKeys(data)
	for i := range cfg.Unknown {
		cfg.Unknown[i].Managed = cfg.Unknown[i].File == path
	}
	return nil
}

// managedKeys lists the settings a file makes, as sorted dotted paths.
func managedKeys(data []byte) []string {
	var raw any
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	var out []string
	walkSet("", raw, reflect.TypeFor[Config](), func(path string, _ any) { out = append(out, path) })
	sort.Strings(out)
	return out
}

// walkSet calls set with the path and value of each setting that decoding v
// into t would change. A struct is followed into; anything else, a list or map
// entry included, is one setting.
func walkSet(path string, v any, t reflect.Type, set func(path string, v any)) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	obj, isObj := v.(map[string]any)
	switch {
	case isObj && t.Kind() == reflect.Struct &&
		!reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]()):
		fields := jsonFields(t)
		for k, e := range obj {
			if f, ok := fieldFor(fields, k); ok && !annotation(k) {
				walkSet(join(path, jsonName(f)), e, f.Type, set)
			}
		}
	case isObj && t.Kind() == reflect.Map:
		// Decoding replaces each entry it names whole, so the entry is the setting.
		for k, e := range obj {
			set(join(path, k), e)
		}
	case v == nil && t.Kind() != reflect.Slice && t.Kind() != reflect.Map:
		// null leaves a plain value as it was.
	case path != "":
		set(path, v)
	}
}

// jsonName is the name a field is written under, as the docs spell it.
func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" {
		name = f.Name
	}
	return strings.ToLower(name)
}

// ManagedSets reports whether the managed configuration made the setting at
// path, a dotted path such as "permissions.mode", or one inside or above it.
func (c Config) ManagedSets(path string) bool {
	for _, k := range c.ManagedKeys {
		if k == path || strings.HasPrefix(k, path+".") || strings.HasPrefix(path, k+".") {
			return true
		}
	}
	return false
}

// AllowLocked reports whether no caller may add allow rules: the managed
// configuration sets a permissions key, whichever one. Every surface asks this.
func (c Config) AllowLocked() bool { return c.ManagedSets("permissions") }

// dropLockedAllow sets aside the allow rules the user's and the workspace's
// files added when the managed file locks allow rules without listing its own.
func dropLockedAllow(c *Config, userFile, workspaceFile, settingsFile string) {
	// Git extensions opted in widen what runs unasked, as an allow rule does.
	if c.AllowLocked() && !c.ManagedSets("permissions.git_extensions") && len(c.Permissions.GitExtensions) > 0 {
		for _, name := range c.Permissions.GitExtensions {
			file := userFile
			switch c.RuleLayer("git_extensions", name) {
			case LayerSettings:
				file = settingsFile
			case LayerWorkspace:
				file = workspaceFile
			}
			c.SetAside = append(c.SetAside, SetAsideKey{File: file, Key: "permissions.git_extensions", Value: name,
				Reason: "the managed configuration sets the permissions, so only its own permissions.git_extensions opts git extensions in"})
		}
		c.Permissions.GitExtensions = nil
	}
	if !c.AllowLocked() || c.ManagedSets("permissions.allow") {
		return
	}
	var kept []string
	for _, r := range c.Permissions.Allow {
		file, layer := userFile, c.RuleLayer("allow", r)
		switch layer {
		case LayerUser:
		case LayerSettings:
			file = settingsFile
		case LayerWorkspace:
			file = workspaceFile
		default:
			kept = append(kept, r)
			continue
		}
		delete(c.ruleLayers, "allow\x00"+strings.TrimSpace(r))
		c.SetAside = append(c.SetAside, SetAsideKey{File: file, Layer: layer, Key: "permissions.allow", Value: r,
			Reason: "the managed configuration sets the permissions, so only its own permissions.allow adds allow rules"})
	}
	if len(kept) < len(c.Permissions.Allow) {
		// The files' list had replaced the built-in rules; put those back.
		c.Permissions.Allow = dedupe(append(slices.Clone(Default().Permissions.Allow), kept...))
		c.SetKeys = slices.DeleteFunc(c.SetKeys, func(k string) bool { return k == "permissions.allow" })
	}
}

// dedupe keeps the first of each rule, in order, comparing them trimmed as
// the rule layers are keyed: " bash(ls*)" is the same rule as "bash(ls*)".
func dedupe(rules []string) []string {
	seen := map[string]bool{}
	out := rules[:0]
	for _, r := range rules {
		if k := strings.TrimSpace(r); !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

// Offered reports whether a provider is one to offer for choosing: the default,
// one not built in, or a built-in one a configuration file names.
func (c Config) Offered(name string) bool {
	if name == c.Model.Default || c.Sets("model.providers."+name) {
		return true
	}
	_, builtIn := Default().Model.Providers[name]
	return !builtIn
}

// Sets reports whether a configuration file made the setting at path, a
// dotted path such as "sandbox.max_memory_mb", whatever value it gave.
func (c Config) Sets(path string) bool {
	for _, k := range c.SetKeys {
		if k == path || strings.HasPrefix(k, path+".") {
			return true
		}
	}
	return false
}

// Overrides are what a caller sets over the loaded configuration: flags on
// the command line, Options in the SDK. Empty fields change nothing.
type Overrides struct {
	Mode           string
	SyntaxCheck    string
	MaxTurns       int
	Allow          []string
	Deny           []string
	AdditionalDirs []string
	// MemoryAuto, when set, turns auto memory on or off.
	MemoryAuto *bool
}

// ManagedError is an override refused because it would loosen a setting the
// managed configuration made.
type ManagedError struct {
	Key    string // the setting, e.g. permissions.mode
	Value  string // what the caller asked for
	Reason string
	File   string // the managed file, whose owner can change the setting
}

func (e *ManagedError) Error() string {
	return fmt.Sprintf("%s %s is refused: %s (set in %s)", e.Key, e.Value, e.Reason, e.File)
}

// refuse builds a ManagedError naming the managed file.
func refuse(key, value, reason string) error {
	return &ManagedError{Key: key, Value: value, Reason: reason, File: managed.ConfigFile}
}

// knownMode reports whether m names a permission mode.
func knownMode(m string) bool {
	switch m {
	case "default", "accept-edits", "plan", "auto", "bypass":
		return true
	}
	return false
}

// Apply returns c with o applied. Deny rules are added. Without a managed
// configuration every other override replaces or adds to the setting, as it
// always has; under one, an override may tighten what the managed file set
// and never loosen it, and bypass mode is refused. A managed limits.max_turns
// of zero or less binds nothing. An unknown mode is an error.
//
// The binding is only as good as ManagedKeys: a Config that Load did not
// build has none, and Apply then refuses nothing.
func (c Config) Apply(o Overrides) (Config, error) {
	if o.Mode != "" {
		switch {
		case !knownMode(o.Mode):
			return c, fmt.Errorf("unknown permission mode %q: want default, accept-edits, plan, auto or bypass", o.Mode)
		case c.ManagedSets("permissions.mode") && o.Mode != c.Permissions.Mode && o.Mode != "plan":
			return c, refuse("permissions.mode", o.Mode, fmt.Sprintf(
				"the managed configuration sets %q; only that mode or plan may be chosen", c.Permissions.Mode))
		case c.Managed && o.Mode == "bypass":
			return c, refuse("permissions.mode", o.Mode, "bypass mode is disabled under a managed configuration")
		}
		c.Permissions.Mode = o.Mode
	}
	if o.SyntaxCheck != "" {
		if c.ManagedSets("tools.syntax_check") && syntaxRank(o.SyntaxCheck) < syntaxRank(c.Tools.SyntaxCheck) {
			return c, refuse("tools.syntax_check", o.SyntaxCheck, fmt.Sprintf(
				"the managed configuration sets %q, which may only be made stricter", orRefuse(c.Tools.SyntaxCheck)))
		}
		c.Tools.SyntaxCheck = o.SyntaxCheck
	}
	if o.MaxTurns > 0 {
		if c.ManagedSets("limits.max_turns") && c.Limits.MaxTurns > 0 && o.MaxTurns > c.Limits.MaxTurns {
			return c, refuse("limits.max_turns", fmt.Sprint(o.MaxTurns), fmt.Sprintf(
				"the managed configuration allows at most %d", c.Limits.MaxTurns))
		}
		c.Limits.MaxTurns = o.MaxTurns
	}
	if o.MemoryAuto != nil {
		if c.ManagedSets("memory.auto") && *o.MemoryAuto != c.Memory.Auto {
			return c, refuse("memory.auto", fmt.Sprint(*o.MemoryAuto), fmt.Sprintf(
				"the managed configuration sets it to %v", c.Memory.Auto))
		}
		c.Memory.Auto = *o.MemoryAuto
	}
	if len(o.Allow) > 0 && c.AllowLocked() {
		return c, refuse("permissions.allow", strings.Join(o.Allow, ","),
			"the managed configuration sets the permissions, so allow rules may not be added")
	}
	if len(o.AdditionalDirs) > 0 && c.ManagedSets("additional_dirs") {
		return c, refuse("additional_dirs", strings.Join(o.AdditionalDirs, ","),
			"the managed configuration sets the additional directories, which may not be added to")
	}
	warnNeverAllows(o.Allow)
	c.ruleLayers = maps.Clone(c.ruleLayers)
	c.Permissions.Allow = append(append([]string{}, c.Permissions.Allow...), o.Allow...)
	c.Permissions.Deny = append(append([]string{}, c.Permissions.Deny...), o.Deny...)
	c.noteRuleLayer(LayerFlag)
	// Copied, so the result never shares a list with the configuration it came from.
	c.AdditionalDirs = append(append([]string{}, c.AdditionalDirs...), o.AdditionalDirs...)
	return c, nil
}

// syntaxRank orders tools.syntax_check from loosest to strictest.
func syntaxRank(v string) int {
	switch orRefuse(v) {
	case "off":
		return 0
	case "report":
		return 1
	case "refuse":
		return 2
	}
	return -1
}

func orRefuse(v string) string {
	if v == "" {
		return "refuse"
	}
	return v
}

// Layers a permission rule can come from, as RuleLayer names them.
const (
	LayerDefault   = "default"
	LayerUser      = "user"
	LayerSettings  = "settings" // a -settings file named for one run
	LayerWorkspace = "workspace"
	LayerManaged   = "managed"
	LayerFlag      = "flag"
	LayerEnv       = "env" // an ABHED_* variable
)

// noteRuleLayer credits the permission rules and extensions not yet
// credited to layer. The managed layer takes every entry of a list it sets,
// since it replaced that list; the others take only entries new to it.
func (c *Config) noteRuleLayer(layer string) {
	if c.ruleLayers == nil {
		c.ruleLayers = map[string]string{}
	}
	for list, rules := range map[string][]string{
		"allow": c.Permissions.Allow, "ask": c.Permissions.Ask, "deny": c.Permissions.Deny,
		"git_extensions": c.Permissions.GitExtensions,
	} {
		replaced := layer == LayerManaged && c.ManagedSets("permissions."+list)
		for _, r := range rules {
			k := list + "\x00" + strings.TrimSpace(r)
			if _, have := c.ruleLayers[k]; !have || replaced {
				c.ruleLayers[k] = layer
			}
		}
	}
	replaced := layer == LayerManaged && c.ManagedSets("extensions")
	for _, e := range c.Extensions {
		k := "extension\x00" + e.Name
		if _, have := c.ruleLayers[k]; !have || replaced {
			c.ruleLayers[k] = layer
		}
	}
}

// ExtensionLayer names where a configured extension came from, as
// RuleLayer does for a rule.
func (c Config) ExtensionLayer(name string) string {
	if l, ok := c.ruleLayers["extension\x00"+name]; ok {
		return l
	}
	return "config"
}

// RuleLayer names where a configured permission rule in list (allow, ask or
// deny) came from: default, user, workspace, managed or flag. A rule loading
// did not see, as in a Config built by hand, is "config".
func (c Config) RuleLayer(list, rule string) string {
	if l, ok := c.ruleLayers[list+"\x00"+strings.TrimSpace(rule)]; ok {
		return l
	}
	return "config"
}
