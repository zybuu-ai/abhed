package controlplane

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// SettingScope says where a setting may be given a value.
type SettingScope string

const (
	// ScopeGlobal settings come from the server's configuration only.
	ScopeGlobal SettingScope = "global"
	// ScopeSandbox settings take their default from the configuration and
	// may be set per sandbox.
	ScopeSandbox SettingScope = "sandbox"
)

// Setting keys.
const (
	SetExecTimeout       = "exec.timeout_seconds"
	SetExecOutput        = "exec.output_bytes"
	SetRestartPolicy     = "restart.policy"
	SetMaxRestarts       = "restart.max_restarts"
	SetRestartBackoff    = "restart.backoff_seconds"
	SetMinTier           = "isolation.min_tier"
	SetMutationsPerMin   = "rate.mutations_per_minute"
	SetExecPerMin        = "rate.exec_per_minute"
	SetReconcileInterval = "reconcile.interval_seconds"
)

// Setting is one entry in the registry.
type Setting struct {
	Key   string
	Scope SettingScope
	// Config is the dotted configuration key that sets the global value.
	Config  string
	Default string
	Doc     string
	// Strict makes the configured value a ceiling even when the managed
	// file does not set it: a sandbox may never ask for weaker isolation
	// than the operator configured.
	Strict bool
	// rank orders values: a higher rank is tighter. It refuses a value the
	// setting does not take.
	rank func(string) (int64, error)
	// value reads the configured value, "" when unset.
	value func(config.Config) string
}

func intRank(lo, hi int64, lowerTighter bool) func(string) (int64, error) {
	return func(v string) (int64, error) {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < lo || n > hi {
			return 0, fmt.Errorf("a whole number from %d to %d", lo, hi)
		}
		if lowerTighter {
			return -n, nil
		}
		return n, nil
	}
}

func enumRank(ranks map[string]int64, says string) func(string) (int64, error) {
	return func(v string) (int64, error) {
		r, ok := ranks[v]
		if !ok {
			return 0, fmt.Errorf("%s", says)
		}
		return r, nil
	}
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// Restart policies.
const (
	RestartNever     = "never"
	RestartOnFailure = "on-failure"
	RestartAlways    = "always"
)

var tierRanks = map[string]int64{}

func init() {
	for _, t := range []sandbox.Tier{sandbox.TierNone, sandbox.TierProcess, sandbox.TierFence, sandbox.TierContainer, sandbox.TierVM} {
		tierRanks[string(t)] = int64(t.Strength())
	}
}

// TierMeets reports whether tier is at least as strong as min. An unknown
// tier meets nothing; an empty min is met by anything.
func TierMeets(tier, min string) bool {
	if min == "" {
		return true
	}
	have, ok := tierRanks[tier]
	need, known := tierRanks[min]
	return ok && known && have >= need
}

// Registry lists every setting the control plane reads.
var Registry = []Setting{
	{Key: SetExecTimeout, Scope: ScopeSandbox, Config: "control_plane.exec_timeout_seconds", Default: "600",
		Doc: "longest one :exec may run", rank: intRank(1, 86400, true),
		value: func(c config.Config) string { return itoa(c.ControlPlane.ExecTimeoutSeconds) }},
	{Key: SetExecOutput, Scope: ScopeSandbox, Config: "control_plane.exec_output_bytes", Default: strconv.Itoa(16 << 20),
		Doc: "most output one :exec may write, stdout and stderr together", rank: intRank(1024, 1<<30, true),
		value: func(c config.Config) string {
			if c.ControlPlane.ExecOutputBytes == 0 {
				return ""
			}
			return strconv.FormatInt(c.ControlPlane.ExecOutputBytes, 10)
		}},
	{Key: SetRestartPolicy, Scope: ScopeSandbox, Config: "control_plane.restart_policy", Default: RestartNever,
		Doc:   "when a sandbox that ended is started again: never, on-failure or always",
		rank:  enumRank(map[string]int64{RestartAlways: 0, RestartOnFailure: 1, RestartNever: 2}, "never, on-failure or always"),
		value: func(c config.Config) string { return c.ControlPlane.RestartPolicy }},
	{Key: SetMaxRestarts, Scope: ScopeSandbox, Config: "control_plane.max_restarts", Default: "5",
		Doc: "restarts the policy makes before it gives up", rank: intRank(0, 1000, true),
		value: func(c config.Config) string {
			if c.ControlPlane.MaxRestarts == nil {
				return ""
			}
			return strconv.Itoa(*c.ControlPlane.MaxRestarts)
		}},
	{Key: SetRestartBackoff, Scope: ScopeSandbox, Config: "control_plane.restart_backoff_seconds", Default: "10",
		Doc: "delay before the first restart; each next one doubles, up to an hour", rank: intRank(1, 3600, false),
		value: func(c config.Config) string { return itoa(c.ControlPlane.RestartBackoffSeconds) }},
	{Key: SetMinTier, Scope: ScopeSandbox, Config: "sandbox.min_tier", Default: "", Strict: true,
		Doc: "weakest isolation a sandbox may run under", rank: func(v string) (int64, error) {
			if v == "" {
				return 0, nil
			}
			return enumRank(tierRanks, "none, process, fence, container or vm")(v)
		},
		value: func(c config.Config) string { return c.Sandbox.MinTier }},
	{Key: SetMutationsPerMin, Scope: ScopeGlobal, Config: "control_plane.mutations_per_minute", Default: "120",
		Doc: "changes to sandboxes one caller may make a minute", rank: intRank(1, 100000, true),
		value: func(c config.Config) string { return itoa(c.ControlPlane.MutationsPerMinute) }},
	{Key: SetExecPerMin, Scope: ScopeGlobal, Config: "control_plane.exec_per_minute", Default: "60",
		Doc: ":exec calls one caller may make a minute", rank: intRank(1, 100000, true),
		value: func(c config.Config) string { return itoa(c.ControlPlane.ExecPerMinute) }},
	{Key: SetReconcileInterval, Scope: ScopeGlobal, Config: "control_plane.reconcile_seconds", Default: "10",
		Doc: "how often every sandbox is checked against its driver", rank: intRank(1, 3600, true),
		value: func(c config.Config) string { return itoa(c.ControlPlane.ReconcileSeconds) }},
}

func lookup(key string) (Setting, bool) {
	i := slices.IndexFunc(Registry, func(s Setting) bool { return s.Key == key })
	if i < 0 {
		return Setting{}, false
	}
	return Registry[i], true
}

// Settings is the registry resolved against one configuration: each
// setting's global value, and which of them are ceilings.
type Settings struct {
	global  map[string]string
	ceiling map[string]bool
	source  map[string]string
}

// NewSettings resolves the registry from configured values (key → value,
// missing keys take the default). A key in ceilings caps what a sandbox may
// ask for: a sandbox may set a tighter value, never a looser one.
func NewSettings(values map[string]string, ceilings map[string]bool) (*Settings, error) {
	st := &Settings{global: map[string]string{}, ceiling: map[string]bool{}, source: map[string]string{}}
	for k := range values {
		if _, ok := lookup(k); !ok {
			return nil, fmt.Errorf("%w: unknown setting %q", ErrInvalid, k)
		}
	}
	for _, def := range Registry {
		v, ok := values[def.Key]
		st.source[def.Key] = "configured"
		if !ok || v == "" {
			v = def.Default
			st.source[def.Key] = "default"
		}
		if _, err := def.rank(v); err != nil {
			return nil, fmt.Errorf("%w: %s (%s) is %q; use %v", ErrInvalid, def.Key, def.Config, v, err)
		}
		st.global[def.Key] = v
		if ceilings[def.Key] {
			st.ceiling[def.Key] = true
			st.source[def.Key] = "managed"
		} else if def.Strict && ok && v != "" {
			st.ceiling[def.Key] = true
		}
	}
	return st, nil
}

// SettingsFromConfig resolves the registry from cfg. A value the managed
// file sets is a ceiling.
func SettingsFromConfig(cfg config.Config) (*Settings, error) {
	values, ceilings := map[string]string{}, map[string]bool{}
	for _, def := range Registry {
		if v := def.value(cfg); v != "" {
			values[def.Key] = v
		}
		if cfg.ManagedSets(def.Config) {
			ceilings[def.Key] = true
		}
	}
	st, err := NewSettings(values, ceilings)
	if err != nil {
		return nil, err
	}
	// A value no file set is the built-in configuration's, not the operator's.
	for _, def := range Registry {
		if st.source[def.Key] == "configured" && !cfg.Sets(def.Config) {
			st.source[def.Key] = "built-in"
		}
	}
	return st, nil
}

// Check refuses per-sandbox settings that name an unknown or global-only
// setting, give a value the setting does not take, or loosen a ceiling.
func (st *Settings) Check(sb map[string]string) error {
	for k, v := range sb {
		def, ok := lookup(k)
		if !ok {
			return fmt.Errorf("%w: unknown setting %q", ErrInvalid, k)
		}
		if def.Scope != ScopeSandbox {
			return fmt.Errorf("%w: %s is set for the whole server, not per sandbox", ErrInvalid, k)
		}
		r, err := def.rank(v)
		if err != nil {
			return fmt.Errorf("%w: %s is %q; use %v", ErrInvalid, k, v, err)
		}
		if st.ceiling[k] {
			cr, _ := def.rank(st.global[k])
			if r < cr {
				return fmt.Errorf("%w: %s=%q is looser than this server allows (%q)", ErrInvalid, k, v, st.global[k])
			}
		}
	}
	return nil
}

// Values are the settings in force for one sandbox.
type Values map[string]string

// For is the settings in force for a sandbox with sb set: each of its
// values, held to the ceilings in force now, else the global value.
func (st *Settings) For(sb map[string]string) Values {
	out := Values{}
	for _, def := range Registry {
		v := st.global[def.Key]
		if own, ok := sb[def.Key]; ok && def.Scope == ScopeSandbox {
			r, err := def.rank(own)
			cr, _ := def.rank(v)
			// A ceiling tightened since the sandbox was set holds it too.
			if err == nil && (!st.ceiling[def.Key] || r >= cr) {
				v = own
			}
		}
		out[def.Key] = v
	}
	return out
}

// Int is a numeric setting's value.
func (v Values) Int(key string) int64 {
	n, _ := strconv.ParseInt(v[key], 10, 64)
	return n
}

// Seconds is a setting in seconds as a duration.
func (v Values) Seconds(key string) time.Duration { return time.Duration(v.Int(key)) * time.Second }

// SettingInfo describes one setting as resolved, for preflight and docs.
type SettingInfo struct {
	Key     string       `json:"key"`
	Scope   SettingScope `json:"scope"`
	Config  string       `json:"config"`
	Value   string       `json:"value"`
	Source  string       `json:"source"`
	Ceiling bool         `json:"ceiling"`
	Doc     string       `json:"doc"`
}

// List describes every setting as resolved.
func (st *Settings) List() []SettingInfo {
	out := make([]SettingInfo, 0, len(Registry))
	for _, def := range Registry {
		out = append(out, SettingInfo{Key: def.Key, Scope: def.Scope, Config: def.Config, Value: st.global[def.Key],
			Source: st.source[def.Key], Ceiling: st.ceiling[def.Key], Doc: def.Doc})
	}
	return out
}
