// Package abhed embeds the agent in another Go program.
//
// Everything Abhed does lives under internal/, which Go refuses to let another
// module import — deliberate for a binary, and a wall for anyone who wants the
// agent inside their own service. This package is the supported surface across
// that wall: it is small on purpose, so the internals stay free to change.
//
// Policy, audit and deny rules hold when embedded. Policy still decides what runs,
// every action is still recorded as an event, and an extension still cannot
// permit what a deny rule forbids. A caller supplies its own approver and
// receives the event stream, which is the point — a host application usually
// has better ideas than a terminal prompt about how to ask for permission.
//
// The organisation's managed configuration (/etc/abhed/config.json) binds an
// embedded agent as it binds the CLI and the server, whether or not ConfigDir
// is set: Options may tighten what it sets and never loosen it, and New
// returns an error for an option that would.
//
// Stored secrets become [secret:NAME] as on the command line, before the record,
// OnEvent, the model, Approve or a returned answer sees them; nothing turns it off.
//
// One guarantee does NOT come with it by default: this package builds no
// sandbox unless the managed configuration sets one or Options.Sandbox asks
// for the configured one. Otherwise bash runs with the privileges of the
// process that embedded it, where the CLI would have wrapped it in the
// configured tier. A host that needs isolation owns it — a container, a jail,
// a separate user — exactly as for any other library that shells out.
package abhed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// Event is one recorded action or observation. The stream is the session: a
// caller that stores it can replay the run exactly.
type Event = agent.Event

// Usage reports what a run cost.
type Usage = agent.Usage

// Decision is what policy decided about a tool call.
type Decision = policy.Result

// ExtensionConfig describes an extension process. It may veto a call, never
// permit one.
type ExtensionConfig = extension.Config

// Options configures an Agent. Workspace and a model are the minimum.
type Options struct {
	// Workspace is the directory the agent may read and write. Every file
	// tool is scoped to it.
	Workspace string

	// ConfigDir loads .abhed/config.json from a directory, the same file the
	// CLI reads, with the user's and the managed file. Provider overrides what
	// it names. Without it only the managed file, if any, is read. That file
	// is untrusted until the person trusts it (`abhed trust`); until then
	// only the settings that tighten apply. Agent.WorkspaceTrust reports it.
	ConfigDir string

	// WorkspaceTrust overrides the recorded decision about ConfigDir's file:
	// config.TrustGranted takes it whole for this agent, config.TrustRefused
	// takes only what tightens. Empty follows the decision and ABHED_TRUST_WORKSPACE.
	WorkspaceTrust config.TrustChoice

	// AllowDefaultModel runs on the configured default model when ConfigDir's
	// untrusted file names its own and was ignored. Without it New returns
	// ErrUntrustedModel then, unless WorkspaceTrust or Provider is set.
	AllowDefaultModel bool

	// Provider names the model directly, for a caller that would rather not
	// keep a config file.
	Provider *Provider

	// Mode is the permission mode: default, plan, accept-edits, auto, bypass.
	// Empty means the configured mode, else default, which asks before every
	// mutation. Under a managed configuration bypass is refused, and if it
	// sets permissions.mode only that mode or plan may be chosen.
	Mode string

	// SyntaxCheck overrides tools.syntax_check: "refuse" (the default),
	// "report" or "off". It governs edits that would break a file's syntax.
	// If the managed configuration sets it, it may only be made stricter.
	SyntaxCheck string

	// Allow and Deny are policy rules, e.g. "bash(go test*)", added to the
	// configured ones. Deny is absolute: no mode, extension or approver
	// overrides it. Allow is refused if the managed configuration sets
	// permissions.allow. A bash allow rule whose pattern holds ; & | ( ) < >
	// $( ${ a backtick or a newline never matches, and a warning names it.
	Allow []string
	Deny  []string

	// Approve decides tool calls that policy routes to a prompt. Nil refuses
	// them, which is the safe default when there is nobody to ask.
	Approve func(ctx context.Context, tool string, args json.RawMessage, d Decision) (bool, error)

	// OnEvent receives every event, in the order the store records them, from
	// a goroutine of its own. The agent does not wait on it: events waiting for
	// a slow or stuck OnEvent are held in memory until it catches up or the
	// agent is closed. Agent.Flush waits for it to catch up.
	OnEvent func(Event)

	// MaxTurns bounds one conversation. Zero uses the default, or the managed
	// limits.max_turns, which it may not exceed.
	MaxTurns int

	// SystemPrompt replaces the built-in prompt entirely. Most callers want
	// AppendSystem instead.
	SystemPrompt string
	// AppendSystem adds host-specific rules to the built-in prompt.
	AppendSystem string

	// Extensions are subprocesses that may veto a tool call. With
	// ConfiguredTools, the tools they provide are offered too.
	Extensions []ExtensionConfig

	// ConfiguredTools gives the agent the tool set the CLI runs with, as the
	// configuration enables it: subagents (task and tasks, sharing this
	// agent's policy, approver and budget), MCP servers, the tools extensions
	// provide, skills and their pipelines, web search, retrieval, rag corpora,
	// and the Kubernetes and SSH tools, and the built-in prompt carries the
	// ABHED.md memory files. Off, the agent has the built-in file, shell and
	// todo tools only and no memory files, so an embedder decides what else
	// it reaches and reads. A configuration file ConfigDir holds that is not
	// trusted adds none of it.
	ConfiguredTools bool

	// Sandbox runs bash in the tier the configuration's sandbox section asks
	// for (process by default), as the CLI does. New returns an error when
	// that tier is not available here, rather than running bash without it.
	Sandbox bool
}

// ErrUntrustedModel is New's refusal to run on another model than the one
// ConfigDir's file names, because that file is not trusted.
var ErrUntrustedModel = errors.New("the workspace configuration's model settings were ignored because it is not trusted")

// Provider names a model endpoint.
type Provider struct {
	Type          string // anthropic, openai, ollama, vllm, … see Providers()
	BaseURL       string
	Model         string
	APIKey        string
	ContextWindow int
	Temperature   *float64
	TopP          *float64
	MaxTokens     int
}

// Agent is an embedded Abhed.
type Agent struct {
	registry *tools.Registry
	loop     *agent.Loop
	store    *agent.MemStore
	set      *toolset.Set
	id       string
	fwd      *forwarder
	redact   *secrets.Redactor
	trust    config.WorkspaceTrust
}

// New builds an agent.
func New(ctx context.Context, opts Options) (*Agent, error) {
	if opts.Workspace == "" {
		return nil, fmt.Errorf("abhed: Workspace is required")
	}

	// The managed configuration applies with or without a config file.
	load := config.LoadManaged
	if opts.ConfigDir != "" {
		load = func() (config.Config, error) {
			return config.LoadWith(opts.ConfigDir, config.LoadOptions{Trust: opts.WorkspaceTrust})
		}
	}
	cfg, err := load()
	if err != nil {
		return nil, fmt.Errorf("abhed: %w", err)
	}
	if keys := untrustedModelKeys(cfg.Workspace); len(keys) > 0 && opts.Provider == nil &&
		opts.WorkspaceTrust == config.TrustAsStored && !opts.AllowDefaultModel {
		return nil, fmt.Errorf("abhed: %w (%s in %s): trust it with `abhed trust grant`, or set Options.WorkspaceTrust, "+
			"Options.Provider or Options.AllowDefaultModel", ErrUntrustedModel, strings.Join(keys, ", "), config.Printable(cfg.Workspace.File))
	}
	if cfg, err = cfg.Apply(config.Overrides{
		Mode: opts.Mode, SyntaxCheck: opts.SyntaxCheck, MaxTurns: opts.MaxTurns,
		Allow: opts.Allow, Deny: opts.Deny,
	}); err != nil {
		return nil, fmt.Errorf("abhed: %w", err)
	}
	if p := opts.Provider; p != nil {
		cfg.Model.Default = "embedded"
		cfg.Model.Providers = map[string]config.ProviderConfig{"embedded": {
			Type: p.Type, BaseURL: p.BaseURL, Model: p.Model, APIKey: p.APIKey,
			ContextWindow: p.ContextWindow,
			Params: config.ParamsConfig{
				Temperature: p.Temperature, TopP: p.TopP, MaxTokens: p.MaxTokens,
			},
		}}
	}

	provider, err := cfg.Provider()
	if err != nil {
		return nil, fmt.Errorf("abhed: %w", err)
	}
	// The CLI's redactor; a store that exists but cannot be loaded refuses the session.
	red, err := secrets.Default().LoadRedactor()
	if err != nil {
		return nil, fmt.Errorf("abhed: %w", err)
	}
	adapter, err := provider.Adapter()
	if err != nil {
		return nil, fmt.Errorf("abhed: %w", err)
	}

	sess, err := tools.NewSession(opts.Workspace)
	if err != nil {
		return nil, fmt.Errorf("abhed: %w", err)
	}
	if sess.Syntax, err = tools.ParseSyntaxMode(cfg.Tools.SyntaxCheck); err != nil {
		return nil, fmt.Errorf("abhed: %w", err)
	}

	pol := policy.New(policy.Mode(orDefault(cfg.Permissions.Mode, "default")))
	pol.Managed = cfg.Managed
	pol.Roots = sess.PolicyRoots
	if err := pol.AddDeny(cfg.Permissions.Deny...); err != nil {
		return nil, fmt.Errorf("abhed: deny rule: %w", err)
	}
	if err := pol.AddAsk(cfg.Permissions.Ask...); err != nil {
		return nil, fmt.Errorf("abhed: ask rule: %w", err)
	}
	if err := pol.AddAllow(cfg.Permissions.Allow...); err != nil {
		return nil, fmt.Errorf("abhed: allow rule: %w", err)
	}

	// A managed sandbox setting binds here too; otherwise bash is unsandboxed
	// unless the caller asked for the configured sandbox.
	// The configuration's own folder may hold the workspace, as a worktree;
	// its .abhed is state all the same.
	var stateRoots []string
	if opts.ConfigDir != "" {
		stateRoots = append(stateRoots, opts.ConfigDir)
		tools.AddStatePath(filepath.Join(opts.ConfigDir, tools.StateDir))
	}
	bash := tools.Bash{}
	if opts.Sandbox || cfg.ManagedSets("sandbox") {
		sb, err := sandboxconfig.Build(cfg, opts.Workspace, stateRoots...)
		if err != nil {
			return nil, fmt.Errorf("abhed: %w", err)
		}
		bash.Sandbox = sb.Command
		bash.Isolation = tools.Isolation{Tier: string(sb.Tier()), Network: cfg.Sandbox.AllowNetwork}
		if in, ok := sb.(sandbox.Interactive); ok {
			bash.Shell, bash.Isolation.Backend = in.Shell, in.Backend()
		}
	}
	// A users or secrets file kept outside .abhed is state all the same.
	for _, p := range sandboxconfig.StatePaths(cfg, opts.Workspace) {
		tools.AddStatePath(p)
	}

	parts := toolset.Vetoes
	if opts.ConfiguredTools {
		parts = toolset.All
	}
	set := toolset.Build(ctx, cfg, toolset.Options{
		Workspace: opts.Workspace, Bash: bash, Parts: parts, Extensions: opts.Extensions,
		Vault: secrets.Default(),
	})
	// A skill's own directory is reachable, as it is from the command line.
	for _, dir := range set.SkillDirs() {
		if err := sess.AddRoot(dir); err != nil {
			set.Close()
			return nil, fmt.Errorf("abhed: skill directory: %w", err)
		}
	}
	toolset.Police(set.Extensions, pol, "embedded")

	store := agent.NewMemStore()
	id := fmt.Sprintf("embedded-%d", time.Now().UnixNano())
	// Every write goes through the forwarder, so OnEvent misses none, from the
	// first event on.
	fwd := newForwarder(store, opts.OnEvent != nil)
	rec := agent.NewRecorder(fwd, id, "")
	rec.Redact = red

	system := opts.SystemPrompt
	switch {
	case system != "":
	case opts.ConfiguredTools:
		system = toolset.SystemPrompt(opts.Workspace, adapter, set.SkillListing)
	default:
		// No ABHED.md: an embedder running on repositories it does not own
		// takes the workspace's instructions only by opting in.
		system = agent.BuildSystemPrompt(agent.BuildOptions{
			Profile: "main", Workspace: opts.Workspace,
			Model: adapter.Profile().Name, ContextWindow: adapter.Profile().ContextWindow,
		})
	}
	if opts.AppendSystem != "" {
		system += "\n\n" + opts.AppendSystem
	}

	loopCfg := toolset.LoopConfig(cfg, system)
	// The file's max_turns binds an embedded agent only when the organisation sets it.
	loopCfg.MaxTurns = agent.DefaultConfig().MaxTurns
	if (opts.MaxTurns > 0 || cfg.ManagedSets("limits.max_turns")) && cfg.Limits.MaxTurns > 0 {
		loopCfg.MaxTurns = cfg.Limits.MaxTurns
	}

	approver := approverFor(opts.Approve, red)
	registry := set.Registry
	budget := toolset.Budget(cfg)
	if opts.ConfiguredTools {
		// The child's events stay in the store, reached through the parent's
		// subagent.* events; OnEvent carries this agent's own record, as the
		// command line's JSON output does.
		f := &agent.SubagentFactory{Adapter: adapter, Policy: pol, Session: sess, Store: store,
			Budget: budget, Config: loopCfg, Workspace: opts.Workspace, Redact: red}
		registry = toolset.Subagents(registry, f, cfg.Limits.MaxParallelSubagents)
	}

	loop := agent.NewLoop(adapter, registry, pol, approver, sess, rec, loopCfg)
	loop.Compactor = agent.NewCompactor(adapter, loopCfg.CompactAt)
	toolset.Summarize(loop.Compactor, set.Extensions, id)
	loop.Budget = budget

	// The loop runs on its own copy of the registry, which RunJSON must add its tool to.
	a := &Agent{loop: loop, store: store, set: set, id: id, registry: loop.Tools, fwd: fwd, redact: red, trust: cfg.Workspace}
	if opts.OnEvent != nil {
		go fwd.run(opts.OnEvent)
	}
	return a, nil
}

// Run sends a prompt and returns the agent's final message.
func (a *Agent) Run(ctx context.Context, prompt string) (string, error) {
	reason, err := a.loop.Run(ctx, prompt)
	if err != nil {
		return "", err
	}
	if reason != agent.TermCompleted {
		return a.lastMessage(), fmt.Errorf("abhed: ended as %s", reason)
	}
	return a.lastMessage(), nil
}

// RunJSON runs a prompt whose answer must be a JSON value matching schema,
// and decodes it into out.
//
// This is what makes the harness embeddable rather than merely runnable: a
// program gets a typed value back, not prose to parse. It works on every
// provider because the answer is delivered by calling a tool whose input
// schema is the caller's schema; the harness validates and either accepts —
// ending the run — or tells the model, path by path, what to fix. The schema
// supports types, required, enum, bounds, patterns, nesting, $ref and
// anyOf/oneOf/allOf; a keyword the validator would not enforce is refused at
// the start rather than ignored.
//
// A run that ends without a valid answer returns ErrNoResult, which carries
// the model's last message for diagnostics.
func (a *Agent) RunJSON(ctx context.Context, prompt string, schema json.RawMessage, out any) error {
	raw, err := a.RunStructured(ctx, prompt, schema)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("abhed: result matched the schema but not the target type: %w", err)
	}
	return nil
}

// RunStructured is RunJSON without the decode: the validated JSON, with stored
// secrets redacted, so it may no longer match schema (see docs/guide/09-sdk.md).
func (a *Agent) RunStructured(ctx context.Context, prompt string, schema json.RawMessage) (json.RawMessage, error) {
	raw, reason, err := agent.RunStructured(ctx, a.loop, a.registry, prompt, schema)
	if err != nil {
		var nr agent.ErrNoResult
		if errors.As(err, &nr) {
			return nil, ErrNoResult{Reason: string(nr.Reason), LastMessage: redactText(a.redact, nr.Last)}
		}
		return nil, fmt.Errorf("abhed: %w", err)
	}
	raw = redactJSON(a.redact, raw)
	if reason != agent.TermCompleted {
		return raw, fmt.Errorf("abhed: ended as %s", reason)
	}
	return raw, nil
}

// ErrNoResult is returned by RunJSON when the run ended without the model
// delivering an answer that matched the schema.
type ErrNoResult struct {
	Reason      string
	LastMessage string
}

func (e ErrNoResult) Error() string {
	return "abhed: run ended (" + e.Reason + ") without a result matching the schema"
}

// Continue sends a follow-up on the same conversation.
func (a *Agent) Continue(ctx context.Context, prompt string) (string, error) {
	return a.Run(ctx, prompt)
}

// Steer redirects a run already in progress, applied at the next turn
// boundary. Safe to call from another goroutine.
func (a *Agent) Steer(text string) { a.loop.Steer(text) }

// WorkspaceTrust reports whether ConfigDir's file was taken whole, and which
// of its settings were ignored because it is not trusted.
func (a *Agent) WorkspaceTrust() config.WorkspaceTrust { return a.trust }

// Events returns everything recorded so far.
func (a *Agent) Events() []Event {
	evs, _ := a.store.Events(a.id)
	return evs
}

// Usage reports what the conversation has cost.
func (a *Agent) Usage() Usage { return a.loop.Usage() }

// Fork continues the conversation from step throughSeq, 0 meaning the whole
// conversation as it stands. It records a conversation.forked event, so the
// steps after throughSeq stay in the record but leave the conversation (see
// Live), and it refuses a step past the end or one an earlier fork abandoned.
func (a *Agent) Fork(throughSeq int64) error {
	_, err := a.loop.ForkTo(a.Events(), throughSeq)
	return err
}

// ExportHTML renders the session as a self-contained page.
func (a *Agent) ExportHTML() string { return agent.ExportHTML(a.id, a.Events()) }

// SetModel swaps the provider mid-conversation, keeping the history. A switch
// the record refuses is not made.
func (a *Agent) SetModel(p Provider) error {
	cfg := config.ProviderConfig{
		Type: p.Type, BaseURL: p.BaseURL, Model: p.Model, APIKey: p.APIKey,
		ContextWindow: p.ContextWindow,
	}
	next, err := cfg.Adapter()
	if err != nil {
		return err
	}
	// Recorded, so the record names the model that answers from here on.
	return a.loop.SwitchModel("", next)
}

// Flush waits until OnEvent has returned for every event recorded before the
// call, or ctx ends, and says which; once the agent is closed it returns an
// error, since nothing more is delivered. Call it
// before exiting on a stopped run, so the run's end is delivered. Give it a
// deadline, and never call it from OnEvent: the events it waits for are
// delivered by the goroutine that called OnEvent, so it could only wait out ctx.
func (a *Agent) Flush(ctx context.Context) error { return a.fwd.flush(ctx) }

// errClosed is Flush's answer once the agent is closed and delivery has stopped.
var errClosed = errors.New("abhed: the agent is closed; no more events are delivered")

// Close releases the extensions and MCP servers, the logins and hosts the
// agent's session made, and stops delivering events.
func (a *Agent) Close() {
	a.fwd.close()
	a.set.Close()
	a.loop.Session.CloseScoped()
}

// Providers lists the model provider types this build supports.
func Providers() []string { return model.Providers() }

type approverFn func(context.Context, string, json.RawMessage, policy.Result) (bool, error)

func (f approverFn) Approve(ctx context.Context, tool string, args json.RawMessage, d policy.Result) (bool, error) {
	return f(ctx, tool, args, d)
}

func approverFor(f func(context.Context, string, json.RawMessage, Decision) (bool, error), red *secrets.Redactor) agent.Approver {
	if f == nil {
		// No approver means nobody to ask, so anything needing approval is
		// refused. Defaulting to yes would make an embedded agent quietly more
		// permissive than the same policy on the command line.
		return agent.AutoApprove{Yes: false}
	}
	// The approver is shown the call as the record holds it, stored values redacted.
	return approverFn(func(ctx context.Context, tool string, args json.RawMessage, d Decision) (bool, error) {
		d.Reason, d.Scope = redactText(red, d.Reason), redactText(red, d.Scope)
		return f(ctx, tool, redactJSON(red, args), d)
	})
}

// withheld stands in for a payload whose redaction left invalid JSON.
var withheld = json.RawMessage(`{"withheld":"` + agent.Withheld + `"}`)

// redactJSON replaces stored values in a JSON payload. It fails closed: a
// payload redaction broke is withheld, never returned as it was.
func redactJSON(red *secrets.Redactor, b json.RawMessage) json.RawMessage {
	out := red.Redact(b)
	if !json.Valid(out) && !bytes.Equal(out, b) {
		return withheld
	}
	return out
}

// redactText replaces stored values in text, withholding it if that fails.
func redactText(red *secrets.Redactor, s string) string {
	raw, _ := json.Marshal(s)
	var out string
	if json.Unmarshal(red.Redact(raw), &out) != nil {
		return agent.Withheld
	}
	return out
}

func (a *Agent) lastMessage() string {
	msgs := a.loop.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleAssistant && msgs[i].Content != "" {
			return redactText(a.redact, msgs[i].Content)
		}
	}
	return ""
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// untrustedModelKeys are the model settings an untrusted file could not make.
func untrustedModelKeys(st config.WorkspaceTrust) []string {
	var out []string
	for _, k := range st.Ignored {
		if k.Key == "model" || strings.HasPrefix(k.Key, "model.") || strings.HasPrefix(k.Key, "custom_providers") {
			out = append(out, k.Key)
		}
	}
	return out
}
