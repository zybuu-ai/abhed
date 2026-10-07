package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/internal/nlink"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/visible"
)

// A workspace's own .abhed/config.json arrives with the repository, so it is
// untrusted until the person trusts that exact content (workspace-trust.md).

// TrustEnv, set to 1, trusts the workspace configuration for one run.
const TrustEnv = "ABHED_TRUST_WORKSPACE"

// TrustChoice is a caller's say over the workspace configuration.
type TrustChoice string

const (
	// TrustAsStored follows the person's recorded decision, or TrustEnv.
	TrustAsStored TrustChoice = ""
	// TrustGranted trusts the file for this load, as -trust-workspace does.
	TrustGranted TrustChoice = "trusted"
	// TrustRefused takes only what tightens, whatever was recorded.
	TrustRefused TrustChoice = "untrusted"
)

// LoadOptions tune LoadWith.
type LoadOptions struct {
	Trust TrustChoice
	// Quiet leaves the untrusted warning to the caller, which may prompt instead.
	Quiet bool
	// Settings is a file the person named for this run (-settings), merged
	// over their own file as one of their own: managed-only keys are set
	// aside, and the managed file still binds. SettingsName says where it came from.
	Settings     []byte
	SettingsName string
}

// WorkspaceTrust is what loading decided about the workspace's config file.
type WorkspaceTrust struct {
	Workspace string `json:"workspace"`
	File      string `json:"file,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Trusted   bool   `json:"trusted"`
	// Reason is none (no file), home (it is the user's own config), stored,
	// flag, env, new, changed, declined or refused.
	Reason string `json:"reason"`
	// Applied are the settings an untrusted file made that only tighten.
	Applied []string `json:"applied,omitempty"`
	// Ignored are the settings an untrusted file made that were not applied.
	Ignored []IgnoredKey `json:"ignored,omitempty"`

	// Agents are the workspace's definition files (.abhed/agents/*.md),
	// relative to it, that the decision below covers.
	Agents []string `json:"agents,omitempty"`
	// AgentsSHA256 is one hash over every definition's path and content.
	AgentsSHA256 string `json:"agents_sha256,omitempty"`
	// AgentsTrusted is whether the definitions load. It is decided apart
	// from the configuration file, against AgentsSHA256.
	AgentsTrusted bool `json:"agents_trusted,omitempty"`
	// AgentsReason is as Reason, for the definitions; none when there are none.
	AgentsReason string `json:"agents_reason,omitempty"`
	// AgentsProblems are definition files that were refused before any
	// decision: a link, a second name, too large. They never load.
	AgentsProblems []string `json:"agents_problems,omitempty"`

	// UsersFile is the workspace's .abhed/users.json, when there is one.
	UsersFile string `json:"users_file,omitempty"`
	// UsersTrusted is whether an accounts file inside the workspace is read,
	// and UsersReason why: as Reason, or home.
	UsersTrusted bool   `json:"users_trusted,omitempty"`
	UsersReason  string `json:"users_reason,omitempty"`

	agentFiles []AgentFile
}

// IgnoredKey is one setting of an untrusted file, with its value as written
// and secrets redacted.
type IgnoredKey struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Reason is set when the value was refused rather than merely not trusted.
	Reason string `json:"reason,omitempty"`
}

// deploymentSections decide who may sign in and where the record goes, so
// leaving them out opens a server rather than narrowing it.
var deploymentSections = []string{"auth", "storage", "server"}

// IgnoredDeployment lists the ignored settings under auth, storage or server.
func (w WorkspaceTrust) IgnoredDeployment() []string {
	var out []string
	for _, k := range w.Ignored {
		for _, sec := range deploymentSections {
			if k.Key == sec || strings.HasPrefix(k.Key, sec+".") {
				out = append(out, k.Key)
			}
		}
	}
	return out
}

// DeploymentError refuses to run a server-side command without settings an
// untrusted file made, since running without them fails open.
func (w WorkspaceTrust) DeploymentError(command string) error {
	keys := w.IgnoredDeployment()
	if len(keys) == 0 {
		return nil
	}
	return fmt.Errorf("the workspace configuration %s sets %s, but it is not trusted. "+
		"%s refuses to run without them: it would start with no sign-in or no durable record. "+
		"Review it with `abhed trust`, then trust it with `abhed trust grant` or run `abhed -trust-workspace %s`; "+
		"or move these settings to ~/.abhed/config.json or the managed /etc/abhed/config.json",
		Printable(w.File), strings.Join(keys, ", "), command, command)
}

// NeedsDecision reports whether the person should be asked: an untrusted file
// that would change something, and no answer yet for this content.
func (w WorkspaceTrust) NeedsDecision() bool {
	return w.configNeedsDecision() || w.agentsNeedDecision()
}

func (w WorkspaceTrust) configNeedsDecision() bool {
	return w.File != "" && !w.Trusted && len(w.Ignored) > 0 &&
		(w.Reason == "new" || w.Reason == "changed")
}

// IgnoredKeys are the names of the ignored settings.
func (w WorkspaceTrust) IgnoredKeys() []string {
	out := make([]string, len(w.Ignored))
	for i, k := range w.Ignored {
		out[i] = k.Key
	}
	return out
}

// Warning is the one-line notice for an untrusted file that was partly
// ignored, or "" when there is nothing to say.
func (w WorkspaceTrust) Warning() string {
	var ignored []string
	what, reason, tighten := Printable(w.File), w.Reason, ""
	if !w.Trusted && len(w.Ignored) > 0 {
		ignored = w.IgnoredKeys()
		tighten = " Only its deny, ask and other tightening settings apply."
	}
	if agents := w.IgnoredAgents(); len(agents) > 0 {
		ignored = append(ignored, agents...)
		if tighten == "" {
			what, reason = Printable(filepath.Join(w.Workspace, filepath.FromSlash(WorkspaceAgentsDir))), w.AgentsReason
		}
	}
	if len(ignored) == 0 {
		return ""
	}
	why := "is not trusted"
	switch reason {
	case "changed":
		why = "changed since it was trusted"
	case "declined":
		why = "was not trusted when you were asked"
	}
	return fmt.Sprintf("the workspace configuration %s %s; ignored %s.%s Review it with `abhed trust`",
		what, why, strings.Join(ignored, ", "), tighten)
}

// warnUntrusted warns once per process: a command that loads the workspace
// twice (resolve, then its agent; ACP, per session) used to say it twice.
func warnUntrusted(w WorkspaceTrust) {
	s := w.Warning()
	if s == "" {
		return
	}
	warnedMu.Lock()
	first := !warned["untrusted\x00"+s]
	warned["untrusted\x00"+s] = true
	warnedMu.Unlock()
	if first {
		fmt.Fprintf(warnOut, "abhed: warning: %s\n", s)
	}
}

// canonical is the absolute path with links resolved, which keys the store.
func canonical(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// GrantFor is choice for the workspace dir, where choice was given for the
// workspace base: -trust-workspace trusts the workspace it names, not every
// other one a client later opens, which keep their recorded decision.
func GrantFor(choice TrustChoice, base, dir string) TrustChoice {
	if choice == TrustGranted && canonical(dir) != canonical(base) {
		return TrustAsStored
	}
	return choice
}

// HashOf is the content hash trust is keyed by.
func HashOf(data []byte) string { return hashOf(data) }

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// mergeWorkspace merges the workspace's file: whole when trusted, and only
// what tightens when not. userFile is the user config already merged.
func mergeWorkspace(cfg *Config, workspace, userFile string, o LoadOptions) (WorkspaceTrust, error) {
	st := WorkspaceTrust{Workspace: canonical(workspace), Reason: "none"}
	// The definitions are decided on their own content, whether or not
	// there is a configuration file beside them.
	readWorkspaceAgents(workspace, &st)
	if isHome(workspace) {
		st.AgentsTrusted, st.AgentsReason = len(st.Agents) > 0, "home"
	} else {
		st.AgentsTrusted, st.AgentsReason = decideAgents(st, o)
	}
	path := filepath.Join(workspace, ".abhed", "config.json")
	if n, err := nlink.Linked(path); err != nil {
		return st, fmt.Errorf("read %s: %w", path, err)
	} else if n > 0 {
		return st, fmt.Errorf("refusing to load the configuration: %w", nlink.Refusal(path, n))
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the workspace's config, read to be classified
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("read %s: %w", path, err)
	}
	st.File, st.SHA256 = path, hashOf(data)
	// The workspace is the home directory: this is the user's own file.
	if userFile != "" && sameFile(path, userFile) {
		st.Trusted, st.Reason = true, "home"
		return st, nil
	}
	st.Trusted, st.Reason = decide(st, o)
	if st.Trusted {
		auto, depth, depthKey := cfg.Memory.Auto, cfg.Memory.ImportDepth, cfg.MemoryImportDepth()
		if _, err := mergeData(cfg, path, data); err != nil {
			return st, err
		}
		// Trust does not reach these: a workspace never makes a managed-only
		// setting, may only turn auto memory off, and may only import less deep.
		setAside(cfg, path, LayerWorkspace)
		noteOrigins(cfg, path, LayerWorkspace, data)
		if cfg.Memory.Auto && !auto {
			cfg.Memory.Auto = false
			cfg.SetKeys = slices.DeleteFunc(cfg.SetKeys, func(k string) bool { return k == "memory.auto" })
			cfg.SetAside = append(cfg.SetAside, SetAsideKey{File: path, Layer: LayerWorkspace, Key: "memory.auto",
				Reason: "a workspace may only turn auto memory off"})
		}
		if cfg.MemoryImportDepth() > depthKey {
			cfg.Memory.ImportDepth = depth
			cfg.SetKeys = slices.DeleteFunc(cfg.SetKeys, func(k string) bool { return k == "memory.import_depth" })
			cfg.SetAside = append(cfg.SetAside, SetAsideKey{File: path, Layer: LayerWorkspace, Key: "memory.import_depth",
				Reason: "a workspace may only make imports shallower"})
		}
		return st, nil
	}
	return st, tighten(cfg, path, data, &st)
}

// decide applies a caller's choice, then TrustEnv, then the stored decision.
func decide(st WorkspaceTrust, o LoadOptions) (bool, string) {
	switch o.Trust {
	case TrustRefused:
		return false, "refused"
	case TrustGranted:
		return true, "flag"
	}
	if v := os.Getenv(TrustEnv); v == "1" || strings.EqualFold(v, "true") {
		return true, "env"
	}
	e, ok, err := lookupTrust(st.Workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: warning: the trust store is unreadable, so no workspace is trusted: %v\n", err)
		return false, "new"
	}
	switch {
	case !ok || e.SHA256 == "":
		// A record made for agent definitions alone decided nothing about the file.
		return false, "new"
	case e.SHA256 != st.SHA256:
		return false, "changed"
	case e.Decision == decisionTrusted:
		return true, "stored"
	}
	return false, "declined"
}

func sameFile(a, b string) bool {
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	return err == nil && os.SameFile(ia, ib)
}

// tighten applies the settings of an untrusted file that only make Abhed
// stricter, and records every other one as ignored.
func tighten(cfg *Config, path string, data []byte, st *WorkspaceTrust) error {
	var ws Config
	if err := json.Unmarshal(data, &ws); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	var raw any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	_ = dec.Decode(&raw)
	cfg.Unknown = append(cfg.Unknown, unknownKeys(path, data, reflect.TypeFor[Config]())...)
	// A rule that does not parse is set aside here, so it cannot stop Abhed starting.
	for key, list := range map[string]*[]string{"permissions.deny": &ws.Permissions.Deny, "permissions.ask": &ws.Permissions.Ask} {
		var ok []string
		for _, r := range *list {
			if _, err := policy.ParseRule(r); err != nil {
				st.Ignored = append(st.Ignored, IgnoredKey{Key: key, Value: shortJSON(r), Reason: Printable(err.Error())})
				continue
			}
			ok = append(ok, r)
		}
		*list = ok
	}
	type setting struct {
		key string
		val any
	}
	var set []setting
	walkSet("", raw, reflect.TypeFor[Config](), func(p string, v any) { set = append(set, setting{p, v}) })
	sort.Slice(set, func(i, j int) bool { return set[i].key < set[j].key })
	for _, s := range set {
		r := ruleFor(s.key)
		if r.apply != nil && r.apply(cfg, &ws) {
			st.Applied = append(st.Applied, s.key)
			if WebKey(s.key) {
				// Applied again over the managed file, which may turn it on.
				in := webIntent{key: s.key, file: path, layer: LayerWorkspace}
				switch s.key {
				case "web_search.max_results":
					in.limit = ws.WebSearch.MaxResults
				case "web_fetch.max_chars":
					in.limit = ws.WebFetch.MaxChars
				default:
					in.off = true
				}
				cfg.webIntents = append(cfg.webIntents, in)
				continue
			}
			cfg.SetKeys = append(cfg.SetKeys, s.key)
			noteOrigin(cfg, path, LayerWorkspace, s.key, s.val)
			continue
		}
		st.Ignored = append(st.Ignored, IgnoredKey{Key: Printable(s.key), Value: shortJSON(redact(s.key, s.val))})
	}
	sort.SliceStable(st.Ignored, func(i, j int) bool { return st.Ignored[i].Key < st.Ignored[j].Key })
	return nil
}

// secretKey names a field whose value is a credential or carries one; a
// field naming an environment variable, or a limit, is not.
func secretKey(k string) bool {
	k = strings.ToLower(k)
	if strings.HasSuffix(k, "_env") || strings.HasPrefix(k, "max_") {
		return false
	}
	for _, s := range []string{"api_key", "apikey", "secret", "password", "passwd", "token", "dsn", "header", "credential", "authorization"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return k == "env" || k == "key"
}

// redact replaces credentials in a value about to be shown: the values of
// secret-looking fields, free-form maps that may hold one, the value after a
// secret-looking flag in args, and passwords and secret query values in URLs.
func redact(key string, v any) any {
	last := key
	if i := strings.LastIndex(key, "."); i >= 0 {
		last = key[i+1:]
	}
	if secretKey(last) || last == "extra" || last == "body" {
		return redactAll(v)
	}
	if list, ok := v.([]any); ok && last == "args" {
		return redactArgs(list)
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = redact(k, e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redact(key, e)
		}
		return out
	case string:
		return redactURL(t)
	}
	return v
}

// redactAll keeps a secret field's shape and names, never its values.
func redactAll(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k := range t {
			out[k] = "[redacted]"
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = "[redacted]"
		}
		return out
	case nil:
		return nil
	}
	return "[redacted]"
}

// redactArgs hides the value that follows a flag such as --token, and the
// value of one written --token=value.
func redactArgs(list []any) []any {
	out := make([]any, len(list))
	hideNext := false
	for i, e := range list {
		a, ok := e.(string)
		switch {
		case !ok:
			out[i], hideNext = redact("", e), false
			continue
		case hideNext:
			out[i], hideNext = "[redacted]", false
			continue
		}
		out[i] = redactURL(a)
		if name, val, has := strings.Cut(strings.TrimLeft(a, "-"), "="); strings.HasPrefix(a, "-") && secretFlag(name) {
			if has {
				out[i] = a[:len(a)-len(val)] + "[redacted]"
			} else {
				hideNext = true
			}
		}
	}
	return out
}

func secretFlag(name string) bool {
	return name != "" && secretKey(strings.ReplaceAll(name, "-", "_"))
}

func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return s
	}
	changed := false
	if u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), "redacted")
			changed = true
		}
	}
	if u.RawQuery != "" {
		q := u.Query()
		for name := range q {
			if secretFlag(name) {
				q.Set(name, "redacted")
				changed = true
			}
		}
		if changed {
			u.RawQuery = q.Encode()
		}
	}
	if !changed {
		return s
	}
	return u.String()
}

func shortJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	s := []rune(Printable(string(b)))
	if len(s) <= 80 {
		return string(s)
	}
	return string(s[:77]) + "..."
}

// Printable escapes what a terminal would act on, and what draws nothing,
// newlines too, so text from the file cannot draw lines of its own in the
// prompt. It is internal/visible's escaper, as every surface uses.
func Printable(s string) string { return visible.Text(s, false, true) }

// PrintableURL is Printable with a password or secret query value hidden.
func PrintableURL(s string) string { return Printable(redactURL(s)) }

// PrintableText is Printable keeping newlines, for a framed body.
func PrintableText(s string) string { return visible.Text(s, true, true) }

// InspectWorkspace reports the workspace file's recorded decision and what
// it would change once trusted, without applying it.
func InspectWorkspace(workspace string) (WorkspaceTrust, error) {
	cfg := Default()
	var userFile string
	if home, err := os.UserHomeDir(); err == nil {
		userFile = filepath.Join(home, ".abhed", "config.json")
		if err := mergeFile(&cfg, userFile); err != nil {
			return WorkspaceTrust{Workspace: canonical(workspace)}, err
		}
	}
	st, err := mergeWorkspace(&cfg, workspace, userFile, LoadOptions{Trust: TrustRefused})
	if err != nil {
		return st, err
	}
	if st.AgentsReason != "home" {
		st.AgentsTrusted, st.AgentsReason = decideAgents(st, LoadOptions{})
	}
	if st.File == "" || st.Reason == "home" {
		noteUsers(&st, workspace, LoadOptions{})
		return st, nil
	}
	st.Trusted, st.Reason = decide(st, LoadOptions{})
	noteUsers(&st, workspace, LoadOptions{})
	// Under a managed lock on allow rules, the workspace's would be dropped
	// even once trusted, so the prompt must not show them as gained.
	locked := Default()
	if mergeManaged(&locked) == nil && locked.AllowLocked() && !locked.ManagedSets("permissions.allow") {
		for i, k := range st.Ignored {
			if k.Key == "permissions.allow" && k.Reason == "" {
				st.Ignored[i].Reason = "the managed configuration locks allow rules, so trust would not add these"
			}
		}
	}
	return st, nil
}

// fieldRule classifies one setting of a workspace file. apply is nil for a
// setting that is always ignored when untrusted; otherwise it applies the
// value when it tightens and reports whether it did.
type fieldRule struct {
	apply func(dst, ws *Config) bool
	why   string
}

// ruleFor finds the rule for a dotted path, or for the nearest section above it.
// A setting no rule names is ignored.
func ruleFor(key string) fieldRule {
	for k := key; k != ""; {
		if r, ok := workspaceRules[k]; ok {
			return r
		}
		i := strings.LastIndex(k, ".")
		if i < 0 {
			break
		}
		k = k[:i]
	}
	return fieldRule{why: "not classified"}
}

// workspaceRules is the classification in workspace-trust.md. Every setting
// must resolve to an entry here; TestEveryConfigFieldIsClassified holds that.
var workspaceRules = map[string]fieldRule{
	"permissions.deny":           {union(func(c *Config) *[]string { return &c.Permissions.Deny }), "added to the deny rules"},
	"permissions.ask":            {union(func(c *Config) *[]string { return &c.Permissions.Ask }), "added to the ask rules"},
	"permissions.mode":           {narrowMode, "only plan or default, and only narrower than the current mode"},
	"permissions.allow":          {nil, "an allow rule widens what runs unasked"},
	"permissions.git_extensions": {nil, "a git extension opted in runs unasked"},

	"sandbox.min_tier":              {raiseTier, "only a stronger tier"},
	"sandbox.allow_network":         {onlyFalse(func(c *Config) *bool { return &c.Sandbox.AllowNetwork }), "only false"},
	"sandbox.max_memory_mb":         {lower(func(c *Config) *int { return &c.Sandbox.MaxMemoryMB }, zeroIs(4096)), "only lower"},
	"sandbox.max_procs":             {lower(func(c *Config) *int { return &c.Sandbox.MaxProcs }, zeroIs(512)), "only lower"},
	"sandbox.terminal":              {onlyLines, "only lines"},
	"sandbox.terminal_idle_minutes": {lower(func(c *Config) *int { return &c.Sandbox.TerminalIdleMinutes }, zeroIs(30)), "only lower"},
	"sandbox.read_only_paths":       {nil, "mounts more of the host into the sandbox"},
	"sandbox.tier":                  {nil, "chooses what confines commands, in place of the strongest available"},
	"fence":                         {nil, "tunes the fence tier's limits"},
	"sandbox.network":               {nil, "managed only: turns on the egress allowlist"},
	"egress":                        {nil, "managed only: names the destinations commands reach"},

	"limits.max_turns":                {lower(func(c *Config) *int { return &c.Limits.MaxTurns }, zeroIsZero), "only lower"},
	"limits.max_tokens":               {lower(func(c *Config) *int { return &c.Limits.MaxTokens }, zeroUnlimited), "only lower"},
	"limits.max_budget_tokens":        {lower(func(c *Config) *int { return &c.Limits.MaxBudgetTokens }, zeroUnlimited), "only lower"},
	"limits.max_subagents":            {lower(func(c *Config) *int { return &c.Limits.MaxSubagents }, zeroUnlimited), "only lower"},
	"limits.max_parallel_subagents":   {lower(func(c *Config) *int { return &c.Limits.MaxParallelSubagents }, zeroIs(8)), "only lower"},
	"limits.nested_subagents":         {onlyFalse(func(c *Config) *bool { return &c.Limits.NestedSubagents }), "only false"},
	"limits.max_background_subagents": {lowerOrZero(func(c *Config) *int { return &c.Limits.MaxBackgroundSubagents }), "only lower; zero allows none"},
	"limits.background_shells":        {lowerOrZero(func(c *Config) *int { return &c.Limits.BackgroundShells }), "only lower; zero allows none"},
	"limits.background_max_minutes":   {lower(func(c *Config) *int { return &c.Limits.BackgroundMaxMinutes }, zeroIs(60)), "only lower"},
	"subagents.wake":                  {tighterWake, "only tighter: off < notify < auto"},
	"subagents.max_wakes_per_hour":    {lowerOrZero(func(c *Config) *int { return &c.Subagents.MaxWakesPerHour }), "only lower; zero never wakes"},
	"subagents.wake_max_turns":        {lower(func(c *Config) *int { return &c.Subagents.WakeMaxTurns }, zeroIs(8)), "only lower"},

	"tools.syntax_check": {stricterSyntax, "only stricter"},

	"web_search.enabled":     {onlyFalse(func(c *Config) *bool { return &c.WebSearch.Enabled }), "only false"},
	"web_search.max_results": {lower(func(c *Config) *int { return &c.WebSearch.MaxResults }, zeroIs(5)), "only lower"},
	"web_search":             {nil, "a search provider, key and endpoint receive the agent's queries"},
	"web_fetch.enabled":      {onlyFalse(func(c *Config) *bool { return &c.WebFetch.Enabled }), "only false"},
	"web_fetch.max_chars":    {lower(func(c *Config) *int { return &c.WebFetch.MaxChars }, zeroIs(20000)), "only lower"},
	"web_fetch":              {nil, "names the hosts the agent may fetch from"},
	"k8s.enabled":            {onlyFalse(func(c *Config) *bool { return &c.K8s.Enabled }), "only false"},
	"k8s.allow_writes":       {onlyFalse(func(c *Config) *bool { return &c.K8s.AllowWrites }), "only false"},
	"k8s":                    {nil, "names which cluster and credentials the agent reaches"},
	"ssh.enabled":            {onlyFalse(func(c *Config) *bool { return &c.SSH.Enabled }), "only false"},
	"ssh.hosts":              {nil, "names machines and keys the agent reaches"},
	"ssh.connect_hosts":      {nil, "names machines ssh_connect may reach"},
	"skills.disabled":        {onlyTrue(func(c *Config) *bool { return &c.Skills.Disabled }), "only true"},
	"skills.dirs":            {nil, "a skill is instructions to the agent"},
	"agents.disabled":        {onlyTrue(func(c *Config) *bool { return &c.Agents.Disabled }), "only true"},
	"agents.dirs":            {nil, "a definition is instructions and a model choice"},
	"telemetry":              {nil, "sends the event stream to an endpoint; turning it off removes an audit feed"},

	"commands.dirs":       {nil, "a command is instructions to the agent"},
	"rules.dirs":          {nil, "a rule is instructions to the agent"},
	"statusline":          {nil, "a statusline command is a process"},
	"memory.auto":         {onlyFalse(func(c *Config) *bool { return &c.Memory.Auto }), "only false"},
	"memory.import_depth": {lower(func(c *Config) *int { return &c.Memory.ImportDepth }, zeroIs(defaultImportDepth)), "only lower"},
	"cli":                 {nil, "only the managed configuration sets it"},
	"record":              {nil, "only the managed configuration sets it"},
	"hooks":               {nil, "only the managed configuration sets it"},
	"studio":              {nil, "only the managed configuration sets it"},
	"suggest.enabled":     {onlyFalse(func(c *Config) *bool { return &c.Suggest.Enabled }), "only false"},
	"suggest.model":       {nil, "a provider receives the conversation"},

	"additional_dirs":  {nil, "widens the directories the agent may reach"},
	"model":            {nil, "a provider and its base_url receive the code"},
	"custom_providers": {nil, "a provider and its base_url receive the code"},
	"context":          {nil, "memory_files are read into the prompt; the rest waits for trust"},
	"rag":              {nil, "a corpus URL and headers are egress"},
	"mcp":              {nil, "an MCP server is a process or endpoint"},
	"retrieval":        {nil, "embed_base_url receives the code"},
	"extensions":       {nil, "an extension is a process"},
	"storage":          {nil, "where the record goes and with which credentials"},
	"server":           {nil, "deployment settings for serve"},
	"schedules":        {nil, "prompts the server runs on its own"},
	"auth":             {nil, "who may sign in"},
}

func union(field func(*Config) *[]string) func(dst, ws *Config) bool {
	return func(dst, ws *Config) bool {
		d := field(dst)
		out := append([]string{}, *d...)
		for _, r := range *field(ws) {
			if !containsString(out, r) {
				out = append(out, r)
			}
		}
		*d = out
		return true
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// zeroMeans is what a zero or negative value of a limit stands for.
type zeroMeans struct {
	unlimited bool
	value     int
}

var (
	zeroUnlimited = zeroMeans{unlimited: true}
	zeroIsZero    = zeroMeans{}
)

func zeroIs(v int) zeroMeans { return zeroMeans{value: v} }

// lower takes a positive value no higher than the current one, compared with
// what the current value means in effect: its default when zero.
func lower(field func(*Config) *int, zero zeroMeans) func(dst, ws *Config) bool {
	return func(dst, ws *Config) bool {
		d, v := field(dst), *field(ws)
		if v <= 0 {
			return false
		}
		cur := *d
		if cur <= 0 {
			if !zero.unlimited && v > zero.value {
				return false
			}
		} else if v > cur {
			return false
		}
		*d = v
		return true
	}
}

// lowerOrZero takes a value no higher than the current one, zero included:
// for these limits zero is the tightest setting, not "unset".
func lowerOrZero(field func(*Config) *int) func(dst, ws *Config) bool {
	return func(dst, ws *Config) bool {
		d, v := field(dst), *field(ws)
		if v < 0 || v > *d {
			return false
		}
		*d = v
		return true
	}
}

var wakeRank = map[string]int{"off": 0, "notify": 1, "auto": 2}

func tighterWake(dst, ws *Config) bool {
	v, ok := wakeRank[ws.Subagents.Wake]
	cur := dst.Subagents.Wake
	if cur == "" {
		cur = "auto"
	}
	if !ok || v > wakeRank[cur] {
		return false
	}
	dst.Subagents.Wake = ws.Subagents.Wake
	return true
}

func onlyFalse(field func(*Config) *bool) func(dst, ws *Config) bool {
	return func(dst, ws *Config) bool {
		if *field(ws) {
			return false
		}
		*field(dst) = false
		return true
	}
}

func onlyTrue(field func(*Config) *bool) func(dst, ws *Config) bool {
	return func(dst, ws *Config) bool {
		if !*field(ws) {
			return false
		}
		*field(dst) = true
		return true
	}
}

// modeRank orders the modes from narrowest to widest.
var modeRank = map[string]int{"plan": 0, "default": 1, "accept-edits": 2, "auto": 3, "bypass": 4}

func narrowMode(dst, ws *Config) bool {
	m := ws.Permissions.Mode
	cur, ok := modeRank[orDefaultMode(dst.Permissions.Mode)]
	if (m != "plan" && m != "default") || !ok || modeRank[m] > cur {
		return false
	}
	dst.Permissions.Mode = m
	return true
}

func orDefaultMode(m string) string {
	if m == "" {
		return "default"
	}
	return m
}

var tierRank = map[string]int{"none": 0, "process": 1, "container": 2, "vm": 3}

func raiseTier(dst, ws *Config) bool {
	v, ok := tierRank[ws.Sandbox.MinTier]
	cur := dst.Sandbox.MinTier
	if cur == "" {
		cur = "process"
	}
	if !ok || v < tierRank[cur] {
		return false
	}
	dst.Sandbox.MinTier = ws.Sandbox.MinTier
	return true
}

func onlyLines(dst, ws *Config) bool {
	if ws.Sandbox.Terminal != "lines" {
		return false
	}
	dst.Sandbox.Terminal = "lines"
	return true
}

func stricterSyntax(dst, ws *Config) bool {
	v := syntaxRank(ws.Tools.SyntaxCheck)
	if ws.Tools.SyntaxCheck == "" || v < 0 || v < syntaxRank(dst.Tools.SyntaxCheck) {
		return false
	}
	dst.Tools.SyntaxCheck = ws.Tools.SyntaxCheck
	return true
}
