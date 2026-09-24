// Package config loads Abhed's layered configuration.
//
// Precedence, lowest to highest: built-in defaults, user config, project
// config, environment, flags — except managed config (/etc/abhed), which
// always wins so local settings cannot escalate past org policy (docs P7).
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/zybuu-ai/abhed/internal/model"
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
	K8s         K8sConfig         `json:"k8s,omitempty"`
	SSH         SSHConfig         `json:"ssh,omitempty"`
	Limits      LimitsConfig      `json:"limits"`
	Sandbox     SandboxConfig     `json:"sandbox"`
	MCP         MCPConfig         `json:"mcp"`
	Retrieval   RetrievalConfig   `json:"retrieval"`
	WebSearch   WebSearchConfig   `json:"web_search"`
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

	// Managed is set when the config came from the org-managed path.
	Managed bool `json:"-"`
	// Unknown lists the keys in the files that no setting reads.
	Unknown []UnknownKey `json:"-"`
}

type ModelConfig struct {
	Default   string                    `json:"default"`
	Providers map[string]ProviderConfig `json:"providers"`
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
	// ABHED_USERS_FILE overrides it.
	UsersFile   string `json:"users_file,omitempty"`
	Issuer      string `json:"issuer,omitempty"`
	Audience    string `json:"audience,omitempty"`
	JWKSURL     string `json:"jwks_url,omitempty"`
	TenantClaim string `json:"tenant_claim,omitempty"`
	GroupsClaim string `json:"groups_claim,omitempty"`
	// RequireGroup gates all access on membership, above tenancy.
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
	SessionHours int  `json:"session_hours,omitempty"`
	// ProxyLogoutURL is the authenticating proxy's own sign-out, in proxy
	// mode. Without it the console offers no sign-out, since the proxy owns it.
	ProxyLogoutURL string `json:"proxy_logout_url,omitempty"`
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
}

// SSHConfig declares reachable machines. The agent can only name a host from
// this list, so the operator decides the blast radius, not the model.
type SSHConfig struct {
	Enabled bool            `json:"enabled"`
	Hosts   []SSHHostConfig `json:"hosts,omitempty"`
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
	MinTier       string   `json:"min_tier"` // none|process|container|vm
	AllowNetwork  bool     `json:"allow_network"`
	ReadOnlyPaths []string `json:"read_only_paths,omitempty"`
	MaxMemoryMB   int      `json:"max_memory_mb"`
	MaxProcs      int      `json:"max_procs"`
	// Terminal is how the workbench terminal runs: "shell" (the default), one
	// interactive shell per tab, or "lines", each line judged before it runs.
	Terminal string `json:"terminal,omitempty"`
	// TerminalIdleMinutes ends a workbench shell nobody has watched for this
	// long. Zero means 30.
	TerminalIdleMinutes int `json:"terminal_idle_minutes,omitempty"`
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
		},
		Context: ContextConfig{
			CompactAt:   0.90,
			MemoryFiles: []string{"ABHED.md", "ABHED.local.md"},
		},
		Limits: LimitsConfig{
			MaxTurns: 100, MaxTokens: 8192, MaxBudgetTokens: 0,
			MaxSubagents: 20, NestedSubagents: false,
		},
		Storage: StorageConfig{Driver: "memory", Tenant: "default", MaxConns: 10},
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
	}
}

// Load assembles configuration from all sources in precedence order.
func Load(workspace string) (Config, error) {
	cfg := Default()

	if home, err := os.UserHomeDir(); err == nil {
		if err := mergeFile(&cfg, filepath.Join(home, ".abhed", "config.json")); err != nil {
			return cfg, err
		}
	}
	if err := mergeFile(&cfg, filepath.Join(workspace, ".abhed", "config.json")); err != nil {
		return cfg, err
	}

	// Managed config is applied last and marks the engine as org-controlled.
	managed := filepath.Join("/etc", "abhed", "config.json")
	if _, err := os.Stat(managed); err == nil {
		if err := mergeFile(&cfg, managed); err != nil {
			return cfg, err
		}
		cfg.Managed = true
		for i := range cfg.Unknown {
			cfg.Unknown[i].Managed = cfg.Unknown[i].File == managed
		}
	}

	applyEnv(&cfg)
	warnUnknown(cfg.Unknown)
	return cfg, cfg.Validate()
}

func mergeFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	// Unmarshalling onto the existing struct merges: fields absent from the
	// file keep their current value, and lists are replaced wholesale.
	if err := json.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.Unknown = append(cfg.Unknown, unknownKeys(path, data, reflect.TypeFor[Config]())...)
	return nil
}

// applyEnv lets a deployment override the endpoint without editing files,
// which is what container and CI environments need.
func applyEnv(cfg *Config) {
	name := cfg.Model.Default
	p, found := cfg.Model.Providers[name]
	if !found {
		return
	}
	if v := os.Getenv("ABHED_BASE_URL"); v != "" {
		p.BaseURL = v
	}
	if v := os.Getenv("ABHED_MODEL"); v != "" {
		p.Model = v
	}
	if v := os.Getenv("ABHED_API_KEY"); v != "" {
		p.APIKey = v
	}
	cfg.Model.Providers[name] = p

	if v := os.Getenv("ABHED_MIGRATE_DATABASE_URL"); v != "" {
		cfg.Storage.MigrateDSN = v
	}
	if v := os.Getenv("ABHED_USERS_FILE"); v != "" {
		cfg.Auth.UsersFile = v
	}
	if v := os.Getenv("ABHED_DATABASE_URL"); v != "" {
		cfg.Storage.DSN = v
		if cfg.Storage.Driver == "" || cfg.Storage.Driver == "memory" {
			cfg.Storage.Driver = "postgres"
		}
	}
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
	switch c.Permissions.Mode {
	case "default", "accept-edits", "plan", "auto", "bypass", "":
	default:
		return fmt.Errorf("unknown permission mode %q", c.Permissions.Mode)
	}
	if c.Context.CompactAt <= 0 || c.Context.CompactAt > 1 {
		return fmt.Errorf("context.compact_at must be between 0 and 1, got %v", c.Context.CompactAt)
	}
	if o := c.Context.OffloadAt; o != nil && (*o < 0 || *o >= c.Context.CompactAt) {
		return fmt.Errorf("context.offload_at must be 0 (off) or below compact_at (%v), got %v", c.Context.CompactAt, *o)
	}
	switch c.Auth.Mode {
	case "none", "proxy", "oidc", "local", "":
	default:
		return fmt.Errorf("unknown auth.mode %q (want none, local, proxy or oidc)", c.Auth.Mode)
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
	switch strings.ToLower(c.WebSearch.Provider) {
	case "", "duckduckgo", "ddg", "brave", "tavily", "serper", "searxng":
	default:
		return fmt.Errorf("unknown web_search.provider %q "+
			"(want duckduckgo, brave, tavily, serper or searxng)", c.WebSearch.Provider)
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
	data, err := json.MarshalIndent(Default(), "", "  ")
	if err != nil {
		return err
	}
	// A config can carry keys. Owner-only, like every other file that can.
	return os.WriteFile(path, append(data, '\n'), 0o600)
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
