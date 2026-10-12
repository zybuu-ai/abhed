// Package config loads Abhed's layered configuration.
//
// Precedence, lowest to highest: built-in defaults, user config, project
// config, environment, flags — except managed config (/etc/abhed), which
// always wins so local settings cannot escalate past org policy (docs P7).
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/nlink"
	"github.com/zybuu-ai/abhed/internal/policy"
)

type Config struct {
	// AdditionalDirs are directories the agent may reach beyond the workspace
	// root it was started in. Set by the operator — from config or --add-dir —
	// and never by the model: see tools.Session.Roots for why the boundary
	// itself is not negotiable.
	AdditionalDirs []string `json:"additional_dirs,omitempty"`

	Model       ModelConfig       `json:"model"`
	Permissions PermissionsConfig `json:"permissions"`
	Context     ContextConfig     `json:"context"`
	RAG         RAGConfig         `json:"rag,omitempty"`
	Skills      SkillsConfig      `json:"skills,omitempty"`
	Agents      AgentsConfig      `json:"agents,omitempty"`
	Subagents   SubagentsConfig   `json:"subagents"`
	K8s         K8sConfig         `json:"k8s,omitempty"`
	SSH         SSHConfig         `json:"ssh,omitempty"`
	Limits      LimitsConfig      `json:"limits"`
	Sandbox     SandboxConfig     `json:"sandbox"`
	MCP         MCPConfig         `json:"mcp"`
	Retrieval   RetrievalConfig   `json:"retrieval"`
	WebSearch   WebSearchConfig   `json:"web_search"`
	WebFetch    WebFetchConfig    `json:"web_fetch,omitempty"`
	Extensions  []ExtensionConfig `json:"extensions,omitempty"`
	// CustomProviders adds model providers without a rebuild.
	CustomProviders []CustomProviderConfig `json:"custom_providers,omitempty"`
	Storage         StorageConfig          `json:"storage"`
	Server          ServerConfig           `json:"server,omitempty"`
	Telemetry       TelemetryConfig        `json:"telemetry,omitempty"`
	// Schedules are prompts run on a timetable by `abhed serve`. Each run is an
	// ordinary session — listed, recorded, replayable — that the clock started.
	Schedules []ScheduleConfig `json:"schedules,omitempty"`
	Auth      AuthConfig       `json:"auth"`
	Tools     ToolsConfig      `json:"tools,omitempty"`

	// CLI tunes the interactive command line.
	CLI CLIConfig `json:"cli,omitempty"`
	// Commands are directories of custom slash commands.
	Commands CommandsConfig `json:"commands,omitempty"`
	// Rules are directories of path-scoped instructions.
	Rules RulesConfig `json:"rules,omitempty"`
	// Statusline is a command that draws the status line.
	Statusline StatuslineConfig `json:"statusline,omitempty"`
	// Memory governs what the agent remembers between sessions.
	Memory MemoryConfig `json:"memory,omitempty"`
	// Record is where the local record is kept and for how long.
	Record RecordConfig `json:"record,omitempty"`
	// Hooks governs the extension hooks.
	Hooks HooksConfig `json:"hooks,omitempty"`
	// Studio governs what Abhed Studio may offer beside the agent.
	Studio StudioConfig `json:"studio,omitempty"`
	// Suggest governs the next-prompt suggestion shown after a turn.
	Suggest SuggestConfig `json:"suggest,omitempty"`
	// Fence tunes the fence tier (sandbox.tier "fence"), a Linux preview.
	Fence FenceConfig `json:"fence,omitempty"`
	// Egress is the rules of the egress proxy that sandbox.network
	// "allowlist" sends commands through. Managed only.
	Egress egress.Config `json:"egress,omitempty"`
	// ControlPlane serves /v1/sandboxes from `abhed serve`. Off by default.
	ControlPlane ControlPlaneConfig `json:"control_plane,omitempty"`

	// Managed is set when the config came from the org-managed path.
	Managed bool `json:"-"`
	// ManagedKeys are the settings the managed file made, as sorted dotted
	// paths such as permissions.mode. See ManagedSets and Apply.
	ManagedKeys []string `json:"-"`
	// Unknown lists the keys in the files that no setting reads.
	Unknown []UnknownKey `json:"-"`
	// SetKeys are the settings any file made, as dotted paths; see Sets.
	SetKeys []string `json:"-"`
	// Workspace is what loading decided about the workspace's own file.
	Workspace WorkspaceTrust `json:"-"`
	// SetAside are settings a file made that its layer may not make, such
	// as a managed-only key in the user's file; each was left out.
	SetAside []SetAsideKey `json:"-"`
	// Settings is the -settings file this run merged, if any.
	Settings SettingsSource `json:"-"`
	// Narrowed are the web settings a lower layer made that took effect
	// over the managed file because they only narrow it.
	Narrowed []SetAsideKey `json:"-"`
	// Overridden are the settings a lower layer made that the managed file
	// replaced with another value.
	Overridden []SetAsideKey `json:"-"`
	// userNet is the person's own network settings, held until the managed file.
	userNet *userNetwork
	// webIntents and origins are what loading holds until the managed file.
	webIntents []webIntent
	origins    map[string]origin
	// ruleLayers names the layer each permission rule came from; see RuleLayer.
	ruleLayers map[string]string
	// managedRules are the rules the managed file itself lists, by list and
	// rule as ruleLayers keys them.
	managedRules map[string]bool
}

// SettingsSource names a -settings file and the hash of what was merged.
type SettingsSource struct {
	Name   string
	SHA256 string
}

// CLIConfig tunes the interactive command line.
type CLIConfig struct {
	// ModeCycle is the modes Shift-Tab steps through, from default,
	// accept-edits and plan. Managed only, and it can only remove modes:
	// auto and bypass are never in the cycle.
	ModeCycle []string `json:"mode_cycle,omitempty"`
	// Title sets the terminal's title to the session's state (ready,
	// working, approval needed). On by default.
	Title bool `json:"title"`
	// Notify is how an unfocused terminal is told an approval waits or a
	// turn ended: auto (the default), bel, osc9 or off.
	Notify string `json:"notify,omitempty"`
	// Copy lets /copy put the last reply on the clipboard through the
	// terminal (OSC 52). On by default.
	Copy bool `json:"copy"`
}

// CommandsConfig lists directories of custom slash commands.
type CommandsConfig struct {
	Dirs []string `json:"dirs,omitempty"`
}

// RulesConfig lists directories of path-scoped rule files.
type RulesConfig struct {
	Dirs []string `json:"dirs,omitempty"`
}

// StatuslineConfig is a command run to draw the status line. It gets the
// status as JSON on stdin.
type StatuslineConfig struct {
	Command string `json:"command,omitempty"`
}

// MemoryConfig governs what the agent remembers between sessions.
type MemoryConfig struct {
	// Auto lets the agent write its own memory files. Off unless turned on:
	// a memory the agent writes is a way for injected text to persist. A
	// workspace may only turn it off, and a managed value binds.
	Auto bool `json:"auto,omitempty"`
	// ImportDepth bounds how deep @imports in memory files are followed.
	// Zero means the default, 5; at most 10.
	ImportDepth int `json:"import_depth,omitempty"`
}

// RecordConfig is where the local record lives and how long it is kept.
// Both are managed only: the record is the audit trail.
type RecordConfig struct {
	// Dir replaces ~/.abhed/records.
	Dir string `json:"dir,omitempty"`
	// RetentionDays, when positive, is how long a session is kept before a
	// prune may remove it. Zero keeps the record until it is pruned by hand.
	RetentionDays int `json:"retention_days,omitempty"`
	// Also are records an embedder hands in beside Dir, never read from a
	// file; they are state as Dir is.
	Also []string `json:"-"`
}

// SuggestConfig governs the next-prompt suggestion an interactive surface
// shows after a completed turn. Headless runs never make one.
// FenceConfig tunes the fence tier; memory and processes come from the
// sandbox's max_memory_mb and max_procs.
type FenceConfig struct {
	// CPUPercent bounds a session's commands' CPU time, in percent of one
	// core: 50 is half of one, 200 two whole ones. Zero leaves it unbounded.
	CPUPercent int `json:"cpu_percent,omitempty"`
}

type SuggestConfig struct {
	// Enabled is on by default; a workspace may only turn it off, and a
	// managed false binds.
	Enabled bool `json:"enabled"`
	// Model names a configured provider to ask instead of the session's own.
	Model string `json:"model,omitempty"`
}

// StudioConfig is what the organisation allows Abhed Studio beside the agent.
type StudioConfig struct {
	// DisableHostTerminal removes Studio's own host shell, which is neither
	// sandboxed nor recorded. Managed only.
	DisableHostTerminal bool `json:"disable_host_terminal,omitempty"`
}

// HooksConfig governs the extension hooks.
type HooksConfig struct {
	// Disabled switches every hook off. Managed only; only true means anything.
	Disabled bool `json:"disabled,omitempty"`
	// ManagedOnly sends hook events only to the managed file's extensions;
	// every other keeps only the tools it provides. Managed only.
	ManagedOnly bool `json:"managed_only,omitempty"`
}

// SetAsideKey is a setting a file made that was left out, and why.
type SetAsideKey struct {
	File string
	// Layer is the layer the file was read as (user, settings or workspace),
	// where it is known: the user's file and a home-folder workspace's are
	// the same path. For env, File is the variable's name.
	Layer string
	Key   string
	// Value is the entry left out of a list, such as one allow rule; empty
	// when the whole setting was.
	Value  string
	Reason string
}

func (k SetAsideKey) String() string {
	if k.Value != "" {
		return fmt.Sprintf("%s sets %s %s, which is ignored: %s", Printable(k.File), k.Key, Printable(k.Value), k.Reason)
	}
	return fmt.Sprintf("%s sets %s, which is ignored: %s", Printable(k.File), k.Key, k.Reason)
}

// MaxSessionHours bounds auth.session_hours at a year.
const MaxSessionHours = 8760

// NetworkAllowlist is the sandbox.network value that sends commands through
// the egress proxy.
const NetworkAllowlist = "allowlist"

// managedOnly are the settings only the managed configuration may make. The
// same key in the user's file or a trusted workspace's is set aside.
var managedOnly = map[string]string{
	"cli.mode_cycle":               "only the managed configuration narrows the Shift-Tab modes",
	"record.dir":                   "only the managed configuration moves the record",
	"record.retention_days":        "only the managed configuration sets how long the record is kept",
	"hooks.disabled":               "only the managed configuration switches hooks off, since that removes their vetoes",
	"hooks.managed_only":           "only the managed configuration limits hooks to its own extensions",
	"studio.disable_host_terminal": "only the managed configuration removes Studio's host terminal",
	"sandbox.network":              "only the managed configuration, or your own file narrowing allow_network, turns on the egress allowlist",
	"sandbox.microvm_socket":       "only the managed configuration names the microVM driver",
	"egress.rules":                 "only the managed configuration sets which destinations commands reach",
	"egress.default":               "only the managed configuration sets which destinations commands reach",
	"egress.mode":                  "only the managed configuration sets the egress mode",
	"egress.record_paths":          "only the managed configuration sets what the egress record keeps",
	"egress.idle_seconds":          "only the managed configuration sets how long an idle egress connection lasts",
	"control_plane.kubernetes":     "only the managed configuration configures the Kubernetes driver",
	"control_plane.interceptors":   "only the managed configuration sets what admits sandbox calls",
}

// containerManagedOnly are the container drivers' settings that reach more
// of the host or loosen a bound, under each engine's section.
var containerManagedOnly = map[string]string{
	"binary":          "only the managed configuration names the engine's program",
	"images":          "only the managed configuration adds images a sandbox may run",
	"mounts":          "only the managed configuration mounts host folders into sandboxes",
	"mount_roots":     "only the managed configuration lets sandboxes mount host folders",
	"gpus":            "only the managed configuration gives sandboxes devices",
	"cpus":            "only the managed configuration sets the CPU bound",
	"seccomp_profile": "only the managed configuration replaces the seccomp profile",
}

func init() {
	for _, engine := range []string{"docker", "podman"} {
		for k, why := range containerManagedOnly {
			managedOnly["control_plane."+engine+"."+k] = why
		}
	}
}

// ManagedOnly reports whether only the managed configuration may make the
// setting at path.
func ManagedOnly(path string) bool {
	_, ok := managedOnly[path]
	return ok
}

// clearManagedOnly resets a managed-only setting to its default.
func clearManagedOnly(c *Config, key string) {
	switch key {
	case "cli.mode_cycle":
		c.CLI.ModeCycle = nil
	case "record.dir":
		c.Record.Dir = ""
	case "record.retention_days":
		c.Record.RetentionDays = 0
	case "hooks.disabled":
		c.Hooks.Disabled = false
	case "hooks.managed_only":
		c.Hooks.ManagedOnly = false
	case "studio.disable_host_terminal":
		c.Studio.DisableHostTerminal = false
	case "sandbox.network":
		c.Sandbox.Network = ""
	case "sandbox.microvm_socket":
		c.Sandbox.MicroVMSocket = ""
	case "egress.rules":
		c.Egress.Rules = nil
	case "egress.default":
		c.Egress.Default = ""
	case "egress.mode":
		c.Egress.Mode = ""
	case "egress.record_paths":
		c.Egress.RecordPaths = nil
	case "egress.idle_seconds":
		c.Egress.IdleSeconds = 0
	case "control_plane.kubernetes":
		c.ControlPlane.Kubernetes = nil
	case "control_plane.interceptors":
		c.ControlPlane.Interceptors = nil
	}
	for engine, cc := range map[string]*ContainerDriverConfig{"docker": &c.ControlPlane.Docker, "podman": &c.ControlPlane.Podman} {
		switch key {
		case "control_plane." + engine + ".binary":
			cc.Binary = ""
		case "control_plane." + engine + ".images":
			cc.Images = nil
		case "control_plane." + engine + ".mounts":
			cc.Mounts = nil
		case "control_plane." + engine + ".mount_roots":
			cc.MountRoots = nil
		case "control_plane." + engine + ".gpus":
			cc.GPUs = nil
		case "control_plane." + engine + ".cpus":
			cc.CPUs = 0
		case "control_plane." + engine + ".seccomp_profile":
			cc.SeccompProfile = ""
		}
	}
}

// setAside leaves out the managed-only settings the files merged so far
// made, crediting them to file. It runs before the managed file is merged,
// so every managed-only value present came from a lower layer.
func setAside(c *Config, file, layer string, data []byte) {
	setAsideWeb(c, file, layer)
	kept := c.SetKeys[:0:0]
	for _, k := range c.SetKeys {
		if why, ok := managedOnly[k]; ok && layer == LayerUser && (k == "sandbox.network" || strings.HasPrefix(k, "egress.")) {
			// Held until the managed file is read: the person's own file may
			// narrow allow_network to an allowlist of their own.
			if c.userNet == nil {
				c.userNet = &userNetwork{network: c.Sandbox.Network, egress: c.Egress}
			}
			c.userNet.keys = append(c.userNet.keys, SetAsideKey{File: file, Layer: layer, Key: k, Value: valueIn(data, k), Reason: why})
			clearManagedOnly(c, k)
			continue
		}
		if why, ok := managedOnly[k]; ok {
			clearManagedOnly(c, k)
			c.SetAside = append(c.SetAside, SetAsideKey{File: file, Layer: layer, Key: k, Value: valueIn(data, k), Reason: why})
			continue
		}
		kept = append(kept, k)
	}
	c.SetKeys = kept
}

// userNetwork is the person's own sandbox.network and egress settings, held
// until the managed file is read.
type userNetwork struct {
	network string
	egress  egress.Config
	keys    []SetAsideKey
}

// userAllowlistNarrows applies the person's own allowlist and egress rules
// where they only narrow: allow_network is true and the managed file sets
// none of allow_network, sandbox.network or egress. Otherwise they are set
// aside, so they never open a host that no network, or the managed rules,
// would have kept closed.
func userAllowlistNarrows(c *Config) {
	u := c.userNet
	c.userNet = nil
	if u == nil {
		return
	}
	managedNet := c.ManagedSets("sandbox.allow_network") || c.ManagedSets("sandbox.network") || c.ManagedSets("egress")
	if u.network == NetworkAllowlist && c.Sandbox.AllowNetwork && !managedNet {
		c.Sandbox.Network, c.Egress = NetworkAllowlist, u.egress
		for _, k := range u.keys {
			c.SetKeys = append(c.SetKeys, k.Key)
		}
		return
	}
	for _, k := range u.keys {
		switch {
		case managedNet:
		case u.network != NetworkAllowlist:
			k.Reason = "your own egress settings apply only with sandbox.network \"allowlist\" in the same file"
		default:
			k.Reason = "your own allowlist applies only over sandbox.allow_network true, which it narrows; without it commands already reach nothing"
		}
		c.SetAside = append(c.SetAside, k)
	}
}

// valueIn is the value data gives the setting at key, as a record shows it,
// or "" when data does not set it.
func valueIn(data []byte, key string) string {
	var raw any
	if json.Unmarshal(data, &raw) != nil {
		return ""
	}
	out := ""
	walkSet("", raw, reflect.TypeFor[Config](), func(p string, v any) {
		if p == key {
			out = RedactValue(p, v)
		}
	})
	return out
}

// warnSetAside warns of each set-aside setting.
func warnSetAside(w warnSink, keys []SetAsideKey) {
	for _, k := range keys {
		w.emit("aside\x00"+k.File+"\x00"+k.Key+"\x00"+k.Value, k.String())
	}
}

// DefaultModeCycle is the Shift-Tab cycle when nothing narrows it. Auto and
// bypass are never in it: they are chosen on purpose, not by a stray key.
var DefaultModeCycle = []string{"default", "accept-edits", "plan"}

// ModeCycle is the modes Shift-Tab offers, in order: the default cycle, less
// any the managed cli.mode_cycle leaves out.
func (c Config) ModeCycle() []string {
	if len(c.CLI.ModeCycle) == 0 {
		return slices.Clone(DefaultModeCycle)
	}
	var out []string
	for _, m := range DefaultModeCycle {
		if slices.Contains(c.CLI.ModeCycle, m) {
			out = append(out, m)
		}
	}
	return out
}

// MemoryImportDepth is how deep memory imports are followed.
func (c Config) MemoryImportDepth() int {
	if c.Memory.ImportDepth <= 0 {
		return defaultImportDepth
	}
	return c.Memory.ImportDepth
}

const (
	defaultImportDepth = 5
	// maxImportDepth bounds memory imports: each level is files read into
	// every prompt, and a chain deeper than this is a mistake or an attack.
	maxImportDepth = 10
)

type ModelConfig struct {
	Default   string                    `json:"default"`
	Providers map[string]ProviderConfig `json:"providers"`
	// Fallback names configured providers, in order, to move to when the
	// default is unreachable or refuses access. The move is recorded.
	Fallback []string `json:"fallback,omitempty"`
}

type ProviderConfig struct {
	// Type names a registered provider — run `abhed providers` for the list.
	// "openai-compatible" remains the generic escape hatch for any endpoint
	// speaking that API.
	Type            string   `json:"type"`
	BaseURL         string   `json:"base_url"`
	Model           string   `json:"model"`
	APIKey          string   `json:"api_key,omitempty"`
	APIKeyEnv       string   `json:"api_key_env,omitempty"`
	ContextWindow   int      `json:"context_window"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	ToolCallFormat  string   `json:"tool_call_format,omitempty"`
	ReasoningTags   []string `json:"reasoning_tags,omitempty"`
	// Think turns a hybrid-reasoning model's thinking phase on or off.
	// Omit to leave the server's default alone. Ollama reads this; an
	// OpenAI-style server uses reasoning_effort instead.
	Think *bool `json:"think,omitempty"`

	// watsonx only. A deployment is scoped by EITHER a project or a space;
	// sending both is rejected by the API.
	ProjectID string `json:"project_id,omitempty"`
	SpaceID   string `json:"space_id,omitempty"`
	// APIVersion is watsonx's date-versioned API. Defaults to 2024-05-01.
	APIVersion string `json:"api_version,omitempty"`
	// IAMURL overrides the token endpoint, for CPD which mints its own.
	IAMURL string `json:"iam_url,omitempty"`

	// Region and Project scope a cloud-hosted deployment (Bedrock, Vertex).
	Region string `json:"region,omitempty"`

	// Params holds the sampling and decoding controls for this provider.
	// Every field is optional; an omitted one leaves the model's own default
	// alone rather than substituting a number Abhed invented.
	Params ParamsConfig `json:"params,omitempty"`

	// CallTimeoutSeconds bounds one request to the model, from sending it to
	// the last byte; StallTimeoutSeconds bounds a wait with no byte. 0 keeps
	// the defaults, 600 and 300.
	CallTimeoutSeconds  int `json:"call_timeout_seconds,omitempty"`
	StallTimeoutSeconds int `json:"stall_timeout_seconds,omitempty"`

	// Extra passes provider-specific settings through without this struct
	// growing a field per vendor.
	Extra map[string]string `json:"extra,omitempty"`
}

// ParamsConfig mirrors model.Params in config form. Pointers throughout, so
// "unset" stays distinguishable from "set to zero" — the difference between
// leaving temperature alone and pinning it to greedy decoding.
type ParamsConfig struct {
	Temperature       *float64 `json:"temperature,omitempty"`
	TopP              *float64 `json:"top_p,omitempty"`
	TopK              *int     `json:"top_k,omitempty"`
	MinP              *float64 `json:"min_p,omitempty"`
	RepetitionPenalty *float64 `json:"repetition_penalty,omitempty"`
	FrequencyPenalty  *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty   *float64 `json:"presence_penalty,omitempty"`
	Seed              *int64   `json:"seed,omitempty"`
	MaxTokens         int      `json:"max_tokens,omitempty"`
	Stop              []string `json:"stop,omitempty"`
	Effort            string   `json:"effort,omitempty"`
	Think             *bool    `json:"think,omitempty"`
	ThinkingBudget    *int     `json:"thinking_budget,omitempty"`
}

// ExtensionConfig describes one extension process.
//
// An extension may veto and never permit: it can block a call, force an
// approval prompt, or rewrite arguments and results, but cannot turn a denied
// action into an allowed one. That is what keeps deny rules absolute no matter
// what an operator drops into this list.
type ExtensionConfig struct {
	Name      string            `json:"name"`
	Command   string            `json:"command"`
	Args      []string          `json:"args,omitempty"`
	Events    []string          `json:"events,omitempty"`
	TimeoutMS int               `json:"timeout_ms,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	// Match narrows tool_call and permission_request to the calls a rule
	// matches, e.g. bash(git *).
	Match []string `json:"match,omitempty"`
	// Async sends the events that only observe without waiting.
	Async bool `json:"async,omitempty"`
}

// CustomProviderConfig adds a model provider from configuration.
type CustomProviderConfig struct {
	Name        string   `json:"name"`
	API         string   `json:"api"` // openai | anthropic | gemini
	BaseURL     string   `json:"base_url"`
	Description string   `json:"description,omitempty"`
	Sampling    []string `json:"sampling,omitempty"`
}

type PermissionsConfig struct {
	Mode  string   `json:"mode"`
	Deny  []string `json:"deny"`
	Ask   []string `json:"ask"`
	Allow []string `json:"allow"`
	// GitExtensions names git subcommands outside git itself, such as lfs,
	// that may run without the destructive step taking them as an alias or
	// extension it cannot read. Not from an untrusted workspace, and set
	// aside when the managed configuration sets the permissions without it.
	GitExtensions []string `json:"git_extensions,omitempty"`
}

type ContextConfig struct {
	CompactAt float64 `json:"compact_at"`
	// OffloadAt is the fraction of the context window at which old, large
	// tool results are replaced in the window by a stub; the full text stays
	// in the session record and the recall tool reads it back. It should sit
	// well below compact_at, so the free and lossless step runs first. Zero
	// turns it off; unset means 0.60.
	OffloadAt   *float64 `json:"offload_at,omitempty"`
	MemoryFiles []string `json:"memory_files"`
}

// AuthConfig controls how callers are identified. Default is "none", which is
// single-tenant local development. Production should use "oidc"; "proxy" is
// only safe when a trusted proxy is the sole route to the port.
type AuthConfig struct {
	// Mode is none | local | proxy | oidc.
	//   local — Abhed holds the accounts: email and password, no external IdP
	//   oidc  — delegate to an identity provider, including Google/Microsoft
	Mode string `json:"mode"`
	// Provider fills in issuer, scopes and tenant claim for a known IdP:
	// "google" for Gmail, "microsoft" for Outlook.
	Provider string `json:"provider,omitempty"`
	// AllowSignup lets anyone create a local account. Off by default: an
	// open signup on an internal tool is rarely what an operator intends.
	AllowSignup bool `json:"allow_signup,omitempty"`
	// DefaultTenant is assigned to accounts created without one.
	DefaultTenant string `json:"default_tenant,omitempty"`
	// UsersFile is where local accounts are kept when there is no database.
	// Empty means <workspace>/.abhed/users.json; a deployment sets it to a
	// path outside every workspace, such as its state directory.
	// ABHED_USERS_FILE overrides it, unless the managed file sets it.
	UsersFile   string `json:"users_file,omitempty"`
	Issuer      string `json:"issuer,omitempty"`
	Audience    string `json:"audience,omitempty"`
	JWKSURL     string `json:"jwks_url,omitempty"`
	TenantClaim string `json:"tenant_claim,omitempty"`
	GroupsClaim string `json:"groups_claim,omitempty"`
	// RequireGroup gates all access on membership, above tenancy, once the
	// caller is signed in; sign-in, sign-out and health stay reachable.
	RequireGroup string `json:"require_group,omitempty"`
	// AdminGroup gates the administrative routes — settings, users, invites —
	// rather than the whole server.
	//
	// Deliberately separate from RequireGroup, whose meaning is the opposite:
	// "everyone who may use this deployment at all". Reusing it would make
	// every ordinary user an administrator, which is how a settings page
	// becomes a privilege-escalation hole.
	AdminGroup string `json:"admin_group,omitempty"`

	// Browser sign-in. Without these, OIDC still validates bearer tokens for
	// API clients, but a person opening the console has no way to sign in.
	ClientID        string `json:"client_id,omitempty"`
	ClientSecret    string `json:"client_secret,omitempty"`
	ClientSecretEnv string `json:"client_secret_env,omitempty"`
	RedirectURL     string `json:"redirect_url,omitempty"`
	// PostLogoutURL is where the IdP returns after ending its own session.
	// Providers require it to be pre-registered, and ignore RP-initiated
	// logout without it — which leaves the user still signed in.
	PostLogoutURL string   `json:"post_logout_redirect_url,omitempty"`
	Scopes        []string `json:"scopes,omitempty"`
	// CookieSecure should be true anywhere but local HTTP development.
	CookieSecure bool `json:"cookie_secure,omitempty"`
	// SessionHours is how long a browser sign-in lasts, 0 to MaxSessionHours.
	SessionHours int `json:"session_hours,omitempty"`
	// ProxyLogoutURL is the authenticating proxy's own sign-out, in proxy
	// mode. Without it the console offers no sign-out, since the proxy owns it.
	ProxyLogoutURL string `json:"proxy_logout_url,omitempty"`
	// CLI is command-line sign-in (abhed login): against local accounts, or
	// through the identity provider in an edition that serves auth.mode oidc.
	CLI CLILoginConfig `json:"cli,omitempty"`
	// Workload is workload identity (SPIFFE) and token exchange, which the
	// Enterprise edition serves. The Community Edition refuses enabled.
	Workload WorkloadAuthConfig `json:"workload,omitempty"`
	// GitHub restricts GitHub sign-in, which only the paid editions provide.
	// The Community Edition validates these keys and otherwise ignores them.
	GitHub GitHubAuthConfig `json:"github,omitempty"`
}

// GitHubAuthConfig says which GitHub accounts may sign in.
type GitHubAuthConfig struct {
	// Orgs admits members of any of these organisations.
	Orgs []string `json:"orgs,omitempty"`
	// Teams admits members of any of these teams, written org/team-slug.
	Teams []string `json:"teams,omitempty"`
	// AllowAny admits every GitHub account; it cannot be combined with a
	// restriction, which would still apply.
	AllowAny bool `json:"allow_any,omitempty"`
}

// WebSearchConfig controls the agent's access to the public web.
//
// OFF by default: Abhed is built to run air-gapped, and this is the one tool
// that deliberately crosses the boundary. Enabling it is a decision an operator
// makes, not a default they inherit.
type WebSearchConfig struct {
	Enabled bool `json:"enabled"`
	// Provider: duckduckgo (free, no key, the default) | brave | tavily |
	// serper | searxng (self-hosted).
	Provider string `json:"provider,omitempty"`
	// APIKeyEnv names the environment variable holding the key, so a
	// credential never sits in a config file.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	APIKey    string `json:"api_key,omitempty"`
	// BaseURL points at a self-hosted instance or an egress broker.
	BaseURL    string `json:"base_url,omitempty"`
	MaxResults int    `json:"max_results,omitempty"`
}

// WebFetchConfig controls the web_fetch tool, which reads one page through
// Abhed rather than the sandboxed shell.
//
// OFF by default, and separate from web_search: turning search on does not
// open a way to send a request to any site, which this does.
type WebFetchConfig struct {
	Enabled bool `json:"enabled"`
	// AllowedHosts, when set, is every host that may be fetched:
	// "docs.python.org" or "*.github.com". Internal addresses are refused
	// whatever it says.
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
	// MaxChars caps the text returned per call; 0 means 20,000.
	MaxChars int `json:"max_chars,omitempty"`
}

// StorageConfig selects the event store. Memory is fine for a CLI session;
// audit and replay across restarts need Postgres (docs §10).
type StorageConfig struct {
	// Driver is "memory" or "postgres".
	Driver string `json:"driver"`
	// DSN may also come from ABHED_DATABASE_URL, so a deployment need not put
	// a credential in a config file.
	DSN string `json:"dsn,omitempty"`
	// Tenant scopes every row; row-level security enforces it.
	Tenant   string `json:"tenant,omitempty"`
	MaxConns int    `json:"max_conns,omitempty"`
	// MigrateDSN connects as the role that OWNS the tables. Only `abhed
	// migrate` uses it; the server never does. It may come from
	// ABHED_MIGRATE_DATABASE_URL instead, and is best kept off the host that
	// runs the server: whoever holds it can disable the append-only triggers.
	MigrateDSN string `json:"migrate_dsn,omitempty"`
	// SingleRole lets the server connect as the role that owns the tables and
	// apply the schema itself. Simpler, and weaker: that role can disable the
	// append-only triggers, so the audit record is protected against mistakes
	// and not against the server's own credentials. The server refuses such a
	// connection unless this is set.
	SingleRole bool `json:"single_role,omitempty"`
}

// ControlPlaneConfig turns on the sandbox control plane in `abhed serve` and
// sets the global values of the settings registry (controlplane.Settings).
// A value the managed file sets is a ceiling a sandbox may only tighten.
// Zero values mean the registry's defaults.
type ControlPlaneConfig struct {
	// Enabled serves /v1/sandboxes and runs the reconciler. Off leaves serve
	// exactly as it was.
	Enabled bool `json:"enabled,omitempty"`
	// WorkspaceRoot is where each sandbox gets a folder of its own. Empty
	// means a folder in the control plane's state folder, outside ~/.abhed
	// and the server's workspace.
	WorkspaceRoot string `json:"workspace_root,omitempty"`
	// SharedWorkspace gives every sandbox the server's workspace instead, as
	// sessions have; WorkspaceRoot must then be unset.
	SharedWorkspace bool `json:"shared_workspace,omitempty"`
	// SnapshotDir holds sandbox snapshots, outside every workspace. Empty
	// means a folder in the control plane's state folder.
	SnapshotDir string `json:"snapshot_dir,omitempty"`
	// Expose lets :expose publish a sandbox's port to whoever asked, on an
	// authenticated path. Off by default.
	Expose bool `json:"expose,omitempty"`
	// ExposeMaxTTLSeconds bounds an exposure's life. Zero means an hour.
	ExposeMaxTTLSeconds int `json:"expose_max_ttl_seconds,omitempty"`
	// SQLitePath is the database file sandboxes are kept in when storage is
	// not Postgres. Empty is ~/.abhed/controlplane/sandboxes.db. Must be
	// absolute, in a folder only the server's user can enter, outside any
	// workspace.
	SQLitePath string `json:"sqlite_path,omitempty"`
	// ExecTimeoutSeconds bounds one :exec.
	ExecTimeoutSeconds int `json:"exec_timeout_seconds,omitempty"`
	// ExecOutputBytes bounds what one :exec may write, stdout and stderr together.
	ExecOutputBytes int64 `json:"exec_output_bytes,omitempty"`
	// RestartPolicy is never, on-failure or always.
	RestartPolicy string `json:"restart_policy,omitempty"`
	// MaxRestarts is how often the policy restarts a sandbox before giving up.
	MaxRestarts *int `json:"max_restarts,omitempty"`
	// RestartBackoffSeconds is the first restart's delay; each next one doubles.
	RestartBackoffSeconds int `json:"restart_backoff_seconds,omitempty"`
	// MutationsPerMinute and ExecPerMinute limit each caller on the sandbox routes.
	MutationsPerMinute int `json:"mutations_per_minute,omitempty"`
	ExecPerMinute      int `json:"exec_per_minute,omitempty"`
	// ReconcileSeconds is how often every sandbox is checked against its driver.
	ReconcileSeconds int `json:"reconcile_seconds,omitempty"`
	// SSHHostKey is the :ssh relay's host key file, so replicas can share one
	// key from a secret. Empty means a key the server makes in the control
	// plane's state folder. Absolute, readable by its owner only, not a link,
	// outside every workspace; read, never made.
	SSHHostKey string `json:"ssh_host_key,omitempty"`
	// SSH bounds the sessions reached through the :ssh relay.
	SSH SSHSessionConfig `json:"ssh,omitempty"`
	// SandboxCPUs and SandboxMemoryMB size every sandbox that does not set
	// its own; zero leaves the size to the driver.
	SandboxCPUs     float64 `json:"sandbox_cpus,omitempty"`
	SandboxMemoryMB int     `json:"sandbox_memory_mb,omitempty"`
	// PerSession gives each served session a control-plane sandbox of its
	// own, made as Sessions says, in place of the one sandbox every session
	// shares. Needs Enabled. Off leaves sessions exactly as they were.
	PerSession bool `json:"per_session,omitempty"`
	// Sessions shapes the sandboxes PerSession makes.
	Sessions SessionSandboxConfig `json:"sessions,omitempty"`
	// Docker and Podman register the container drivers by those names.
	Docker ContainerDriverConfig `json:"docker,omitempty"`
	Podman ContainerDriverConfig `json:"podman,omitempty"`
	// HA runs several replicas of serve on one Postgres (Enterprise).
	HA HAConfig `json:"ha,omitempty"`
	// Interceptors admit every mutating sandbox call, in order (Enterprise).
	Interceptors []InterceptorConfig `json:"interceptors,omitempty"`
	// Kubernetes is the edition's Kubernetes driver options, kept as written
	// for the edition to decode. A JSON object; managed only.
	Kubernetes json.RawMessage `json:"kubernetes,omitempty"`
}

// SessionSandboxConfig is how control_plane.per_session gives sessions
// their sandboxes.
type SessionSandboxConfig struct {
	// Scope is "session", a sandbox per session (the default), or "user",
	// one per user that all of that user's sessions share.
	Scope string `json:"scope,omitempty"`
	// Template is the template each sandbox is made from; "session" when empty.
	Template string `json:"template,omitempty"`
	// Driver is the registered driver each runs under; the default when empty.
	Driver string `json:"driver,omitempty"`
	// Retention is what a session's sandbox does when the server stops:
	// "stop" (the default), "delete" or "keep". Deleting a session always
	// deletes a sandbox of its own.
	Retention string `json:"retention,omitempty"`
	// WarmPool keeps this many Ready sandboxes for new sessions to claim;
	// zero keeps none, at most 16.
	WarmPool int `json:"warm_pool,omitempty"`
	// ReadySeconds bounds how long a session waits for its sandbox; 120 when zero.
	ReadySeconds int `json:"ready_seconds,omitempty"`
}

// ContainerDriverConfig registers the docker or podman driver. Every key
// that lets a sandbox reach more of the host, or loosens a bound, is
// managed only.
type ContainerDriverConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Binary is the engine's CLI, found on PATH by the engine's name when
	// empty. Managed only.
	Binary string `json:"binary,omitempty"`
	// Image is the default image; ABHED_SANDBOX_IMAGE or the container
	// tier's when empty.
	Image string `json:"image,omitempty"`
	// Images are further images a sandbox may ask for. Managed only.
	Images []string `json:"images,omitempty"`
	// Mounts are host folders every sandbox gets. Managed only.
	Mounts []ContainerMount `json:"mounts,omitempty"`
	// MountRoots are where a sandbox's own mounts may come from. Managed only.
	MountRoots []ContainerMountRoot `json:"mount_roots,omitempty"`
	// GPUs are the CDI device names a sandbox may ask for. Managed only.
	GPUs []string `json:"gpus,omitempty"`
	// CPUs bounds each sandbox's CPU; 2 when zero. Managed only.
	CPUs float64 `json:"cpus,omitempty"`
	// SeccompProfile is a profile file in place of the engine's default;
	// "unconfined" is refused. Managed only.
	SeccompProfile string `json:"seccomp_profile,omitempty"`
	// NamePrefix starts every container's name, "abhed-sbx-" when empty;
	// letters, digits, '_', '.' and '-', starting with a letter or digit.
	NamePrefix string `json:"name_prefix,omitempty"`
}

// ContainerMount is a host folder every container sandbox sees.
type ContainerMount struct {
	Host     string `json:"host"`
	Target   string `json:"target"`
	Writable bool   `json:"rw,omitempty"`
	Relabel  bool   `json:"relabel,omitempty"`
}

// ContainerMountRoot is a host folder under which a sandbox may ask for mounts.
type ContainerMountRoot struct {
	Path     string `json:"path"`
	Writable bool   `json:"rw,omitempty"`
	Relabel  bool   `json:"relabel,omitempty"`
}

// SSHSessionConfig bounds an SSH connection to a sandbox. Its commands are
// bounded by these rather than by exec_timeout_seconds, so a long-lived
// server an editor starts over SSH keeps running.
type SSHSessionConfig struct {
	// IdleTimeoutSeconds closes a connection with no traffic either way for
	// this long. Zero means an hour.
	IdleTimeoutSeconds int `json:"idle_timeout_seconds,omitempty"`
	// MaxSessionSeconds closes a connection this long after it opened. Zero
	// means twelve hours.
	MaxSessionSeconds int `json:"max_session_seconds,omitempty"`
}

// HAConfig makes this server one of several replicas sharing a Postgres
// store: each sandbox is owned by one replica under a lease, and a call for
// it on another replica is relayed to the owner. Enterprise only.
type HAConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// NodeID names this replica; unique among the replicas, without '#'.
	NodeID string `json:"node_id,omitempty"`
	// AdvertiseURL is where the other replicas reach this one, https unless
	// AllowHTTPRelay is set.
	AdvertiseURL string `json:"advertise_url,omitempty"`
	// RelayKeyFile or RelayKeyEnv holds the key replicas sign relayed calls
	// with: at least 32 bytes, the same on every replica.
	RelayKeyFile string `json:"relay_key_file,omitempty"`
	RelayKeyEnv  string `json:"relay_key_env,omitempty"`
	// RelayCAFile, when set, is the CA bundle the replicas' certificates are
	// checked against, instead of the system's.
	RelayCAFile string `json:"relay_ca_file,omitempty"`
	// AllowHTTPRelay permits plain-HTTP advertise URLs, for a private network.
	AllowHTTPRelay bool `json:"allow_http_relay,omitempty"`
	// LeaseSeconds is how long a replica's hold on a sandbox lasts without
	// renewal; another replica takes it over after that.
	LeaseSeconds int `json:"lease_seconds,omitempty"`
}

// InterceptorConfig is one admission hook on the sandbox API. Enterprise only.
type InterceptorConfig struct {
	Name string `json:"name"`
	// Phase is validate (allow or deny), patch (may return an RFC 6902 patch
	// of labels and settings) or post_commit (told after the call).
	Phase string `json:"phase"`
	// Operations limits the hook to these calls; empty means all of them.
	Operations []string `json:"operations,omitempty"`
	// URL is an HTTP endpoint to POST each request to, or Command is a
	// program speaking JSON Lines on stdin and stdout. One of the two.
	URL     string   `json:"url,omitempty"`
	Command []string `json:"command,omitempty"`
	// TokenEnv names an environment variable sent as a bearer token to URL.
	TokenEnv string `json:"token_env,omitempty"`
	// TimeoutMS bounds one answer; 500 when unset.
	TimeoutMS int `json:"timeout_ms,omitempty"`
	// FailOpen lets a call through when this hook errs or times out, and
	// records that it did. A denial is always enforced.
	FailOpen bool `json:"fail_open,omitempty"`
}

// ServerConfig holds the settings that only matter once `abhed serve` is
// reachable from a network Abhed does not control.
//
// These are deliberately separate from AuthConfig: they describe the deployment
// (is TLS terminated in front of us, whose Origin do we trust, is there a proxy)
// rather than who the user is. A laptop deployment leaves all of them at their
// zero values and behaves exactly as before.
type ServerConfig struct {
	// HSTS emits Strict-Transport-Security. Only set this when TLS really is
	// terminated in front of the server: sent over plain HTTP it pins the
	// browser to a scheme that does not answer, and the pin outlives the
	// mistake.
	HSTS bool `json:"hsts,omitempty"`
	// AllowedOrigins are the origins permitted to make state-changing requests.
	// Empty means "the host the request arrived on", which is correct for a
	// single-domain deployment and wrong only if the console is served from
	// somewhere other than the API.
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
	// TrustProxy makes rate limiting read X-Forwarded-For. Only enable it when
	// a proxy you control is the sole route to the port: otherwise a client
	// sets its own limiter key and rotates it at will.
	TrustProxy bool `json:"trust_proxy,omitempty"`
	// HomeURL is linked from the console and sign-in page as the way back to
	// whoever runs this deployment. Empty renders no link at all, which is the
	// right default: an air-gapped install cannot follow one.
	HomeURL string `json:"home_url,omitempty"`
	// CanonicalHost, when set, is the one hostname this deployment answers
	// on: a request that arrives for any other host is redirected there with
	// a 301, path and query intact. It exists so an old hostname can keep
	// working after a rename without the deployment living at two names,
	// which would split cookies, bookmarks and audit records between them.
	// Empty means "answer on whatever host the request names", which is the
	// only workable default for a laptop or an air-gapped install.
	CanonicalHost string `json:"canonical_host,omitempty"`
	// DrainSeconds is how long a shutdown lets running turns finish before
	// ending them. Behind a load balancer this is what makes a rolling deploy
	// stop interrupting work: the node refuses new turns with a 503 and exits
	// once the ones in flight are done.
	//
	// Zero ends turns at once, which is right for a single node with nowhere
	// to drain to. Turns still running when the budget is spent are recorded
	// as a shutdown and can be resumed elsewhere.
	DrainSeconds int `json:"drain_seconds,omitempty"`
	// TLS makes `abhed serve` terminate TLS itself instead of behind a proxy.
	// Unset, the server speaks plain HTTP as it always has.
	TLS ServerTLSConfig `json:"tls,omitempty"`
}

// ServerTLSConfig is the TLS `abhed serve` terminates. Relative paths are
// resolved against the workspace, as auth.users_file is.
type ServerTLSConfig struct {
	CertFile string `json:"cert_file,omitempty"`
	KeyFile  string `json:"key_file,omitempty"`
	// AdditionalCertificates are served by SNI name and key type: an RSA
	// certificate beside an ECDSA one, or the next certificate during a
	// rotation. The first that suits the client wins; cert_file is first.
	AdditionalCertificates []CertificatePair `json:"additional_certificates,omitempty"`
	// MinVersion is "1.2" (the default) or "1.3".
	MinVersion string `json:"min_version,omitempty"`
	// ClientCAFile turns on client certificates, verified against this CA.
	ClientCAFile string `json:"client_ca_file,omitempty"`
	// ClientAuth is "require" (the default once client_ca_file is set), or
	// "optional" to let a browser without a certificate use another sign-in.
	ClientAuth string `json:"client_auth,omitempty"`
	// ClientCertAccounts signs a local account in from a verified client
	// certificate whose common name is its username. Needs auth.mode local.
	ClientCertAccounts bool `json:"client_cert_accounts,omitempty"`
	// ReloadSeconds is how often the files are checked for a change; 10 when unset.
	ReloadSeconds int `json:"reload_seconds,omitempty"`
}

// CertificatePair is one certificate chain file and its key file.
type CertificatePair struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

// Enabled reports whether any TLS setting is made, so serve terminates TLS
// and refuses to start without a usable certificate.
func (t ServerTLSConfig) Enabled() bool {
	return t.CertFile != "" || t.KeyFile != "" || len(t.AdditionalCertificates) > 0 ||
		t.ClientCAFile != "" || t.ClientAuth != "" || t.ClientCertAccounts || t.MinVersion != ""
}

// CLILoginConfig is command-line sign-in (`abhed login`) for local accounts.
type CLILoginConfig struct {
	// Enabled serves the device-code and browser (PKCE) sign-in endpoints.
	Enabled bool `json:"enabled,omitempty"`
	// TokenHours is how long a command-line token lasts; 168 (a week) when unset.
	TokenHours int `json:"token_hours,omitempty"`
	// KeyFile holds the key tokens are signed with; ~/.abhed/cli-token.key
	// when unset. Servers sharing an account store share it, or each
	// accepts only the tokens it issued.
	KeyFile string `json:"key_file,omitempty"`
	// ClientID is the identity provider's client the command line signs in
	// through with auth.mode oidc, in an edition that serves it; auth.client_id
	// when unset. It must allow the device grant and loopback redirects.
	ClientID string `json:"client_id,omitempty"`
}

// RetrievalConfig controls the on-prem index. Retrieval is an accelerator over
// agentic grep, not a replacement: the evidence favouring one over the other is
// contested, so Abhed builds both and measures (docs §02 §4).
type RetrievalConfig struct {
	// Enabled builds an index at startup and exposes the search tool.
	Enabled bool `json:"enabled"`
	// Embed adds the vector tier. Off by default - the symbol and BM25 tiers
	// answer most code questions without the cost of embedding a monorepo.
	Embed        bool   `json:"embed"`
	EmbedBaseURL string `json:"embed_base_url,omitempty"`
	EmbedModel   string `json:"embed_model,omitempty"`
	EmbedDims    int    `json:"embed_dims,omitempty"`
}

// SkillsConfig points at directories holding skills.
//
// Deliberately NOT the workspace: a skill body is instructions by
// construction, so reading them from the repository the agent is editing would
// let any cloned project carry its own orders to the agent reading it. Skills
// come from where the operator put them.
type SkillsConfig struct {
	// Dirs each contain one directory per skill, holding a SKILL.md.
	// Defaults to ~/.abhed/skills when unset.
	Dirs []string `json:"dirs,omitempty"`
	// Disabled turns skills off entirely, including the default directory.
	Disabled bool `json:"disabled,omitempty"`
}

// AgentsConfig points at directories of subagent definitions: markdown files
// naming a role, its tools and model, with the role's instructions as the body.
//
// A workspace's own .abhed/agents loads only when the person trusted that
// exact content; the organisation's /etc/abhed/agents always loads.
type AgentsConfig struct {
	// Dirs hold *.md definitions. Defaults to ~/.abhed/agents when unset. A
	// later directory wins a name.
	Dirs []string `json:"dirs,omitempty"`
	// Disabled loads only the organisation's managed definitions.
	Disabled bool `json:"disabled,omitempty"`
}

// K8sConfig enables cluster access. Off by default: reaching a cluster is an
// authorization decision, and the credentials already on the machine are not
// a reason to hand them to an agent without being asked.
type K8sConfig struct {
	Enabled bool `json:"enabled"`
	// Kubeconfig path; empty uses $KUBECONFIG then ~/.kube/config.
	Kubeconfig string `json:"kubeconfig,omitempty"`
	// Context pins which cluster is the default. Empty uses current-context.
	Context   string `json:"context,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	// AllowWrites exposes k8s_apply. Even then every call needs approval;
	// this decides whether the capability exists at all.
	AllowWrites bool `json:"allow_writes,omitempty"`
	// Clusters are the only servers k8s_login may send a stored token to. The
	// model names one; it never supplies a URL.
	Clusters []K8sClusterConfig `json:"clusters,omitempty"`
	// CAFile adds a CA bundle to the system roots for a cluster that names none.
	CAFile string `json:"ca_file,omitempty"`
}

// validateClusters refuses clusters k8s_login could not tell apart or reach
// safely, at load rather than at the first login.
func (k K8sConfig) validateClusters() error {
	seen := map[string]string{}
	for i, c := range k.Clusters {
		if strings.TrimSpace(c.Name) == "" || c.Name != strings.TrimSpace(c.Name) {
			return fmt.Errorf("k8s.clusters[%d]: name is required, without surrounding spaces", i)
		}
		// Rules and "always allow" read a call as cluster/namespace/verb, and
		// a kubeconfig context as context:NAME.
		if strings.ContainsAny(c.Name, "/:*?") {
			return fmt.Errorf("k8s.clusters %q: a name cannot hold / : * or ?, which permission rules on it use", c.Name)
		}
		if prev, dup := seen[strings.ToLower(c.Name)]; dup {
			return fmt.Errorf("k8s.clusters: %q and %q name the same cluster", prev, c.Name)
		}
		seen[strings.ToLower(c.Name)] = c.Name
		u, err := url.Parse(c.Server)
		switch {
		case err != nil:
			// Not echoed: text that does not parse may still hold a credential.
			return fmt.Errorf("k8s.clusters %q: server is not a URL", c.Name)
		case u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
			return fmt.Errorf("k8s.clusters %q: server must not carry a user, password, query "+
				"or fragment; the token comes from k8s_login", c.Name)
		case u.Scheme != "https" || u.Host == "":
			return fmt.Errorf("k8s.clusters %q: server must be an https:// URL with a host, got %q", c.Name, u.String())
		}
	}
	return nil
}

// K8sClusterConfig declares one cluster k8s_login may reach.
type K8sClusterConfig struct {
	Name   string `json:"name"`
	Server string `json:"server"`
	CAFile string `json:"ca_file,omitempty"`
	// InsecureSkipTLSVerify sends the token without checking who answers.
	// For lab clusters only; reported at startup and by doctor.
	InsecureSkipTLSVerify bool `json:"insecure_skip_tls_verify,omitempty"`
}

// SSHConfig declares reachable machines. The agent can only name a host from
// this list, so the operator decides the blast radius, not the model.
type SSHConfig struct {
	Enabled bool            `json:"enabled"`
	Hosts   []SSHHostConfig `json:"hosts,omitempty"`
	// ConnectHosts, when set, are the only addresses ssh_connect may reach:
	// "10.0.0.5", "*.lab.example" or "vm.example:2222", matched without case.
	ConnectHosts []string `json:"connect_hosts,omitempty"`
}

type SSHHostConfig struct {
	Name         string `json:"name"`
	Addr         string `json:"addr"`
	User         string `json:"user"`
	IdentityFile string `json:"identity_file,omitempty"`
	// PasswordEnv names an environment variable, never the password itself.
	PasswordEnv              string `json:"password_env,omitempty"`
	KnownHostsFile           string `json:"known_hosts_file,omitempty"`
	InsecureSkipHostKeyCheck bool   `json:"insecure_skip_host_key_check,omitempty"`
}

// RAGConfig registers external retrieval corpora. Each becomes a rag_<name>
// tool. Empty by default: reaching a corpus is a network egress and an
// authorization decision, not something a default should make.
type RAGConfig struct {
	Corpora []RAGCorpusConfig `json:"corpora,omitempty"`
}

type RAGCorpusConfig struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url"`
	Method      string `json:"method,omitempty"`

	Headers    map[string]string `json:"headers,omitempty"`
	HeadersEnv map[string]string `json:"headers_env,omitempty"`

	QueryField string         `json:"query_field,omitempty"`
	QueryParam string         `json:"query_param,omitempty"`
	TopKField  string         `json:"top_k_field,omitempty"`
	TopK       int            `json:"top_k,omitempty"`
	Body       map[string]any `json:"body,omitempty"`

	ResultsPath string `json:"results_path,omitempty"`
	TextField   string `json:"text_field,omitempty"`
	SourceField string `json:"source_field,omitempty"`
	TitleField  string `json:"title_field,omitempty"`
	ScoreField  string `json:"score_field,omitempty"`

	Enabled bool `json:"enabled"`
}

// MCPConfig registers Model Context Protocol servers. A server not listed here
// does not run: discovery does not imply trust (docs §03 T4).
type MCPConfig struct {
	Servers []MCPServerConfig `json:"servers,omitempty"`
}

type MCPServerConfig struct {
	Name string `json:"name"`
	// Command spawns the server locally; URL reaches one that already runs.
	// Exactly one of the two.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"`
	URL     string   `json:"url,omitempty"`
	// Headers are sent on every request to a URL server. Prefer headers_env
	// for anything secret: it names an environment variable to read instead
	// of putting the credential in a file the agent itself can read.
	Headers    map[string]string `json:"headers,omitempty"`
	HeadersEnv map[string]string `json:"headers_env,omitempty"`
	Enabled    bool              `json:"enabled"`
	AllowTools []string          `json:"allow_tools,omitempty"`
	Digest     string            `json:"digest,omitempty"`
}

// SandboxConfig controls execution isolation. Defaults deny egress, because a
// successful prompt injection then has no channel to exfiltrate through.
type SandboxConfig struct {
	MinTier string `json:"min_tier"` // none|process|container|vm
	// Tier chooses the backend instead of the strongest available one:
	// "fence", the Linux preview. Empty selects as before.
	Tier         string `json:"tier,omitempty"`
	AllowNetwork bool   `json:"allow_network"`
	// Network "allowlist" sends commands' traffic through the session's
	// egress proxy, which reaches only what the egress rules allow, in place
	// of allow_network. Managed only.
	Network       string   `json:"network,omitempty"`
	ReadOnlyPaths []string `json:"read_only_paths,omitempty"`
	// MaxMemoryMB applies on the container and vm tiers only; MaxProcs on
	// every tier but none.
	MaxMemoryMB int `json:"max_memory_mb"`
	MaxProcs    int `json:"max_procs"`
	// Terminal is how the workbench terminal runs: "shell" (the default), one
	// interactive shell per tab, or "lines", each line judged before it runs.
	Terminal string `json:"terminal,omitempty"`
	// TerminalIdleMinutes ends a workbench shell nobody has watched for this
	// long. Zero means 30.
	TerminalIdleMinutes int `json:"terminal_idle_minutes,omitempty"`
	// WriteProtected are workspace paths commands may not write, set by the
	// surface that runs the session, never by a file.
	WriteProtected []string `json:"-"`
	// ProtectGit write-protects what git reads in every git folder in the
	// workspace (config, hooks, commondir and the like); set as WriteProtected
	// is, by a surface that requires it.
	ProtectGit bool `json:"-"`
	// GitProtect is sandbox.protect_git: unset, git's files are protected
	// wherever the tier can hold them; true requires it; false turns it off
	// where no surface requires it.
	GitProtect *bool `json:"protect_git,omitempty"`
	// MicroVMSocket is the socket of an abhed-microvm driver; when set, the
	// control plane can choose the microvm driver by name. Managed only.
	MicroVMSocket string `json:"microvm_socket,omitempty"`
}

// GitProtected reports whether commands are kept from writing git's files,
// and required reports whether a tier that cannot hold them is refused.
func (s SandboxConfig) GitProtected() (on, required bool) {
	switch {
	case s.ProtectGit:
		return true, true
	case s.GitProtect == nil:
		return true, false
	}
	return *s.GitProtect, *s.GitProtect
}

type LimitsConfig struct {
	MaxTurns  int `json:"max_turns"`
	MaxTokens int `json:"max_tokens"`
	// MaxBudgetTokens caps total spend across a session AND its subagents.
	// Zero means unlimited.
	MaxBudgetTokens int  `json:"max_budget_tokens"`
	MaxSubagents    int  `json:"max_subagents"`
	NestedSubagents bool `json:"nested_subagents"`
	// MaxParallelSubagents bounds how many of a `tasks` call's subagents run
	// at once. Zero means all of them, up to the tool's own cap of eight.
	MaxParallelSubagents int `json:"max_parallel_subagents,omitempty"`
	// MaxBackgroundSubagents bounds a session's background subagents alive
	// at once, across its runs. Zero allows none. Default 4.
	MaxBackgroundSubagents int `json:"max_background_subagents"`
	// BackgroundMaxMinutes is each background subagent's wall-clock
	// lifetime. Zero means 60; at most 480.
	BackgroundMaxMinutes int `json:"background_max_minutes,omitempty"`
	// BackgroundShells bounds a session's commands run with run_in_background
	// at once. Zero allows none. Default 4.
	BackgroundShells int `json:"background_shells"`
}

// SubagentsConfig is what a background subagent's result does when it
// arrives while the session is idle.
type SubagentsConfig struct {
	// Wake is off, notify or auto. auto, the default, starts a short run that
	// acts on the result; notify records it for the person's next message; off
	// makes a run wait for its background subagents. A surface may allow
	// less: -p, eval and unattended runs are always off.
	Wake string `json:"wake,omitempty"`
	// MaxWakesPerHour bounds automatic wake runs per session. Zero never wakes. Default 4.
	MaxWakesPerHour int `json:"max_wakes_per_hour"`
	// WakeMaxTurns caps one wake run. Zero means 8.
	WakeMaxTurns int `json:"wake_max_turns,omitempty"`
}

func Default() Config {
	return Config{
		Model: ModelConfig{
			Default: "local",
			Providers: map[string]ProviderConfig{
				"local": {
					Type:    "openai-compatible",
					BaseURL: "http://localhost:11434/v1", // Ollama's default
					// gemma4:26b, chosen by measurement rather than benchmark
					// (docs/ops/model-selection.md). It is MoE — 128 experts,
					// 8 active — so it decodes at ~35 tok/s on an M3 Pro where
					// a dense 27B manages 3.
					//
					// The reason it beats the faster qwen3-coder:30b is not
					// speed but honesty: on a planted two-bug review task it
					// found both, verified its own work, and reported the real
					// command output. qwen3-coder fixed one, misidentified the
					// other, and twice claimed environment restrictions that
					// did not exist. A model that misreports whether it
					// verified something is the worst failure mode in an
					// autonomous agent, because every later decision inherits
					// the false premise.
					Model:         "gemma4:26b",
					ContextWindow: 65536,
				},
			},
		},
		Permissions: PermissionsConfig{
			Mode: "default",
			// Commands with no undo. Denied outright rather than merely asked,
			// because a tired user clicks yes.
			Deny: []string{
				"bash(rm -rf /*)",
				"bash(*mkfs*)",
				"bash(*shutdown*)",
				"write(/etc/**)",
			},
			Allow: []string{
				"bash(git status*)", "bash(git diff*)", "bash(git log*)",
				"bash(ls*)", "bash(pwd)", "bash(cat *)",
			},
			// A command that trusts a workspace for a nested run is the
			// person's decision, not the agent's. So is a memory file, which
			// every later session reads as instructions.
			Ask: []string{"bash(*ABHED_TRUST_WORKSPACE*)", "bash(*trust-workspace*)",
				"write(ABHED.md)", "edit(ABHED.md)", "write(ABHED.local.md)", "edit(ABHED.local.md)",
				"write(AGENTS.md)", "edit(AGENTS.md)",
				// Files that commonly hold credentials are read only with a
				// yes. A speed bump for the read tool, not credential protection:
				// a command can still print them.
				"read(.env)", "read(**/.env)", "read(.env.*)", "read(**/.env.*)",
				"read(*.pem)", "read(**/*.pem)", "read(*.key)", "read(**/*.key)",
				"read(id_rsa*)", "read(**/id_rsa*)", "read(id_ed25519*)", "read(**/id_ed25519*)",
				"read(id_ecdsa*)", "read(**/id_ecdsa*)"},
		},
		Context: ContextConfig{
			CompactAt:   0.90,
			MemoryFiles: []string{"ABHED.md", "ABHED.local.md"},
		},
		Limits: LimitsConfig{
			MaxTurns: 100, MaxTokens: 8192, MaxBudgetTokens: 0,
			MaxSubagents: 20, NestedSubagents: false,
			MaxBackgroundSubagents: 4, BackgroundMaxMinutes: 60,
			BackgroundShells: 4,
		},
		Subagents: SubagentsConfig{Wake: "auto", MaxWakesPerHour: 4, WakeMaxTurns: 8},
		Storage:   StorageConfig{Driver: "memory", Tenant: "default", MaxConns: 10},
		// Secure by default: a cookie that would travel over plain HTTP has
		// to be asked for. Browsers accept Secure cookies on localhost, so
		// local development does not need the exception it used to get.
		Auth: AuthConfig{Mode: "none", CookieSecure: true},
		Sandbox: SandboxConfig{
			MinTier:      "process",
			AllowNetwork: false,
			MaxMemoryMB:  4096,
			MaxProcs:     512,
		},
		// Off by default: Abhed runs air-gapped, and web search is the one tool
		// that deliberately crosses the boundary.
		WebSearch: WebSearchConfig{Enabled: false, Provider: "duckduckgo", MaxResults: 5},
		Suggest:   SuggestConfig{Enabled: true},
		CLI:       CLIConfig{Title: true, Copy: true, Notify: "auto"},
	}
}

// Load assembles configuration from all sources in precedence order, taking
// the workspace's file whole only once the person has trusted it.
func Load(workspace string) (Config, error) {
	return LoadWith(workspace, LoadOptions{})
}

// LoadWith is Load with the caller's say over the workspace file.
func LoadWith(workspace string, o LoadOptions) (Config, error) {
	cfg := Default()
	cfg.noteRuleLayer(LayerDefault)

	var userFile string
	if home, err := os.UserHomeDir(); err == nil {
		userFile = filepath.Join(home, ".abhed", "config.json")
		data, err := mergeFileData(&cfg, userFile)
		if err != nil {
			return cfg, err
		}
		setAside(&cfg, userFile, LayerUser, data)
		noteOrigins(&cfg, userFile, LayerUser, data)
		cfg.noteRuleLayer(LayerUser)
	}
	if o.SettingsName != "" {
		if _, err := mergeData(&cfg, o.SettingsName, o.Settings); err != nil {
			return cfg, err
		}
		setAside(&cfg, o.SettingsName, LayerSettings, o.Settings)
		noteOrigins(&cfg, o.SettingsName, LayerSettings, o.Settings)
		cfg.noteRuleLayer(LayerSettings)
		sum := sha256.Sum256(o.Settings)
		cfg.Settings = SettingsSource{Name: o.SettingsName, SHA256: hex.EncodeToString(sum[:])}
	}
	st, err := mergeWorkspace(&cfg, workspace, userFile, o)
	noteUsers(&st, workspace, o)
	cfg.Workspace = st
	if err != nil {
		return cfg, err
	}
	cfg.noteRuleLayer(LayerWorkspace)

	// Managed config is applied last and marks the engine as org-controlled.
	if err := mergeManaged(&cfg); err != nil {
		return cfg, err
	}
	cfg.noteRuleLayer(LayerManaged)
	cfg.origins = nil
	applyWebIntents(&cfg)
	dropLockedAllow(&cfg, userFile, cfg.Workspace.File, o.SettingsName)
	userAllowlistNarrows(&cfg)

	applyEnv(&cfg)
	w := warnSink(o.Warn)
	warnUnknown(w, cfg.Unknown)
	warnSetAside(w, cfg.SetAside)
	warnNotYetInEffect(w, cfg)
	warnNeverAllows(w, cfg.Permissions.Allow)
	warnRuleFindings(w, cfg.RuleFindings())
	if !o.Quiet {
		warnUntrusted(w, cfg.Workspace)
	}
	return cfg, cfg.Validate()
}

// mergeFile merges the workspace's or the home directory's config. One with a
// second name is refused: a command could rewrite it through that name.
func mergeFile(cfg *Config, path string) error {
	_, err := mergeFileData(cfg, path)
	return err
}

// mergeFileData is mergeFile, returning what it read.
func mergeFileData(cfg *Config, path string) ([]byte, error) {
	if n, err := nlink.Linked(path); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	} else if n > 0 {
		return nil, fmt.Errorf("refusing to load the configuration: %w", nlink.Refusal(path, n))
	}
	return readMerge(cfg, path)
}

// readMerge merges the file at path into cfg and returns what it read, or
// nil when there is no such file.
func readMerge(cfg *Config, path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return mergeData(cfg, path, data)
}

// mergeData merges a file's contents, already read, into cfg.
func mergeData(cfg *Config, path string, data []byte) ([]byte, error) {
	// Unmarshalling onto the existing struct merges: fields absent from the
	// file keep their current value, and lists are replaced wholesale.
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.Unknown = append(cfg.Unknown, unknownKeys(path, data, reflect.TypeFor[Config]())...)
	cfg.SetKeys = append(cfg.SetKeys, managedKeys(data)...)
	return data, nil
}

// applyEnv lets a deployment override the endpoint without editing files,
// which is what container and CI environments need. It runs after the
// managed file, so it never changes a setting that file makes: the value is
// ignored and the warning names the setting.
func applyEnv(cfg *Config) {
	name := cfg.Model.Default
	if p, found := cfg.Model.Providers[name]; found {
		key := "model.providers." + name
		envSet(cfg, "ABHED_BASE_URL", key+".base_url", &p.BaseURL)
		envSet(cfg, "ABHED_MODEL", key+".model", &p.Model)
		envSet(cfg, "ABHED_API_KEY", key+".api_key", &p.APIKey)
		cfg.Model.Providers[name] = p
	}
	envSet(cfg, "ABHED_MIGRATE_DATABASE_URL", "storage.migrate_dsn", &cfg.Storage.MigrateDSN)
	envSet(cfg, "ABHED_USERS_FILE", "auth.users_file", &cfg.Auth.UsersFile)
	if envSet(cfg, "ABHED_DATABASE_URL", "storage.dsn", &cfg.Storage.DSN) &&
		(cfg.Storage.Driver == "" || cfg.Storage.Driver == "memory") {
		if cfg.ManagedSets("storage.driver") {
			envRefused(cfg, "ABHED_DATABASE_URL", "storage.driver")
		} else {
			cfg.Storage.Driver = "postgres"
		}
	}
}

// envSet sets *dst from the variable env unless the managed configuration
// makes key, and reports whether it did.
func envSet(cfg *Config, env, key string, dst *string) bool {
	v := os.Getenv(env)
	if v == "" {
		return false
	}
	if cfg.ManagedSets(key) {
		envRefused(cfg, env, key)
		return false
	}
	*dst = v
	return true
}

// envRefused notes a variable left out because the managed file makes key.
// The value is not kept: it may be a key or a password.
func envRefused(cfg *Config, env, key string) {
	cfg.SetAside = append(cfg.SetAside, SetAsideKey{File: env, Layer: LayerEnv, Key: key,
		Reason: "the managed configuration sets it, and the environment may not change it"})
}

// Provider returns the active provider with its API key resolved.
// ValidateProvider checks a provider's settings are internally consistent, so
// a misconfiguration is a startup error rather than a failed request.
func ValidateProvider(p ProviderConfig) error {
	if p.Type != "watsonx" {
		return nil
	}
	if p.BaseURL == "" {
		return fmt.Errorf("watsonx needs base_url (e.g. https://us-south.ml.cloud.ibm.com)")
	}
	if p.Model == "" {
		return fmt.Errorf("watsonx needs model (e.g. openai/gpt-oss-120b)")
	}
	if p.APIKey == "" && p.APIKeyEnv == "" {
		return fmt.Errorf("watsonx needs api_key_env naming the variable holding the key")
	}
	if p.ProjectID == "" && p.SpaceID == "" {
		return fmt.Errorf("watsonx needs either project_id or space_id")
	}
	if p.ProjectID != "" && p.SpaceID != "" {
		return fmt.Errorf("watsonx takes project_id OR space_id, not both — " +
			"the API rejects a request carrying each")
	}
	return nil
}

func (c Config) Provider() (ProviderConfig, error) {
	return c.ProviderNamed(c.Model.Default)
}

// ProviderNamed resolves one configured provider by name.
//
// Split out from Provider so a caller that lets someone CHOOSE a model — the
// console's picker, `abhed -model` — resolves it exactly the way the default is
// resolved: the key comes from the server's environment via APIKeyEnv, and the
// same validation runs. A second implementation of this would eventually
// forget one of those two things.
//
// The name is looked up in the configured map, never treated as an endpoint.
// That is what stops a caller pointing the agent at a host of their choosing.
func (c Config) ProviderNamed(name string) (ProviderConfig, error) {
	p, found := c.Model.Providers[name]
	if !found {
		return ProviderConfig{}, fmt.Errorf(
			"model %q is not defined. Available: %s",
			name, strings.Join(providerNames(c.Model.Providers), ", "))
	}
	if p.APIKey == "" && p.APIKeyEnv != "" {
		p.APIKey = os.Getenv(p.APIKeyEnv)
	}
	if err := ValidateProvider(p); err != nil {
		return p, fmt.Errorf("model %q: %w", name, err)
	}
	return p, nil
}

func providerNames(m map[string]ProviderConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ToolsConfig tunes the built-in tools.
type ToolsConfig struct {
	// SyntaxCheck is what edit and write do with a change that would leave a
	// file that parsed no longer parsing: "refuse" (the default), "report" or "off".
	SyntaxCheck string `json:"syntax_check,omitempty"`
}

func (c Config) Validate() error {
	switch c.Subagents.Wake {
	case "", "off", "notify", "auto":
	default:
		return fmt.Errorf("subagents.wake is %q; use off, notify or auto", c.Subagents.Wake)
	}
	switch c.CLI.Notify {
	case "", "auto", "bel", "osc9", "off":
	default:
		return fmt.Errorf("cli.notify is %q; use auto, bel, osc9 or off", c.CLI.Notify)
	}
	for _, m := range c.CLI.ModeCycle {
		if !slices.Contains(DefaultModeCycle, m) {
			return fmt.Errorf("cli.mode_cycle may only leave modes out of %s; %q is not one of them",
				strings.Join(DefaultModeCycle, ", "), m)
		}
	}
	switch c.Sandbox.Network {
	case "", NetworkAllowlist:
	default:
		return fmt.Errorf("sandbox.network is %q; use %q, or leave it out and set sandbox.allow_network", c.Sandbox.Network, NetworkAllowlist)
	}
	if _, err := egress.Compile(c.Egress); err != nil {
		return err
	}
	if k := c.ControlPlane.Kubernetes; len(k) > 0 && !jsonObject(k) {
		return errors.New("control_plane.kubernetes must be a JSON object of the Kubernetes driver's options")
	}
	if c.Record.RetentionDays < 0 {
		return fmt.Errorf("record.retention_days is %d; use a number of days, or 0 to keep the record until it is pruned", c.Record.RetentionDays)
	}
	if c.Memory.ImportDepth < 0 || c.Memory.ImportDepth > maxImportDepth {
		return fmt.Errorf("memory.import_depth is %d; use 0 for the default of %d, or a depth up to %d",
			c.Memory.ImportDepth, defaultImportDepth, maxImportDepth)
	}
	if m := c.Suggest.Model; m != "" {
		if _, found := c.Model.Providers[m]; !found {
			return fmt.Errorf("suggest.model is %q, which is not a configured provider; available: %s",
				m, strings.Join(providerNames(c.Model.Providers), ", "))
		}
	}
	if c.Limits.BackgroundMaxMinutes > 480 {
		return fmt.Errorf("limits.background_max_minutes is %d; at most 480", c.Limits.BackgroundMaxMinutes)
	}
	p, err := c.Provider()
	if err != nil {
		return err
	}
	// Whether a base_url is required is the provider's business: anthropic,
	// openai and gemini know their own endpoints, while vllm and watsonx must
	// be told. Asking the registry to build the adapter answers that question
	// and validates the sampling parameters at the same time, so a bad setting
	// is reported here rather than on the first model call.
	if !model.Known(p.Type) {
		return fmt.Errorf("provider %q has unknown type %q; run `abhed providers` "+
			"for the list", c.Model.Default, p.Type)
	}
	if _, err := p.Adapter(); err != nil {
		return fmt.Errorf("provider %q: %w", c.Model.Default, err)
	}
	if c.Permissions.Mode != "" && !knownMode(c.Permissions.Mode) {
		return fmt.Errorf("unknown permission mode %q", c.Permissions.Mode)
	}
	// Checked here so every path fails on a bad rule, not only the CLI.
	for _, l := range []struct {
		name  string
		rules []string
	}{{"deny", c.Permissions.Deny}, {"ask", c.Permissions.Ask}, {"allow", c.Permissions.Allow}} {
		for _, r := range l.rules {
			if _, err := policy.ParseRule(r); err != nil {
				return fmt.Errorf("permissions.%s: %w", l.name, err)
			}
		}
	}
	if err := managedNeverDenies(c.RuleFindings()); err != nil {
		return err
	}
	if err := new(policy.Engine).AllowGitExtensions(c.Permissions.GitExtensions...); err != nil {
		return fmt.Errorf("permissions.git_extensions: %w", err)
	}
	if c.Context.CompactAt <= 0 || c.Context.CompactAt > 1 {
		return fmt.Errorf("context.compact_at must be between 0 and 1, got %v", c.Context.CompactAt)
	}
	if o := c.Context.OffloadAt; o != nil && (*o < 0 || *o >= c.Context.CompactAt) {
		return fmt.Errorf("context.offload_at must be 0 (off) or below compact_at (%v), got %v", c.Context.CompactAt, *o)
	}
	if h := c.Auth.SessionHours; h < 0 || h > MaxSessionHours {
		return fmt.Errorf("auth.session_hours is %d; use a number of hours up to %d (a year), or 0 for the default", h, MaxSessionHours)
	}
	switch c.Auth.Mode {
	case "none", "proxy", "oidc", "local", "":
	default:
		return fmt.Errorf("unknown auth.mode %q (want none, local, proxy or oidc)", c.Auth.Mode)
	}
	if err := c.validateTLS(); err != nil {
		return err
	}
	if err := c.validateCLILogin(); err != nil {
		return err
	}
	if err := c.validateControlPlanePaths(); err != nil {
		return err
	}
	if err := c.Auth.Workload.validate(); err != nil {
		return err
	}
	if c.Auth.Mode == "oidc" && c.Auth.Issuer == "" && c.Auth.Provider == "" {
		return fmt.Errorf("auth.mode is oidc but neither auth.issuer nor " +
			"auth.provider (google, microsoft) is set")
	}
	if err := c.Auth.GitHub.validate(); err != nil {
		return err
	}
	if u := c.Auth.ProxyLogoutURL; u != "" && !validLogoutURL(u) {
		return fmt.Errorf("auth.proxy_logout_url %q must be an http(s) URL or a path on this host", u)
	}
	if err := c.K8s.validateClusters(); err != nil {
		return err
	}
	switch strings.ToLower(c.WebSearch.Provider) {
	case "", "duckduckgo", "ddg", "brave", "tavily", "serper", "searxng":
	default:
		return fmt.Errorf("unknown web_search.provider %q "+
			"(want duckduckgo, brave, tavily, serper or searxng)", c.WebSearch.Provider)
	}
	for _, h := range c.WebFetch.AllowedHosts {
		if !validHostPattern(h) {
			return fmt.Errorf("web_fetch.allowed_hosts: %q is not a host name or *.domain "+
				"(no scheme, port or path; a wildcard needs a domain of two labels or more)", h)
		}
	}
	switch c.Storage.Driver {
	case "memory", "postgres", "":
	default:
		return fmt.Errorf("unknown storage.driver %q (want memory or postgres)", c.Storage.Driver)
	}
	if c.Storage.Driver == "postgres" && c.Storage.DSN == "" && os.Getenv("ABHED_DATABASE_URL") == "" {
		return fmt.Errorf("storage.driver is postgres but no DSN is set (use storage.dsn or ABHED_DATABASE_URL)")
	}
	switch c.Tools.SyntaxCheck {
	case "", "refuse", "report", "off":
	default:
		return fmt.Errorf("unknown tools.syntax_check %q (want refuse, report or off)", c.Tools.SyntaxCheck)
	}
	switch c.Sandbox.MinTier {
	case "none", "process", "container", "vm", "":
	default:
		return fmt.Errorf("unknown sandbox.min_tier %q (want none|process|container|vm)", c.Sandbox.MinTier)
	}
	switch c.Sandbox.Tier {
	case "", "fence":
	default:
		return fmt.Errorf("unknown sandbox.tier %q (want fence, or leave it unset)", c.Sandbox.Tier)
	}
	if c.Fence.CPUPercent < 0 || c.Fence.CPUPercent > 102400 {
		return fmt.Errorf("fence.cpu_percent %d is outside 0 to 102400", c.Fence.CPUPercent)
	}
	switch c.Sandbox.Terminal {
	case "", "shell", "lines":
	default:
		return fmt.Errorf("unknown sandbox.terminal %q (want shell or lines)", c.Sandbox.Terminal)
	}
	return nil
}

// validLogoutURL accepts an absolute http(s) URL or a path with no host; a
// "//" or backslash prefix would send the browser to another site.
func validLogoutURL(s string) bool {
	if strings.ContainsAny(s, "\\\x00\r\n") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if strings.HasPrefix(s, "/") {
		return u.Host == "" && u.Scheme == "" && !strings.HasPrefix(s, "//")
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (g GitHubAuthConfig) validate() error {
	for _, o := range g.Orgs {
		if strings.TrimSpace(o) == "" || strings.Contains(o, "/") {
			return fmt.Errorf("auth.github.orgs: %q is not an organisation name", o)
		}
	}
	for _, t := range g.Teams {
		if org, team, ok := strings.Cut(t, "/"); !ok || strings.TrimSpace(org) == "" ||
			strings.TrimSpace(team) == "" || strings.Contains(team, "/") {
			return fmt.Errorf("auth.github.teams: %q must be written org/team-slug", t)
		}
	}
	if g.AllowAny && len(g.Orgs)+len(g.Teams) > 0 {
		return fmt.Errorf("auth.github.allow_any cannot be combined with orgs or teams; " +
			"the restriction would still apply")
	}
	return nil
}

// WriteDefault creates a starter config, so `abhed init` produces something
// the user can edit rather than a blank file.
func WriteDefault(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := defaultConfigJSON()
	if err != nil {
		return err
	}
	// A config can carry keys. Owner-only, like every other file that can.
	return os.WriteFile(path, data, 0o600)
}

func defaultConfigJSON() ([]byte, error) {
	data, err := json.MarshalIndent(Default(), "", "  ")
	return append(data, '\n'), err
}

// TelemetryConfig exports the event stream as OpenTelemetry traces.
//
// Every session is already an ordered log of events; this turns that log into
// spans — one per session, one per tool call, one per subagent — and ships
// them over OTLP/HTTP to whatever collector the operator runs. Nothing else
// changes: the event log stays the source of truth, and the exporter is a
// tap on it, so a collector being down cannot slow or fail a session.
type TelemetryConfig struct {
	Enabled bool `json:"enabled"`
	// Endpoint is the OTLP/HTTP base, e.g. http://otel-collector:4318. The
	// exporter posts to <Endpoint>/v1/traces.
	Endpoint string `json:"endpoint,omitempty"`
	// Headers are sent with every export, for collectors that want a token.
	Headers map[string]string `json:"headers,omitempty"`
	// ServiceName is the resource attribute a backend groups traces under.
	ServiceName string `json:"service_name,omitempty"`
}

// ScheduleConfig is one recurring run.
type ScheduleConfig struct {
	// Name identifies the schedule in the admin view and in the session list,
	// where the run appears as user "schedule:<name>".
	Name string `json:"name"`
	// Cron is a five-field expression (minute hour day month weekday) or one
	// of @hourly, @daily, @weekly, @monthly. Evaluated in the server's local
	// time.
	Cron   string `json:"cron"`
	Prompt string `json:"prompt"`
	// Mode narrows permissions for the run. A scheduled run has no human to
	// approve anything, so an "ask" is refused; "auto" or "plan" are the modes
	// that make sense here. Empty uses the configured default.
	Mode     string `json:"mode,omitempty"`
	Provider string `json:"provider,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

// OffloadFraction resolves context.offload_at: unset is the default, an
// explicit zero is off. A pointer because those two have to stay different.
func (c ContextConfig) OffloadFraction() float64 {
	if c.OffloadAt == nil {
		return 0.60
	}
	return *c.OffloadAt
}

// validHostPattern is a host name, or *. and a domain: what web_fetch's
// allowlist can match. A URL or a pattern of any other shape would match
// nothing, and an operator would believe it did.
func validHostPattern(h string) bool {
	h, wild := strings.CutPrefix(h, "*.")
	if h == "" || len(h) > 253 {
		return false
	}
	// *.com would allow every site under a top-level domain.
	if wild && !strings.Contains(h, ".") {
		return false
	}
	// A name ending in a number is never a host web_fetch fetches, and a
	// wildcard over one would match address numbers: only a plain IPv4
	// address may end in digits.
	labels := strings.Split(h, ".")
	if last := strings.ToLower(labels[len(labels)-1]); strings.HasPrefix(last, "0x") || strings.Trim(last, "0123456789") == "" {
		a, err := netip.ParseAddr(h)
		if wild || err != nil || !a.Is4() || a.String() != h {
			return false
		}
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, c := range label {
			ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
			if !ok {
				return false
			}
		}
	}
	return true
}

// validateTLS checks server.tls for what can be told without reading a file.
func (c Config) validateTLS() error {
	t := c.Server.TLS
	if !t.Enabled() {
		return nil
	}
	if t.CertFile == "" || t.KeyFile == "" {
		return fmt.Errorf("server.tls needs both cert_file and key_file")
	}
	for i, p := range t.AdditionalCertificates {
		if p.CertFile == "" || p.KeyFile == "" {
			return fmt.Errorf("server.tls.additional_certificates[%d] needs both cert_file and key_file", i)
		}
	}
	switch t.MinVersion {
	case "", "1.2", "1.3":
	default:
		return fmt.Errorf("server.tls.min_version is %q; use 1.2 or 1.3", t.MinVersion)
	}
	switch t.ClientAuth {
	case "", "require", "optional":
	default:
		return fmt.Errorf("server.tls.client_auth is %q; use require or optional", t.ClientAuth)
	}
	if (t.ClientAuth != "" || t.ClientCertAccounts) && t.ClientCAFile == "" {
		return fmt.Errorf("server.tls.client_auth and client_cert_accounts need server.tls.client_ca_file")
	}
	if t.ClientCertAccounts && c.Auth.Mode != "local" {
		return fmt.Errorf("server.tls.client_cert_accounts signs local accounts in, so it needs auth.mode local")
	}
	if t.ReloadSeconds < 0 || t.ReloadSeconds > 3600 {
		return fmt.Errorf("server.tls.reload_seconds is %d; use 1 to 3600, or 0 for 10", t.ReloadSeconds)
	}
	return nil
}

// validateCLILogin checks auth.cli.
func (c Config) validateCLILogin() error {
	l := c.Auth.CLI
	if l.TokenHours < 0 || l.TokenHours > 24*90 {
		return fmt.Errorf("auth.cli.token_hours is %d; use 1 to %d, or 0 for a week", l.TokenHours, 24*90)
	}
	if l.Enabled && c.Auth.Mode != "local" && c.Auth.Mode != "oidc" {
		return fmt.Errorf("auth.cli.enabled signs accounts in from the command line, so it needs auth.mode local (or oidc, in an edition that serves it)")
	}
	return nil
}

// jsonObject reports whether raw is one JSON object.
func jsonObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil
}
