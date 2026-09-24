package config

import (
	"encoding/json"
	"fmt"
	"reflect"
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
	return cfg, nil
}

// mergeManaged applies the managed file, if there is one, over cfg and
// records which settings it made.
func mergeManaged(cfg *Config) error {
	path := managed.ConfigFile
	// Only a missing file means unmanaged; one that cannot be read is an error.
	data, err := readMerge(cfg, path)
	if err != nil || data == nil {
		return err
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
	walkSet("", raw, reflect.TypeFor[Config](), &out)
	sort.Strings(out)
	return out
}

// walkSet records the path of each value that decoding into t would change.
// A struct is followed into; anything else, a list or map entry included, is one setting.
func walkSet(path string, v any, t reflect.Type, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	obj, isObj := v.(map[string]any)
	switch {
	case isObj && t.Kind() == reflect.Struct &&
		!reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]()):
		fields := jsonFields(t)
		for k, e := range obj {
			if f, ok := fields[strings.ToLower(k)]; ok && !annotation(k) {
				walkSet(join(path, jsonName(f)), e, f.Type, out)
			}
		}
	case isObj && t.Kind() == reflect.Map:
		// Decoding replaces each entry it names whole, so the entry is the setting.
		for k := range obj {
			*out = append(*out, join(path, k))
		}
	case v == nil && t.Kind() != reflect.Slice && t.Kind() != reflect.Map:
		// null leaves a plain value as it was.
	case path != "":
		*out = append(*out, path)
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

// Overrides are what a caller sets over the loaded configuration: flags on
// the command line, Options in the SDK. Empty fields change nothing.
type Overrides struct {
	Mode           string
	SyntaxCheck    string
	MaxTurns       int
	Allow          []string
	Deny           []string
	AdditionalDirs []string
}

// ManagedError is an override refused because it would loosen a setting the
// managed configuration made.
type ManagedError struct {
	Key    string // the setting, e.g. permissions.mode
	Value  string // what the caller asked for
	Reason string
}

func (e *ManagedError) Error() string {
	return fmt.Sprintf("%s %s is refused: %s", e.Key, e.Value, e.Reason)
}

// Apply returns c with o applied. Deny rules are added. Without a managed
// configuration every other override replaces or adds to the setting, as it
// always has; under one, an override may tighten what the managed file set
// and never loosen it, and bypass mode is refused.
func (c Config) Apply(o Overrides) (Config, error) {
	if o.Mode != "" {
		switch {
		case c.ManagedSets("permissions.mode") && o.Mode != c.Permissions.Mode && o.Mode != "plan":
			return c, &ManagedError{Key: "permissions.mode", Value: o.Mode, Reason: fmt.Sprintf(
				"the managed configuration sets %q; only that mode or plan may be chosen", c.Permissions.Mode)}
		case c.Managed && o.Mode == "bypass":
			return c, &ManagedError{Key: "permissions.mode", Value: o.Mode,
				Reason: "bypass mode is disabled under a managed configuration"}
		}
		c.Permissions.Mode = o.Mode
	}
	if o.SyntaxCheck != "" {
		if c.ManagedSets("tools.syntax_check") && syntaxRank(o.SyntaxCheck) < syntaxRank(c.Tools.SyntaxCheck) {
			return c, &ManagedError{Key: "tools.syntax_check", Value: o.SyntaxCheck, Reason: fmt.Sprintf(
				"the managed configuration sets %q, which may only be made stricter", orRefuse(c.Tools.SyntaxCheck))}
		}
		c.Tools.SyntaxCheck = o.SyntaxCheck
	}
	if o.MaxTurns > 0 {
		if c.ManagedSets("limits.max_turns") && c.Limits.MaxTurns > 0 && o.MaxTurns > c.Limits.MaxTurns {
			return c, &ManagedError{Key: "limits.max_turns", Value: fmt.Sprint(o.MaxTurns), Reason: fmt.Sprintf(
				"the managed configuration allows at most %d", c.Limits.MaxTurns)}
		}
		c.Limits.MaxTurns = o.MaxTurns
	}
	if len(o.Allow) > 0 && c.ManagedSets("permissions.allow") {
		return c, &ManagedError{Key: "permissions.allow", Value: strings.Join(o.Allow, ","),
			Reason: "the managed configuration sets the allow rules, which may not be added to"}
	}
	if len(o.AdditionalDirs) > 0 && c.ManagedSets("additional_dirs") {
		return c, &ManagedError{Key: "additional_dirs", Value: strings.Join(o.AdditionalDirs, ","),
			Reason: "the managed configuration sets the additional directories, which may not be added to"}
	}
	// Copied, so the result never shares a list with the configuration it came from.
	c.Permissions.Allow = append(append([]string{}, c.Permissions.Allow...), o.Allow...)
	c.Permissions.Deny = append(append([]string{}, c.Permissions.Deny...), o.Deny...)
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
