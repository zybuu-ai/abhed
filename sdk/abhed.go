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
// One guarantee does NOT come with it: this package builds no sandbox unless
// the managed configuration sets one. Otherwise bash runs with the privileges
// of the process that embedded it, where the CLI would have wrapped it in the
// configured tier. A host that needs isolation owns it — a container, a jail,
// a separate user — exactly as for any other library that shells out.
package abhed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/tools"
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
	// it names. Without it only the managed file, if any, is read.
	ConfigDir string

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

	// OnEvent receives every event as it happens. It must not block for long:
	// the agent waits on it.
	OnEvent func(Event)

	// MaxTurns bounds one conversation. Zero uses the default, or the managed
	// limits.max_turns, which it may not exceed.
	MaxTurns int

	// SystemPrompt replaces the built-in prompt entirely. Most callers want
	// AppendSystem instead.
	SystemPrompt string
	// AppendSystem adds host-specific rules to the built-in prompt.
	AppendSystem string

	// Extensions are subprocesses that may veto a tool call.
	Extensions []ExtensionConfig
}

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
	host     *extension.Host
	id       string
}

// New builds an agent.
func New(ctx context.Context, opts Options) (*Agent, error) {
	if opts.Workspace == "" {
		return nil, fmt.Errorf("abhed: Workspace is required")
	}

	// The managed configuration applies with or without a config file.
	load := config.LoadManaged
	if opts.ConfigDir != "" {
		load = func() (config.Config, error) { return config.Load(opts.ConfigDir) }
	}
	cfg, err := load()
	if err != nil {
		return nil, fmt.Errorf("abhed: %w", err)
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
	if err := pol.AddDeny(cfg.Permissions.Deny...); err != nil {
		return nil, fmt.Errorf("abhed: deny rule: %w", err)
	}
	if err := pol.AddAsk(cfg.Permissions.Ask...); err != nil {
		return nil, fmt.Errorf("abhed: ask rule: %w", err)
	}
	if err := pol.AddAllow(cfg.Permissions.Allow...); err != nil {
		return nil, fmt.Errorf("abhed: allow rule: %w", err)
	}

	// A managed sandbox setting binds here too; otherwise bash is unsandboxed.
	bash := tools.Bash{}
	if cfg.ManagedSets("sandbox") {
		sb, err := sandboxconfig.Build(cfg, opts.Workspace)
		if err != nil {
			return nil, fmt.Errorf("abhed: %w", err)
		}
		bash.Sandbox = sb.Command
	}

	host := extension.NewHost(nil)
	specs := append(cfg.ExtensionSpecs(), opts.Extensions...)
	if len(specs) > 0 {
		host.Load(ctx, specs)
		if host.Len() > 0 {
			pol.Hooks = append(pol.Hooks, host.PolicyHook(ctx, "embedded"))
		}
	}

	store := agent.NewMemStore()
	id := fmt.Sprintf("embedded-%d", time.Now().UnixNano())
	rec := agent.NewRecorder(store, id, "")

	system := opts.SystemPrompt
	if system == "" {
		system = agent.BuildSystemPrompt(agent.BuildOptions{
			Profile: "main", Workspace: opts.Workspace,
			Model: provider.Model, ContextWindow: provider.ContextWindow,
		})
	}
	if opts.AppendSystem != "" {
		system += "\n\n" + opts.AppendSystem
	}

	loopCfg := agent.DefaultConfig()
	loopCfg.SystemPrompt = system
	if (opts.MaxTurns > 0 || cfg.ManagedSets("limits.max_turns")) && cfg.Limits.MaxTurns > 0 {
		loopCfg.MaxTurns = cfg.Limits.MaxTurns
	}

	registry := tools.NewRegistry(
		tools.Read{}, tools.Write{}, tools.Edit{},
		tools.Glob{}, tools.Grep{}, bash, tools.Todo{},
	)

	loop := agent.NewLoop(adapter, registry, pol, approverFor(opts.Approve),
		sess, rec, loopCfg)
	loop.Compactor = agent.NewCompactor(adapter, loopCfg.CompactAt)

	a := &Agent{loop: loop, store: store, host: host, id: id, registry: registry}
	if opts.OnEvent != nil {
		go func() {
			for ev := range store.Subscribe(id) {
				opts.OnEvent(ev)
			}
		}()
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

// RunStructured is RunJSON without the decode: the validated JSON as sent.
func (a *Agent) RunStructured(ctx context.Context, prompt string, schema json.RawMessage) (json.RawMessage, error) {
	raw, reason, err := agent.RunStructured(ctx, a.loop, a.registry, prompt, schema)
	if err != nil {
		var nr agent.ErrNoResult
		if errors.As(err, &nr) {
			return nil, ErrNoResult{Reason: string(nr.Reason), LastMessage: nr.Last}
		}
		return nil, fmt.Errorf("abhed: %w", err)
	}
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

// Events returns everything recorded so far.
func (a *Agent) Events() []Event {
	evs, _ := a.store.Events(a.id)
	return evs
}

// Usage reports what the conversation has cost.
func (a *Agent) Usage() Usage { return a.loop.Usage() }

// Fork rebuilds the conversation up to a sequence number and continues from
// there, discarding what came after.
func (a *Agent) Fork(throughSeq int64) error {
	msgs, err := agent.Fork(a.Events(), throughSeq)
	if err != nil {
		return err
	}
	a.loop.Restore(msgs)
	return nil
}

// ExportHTML renders the session as a self-contained page.
func (a *Agent) ExportHTML() string { return agent.ExportHTML(a.id, a.Events()) }

// SetModel swaps the provider mid-conversation, keeping the history.
func (a *Agent) SetModel(p Provider) error {
	cfg := config.ProviderConfig{
		Type: p.Type, BaseURL: p.BaseURL, Model: p.Model, APIKey: p.APIKey,
		ContextWindow: p.ContextWindow,
	}
	next, err := cfg.Adapter()
	if err != nil {
		return err
	}
	a.loop.SetAdapter(next)
	return nil
}

// Close releases the extensions.
func (a *Agent) Close() { a.host.Close() }

// Providers lists the model provider types this build supports.
func Providers() []string { return model.Providers() }

type approverFn func(context.Context, string, json.RawMessage, policy.Result) (bool, error)

func (f approverFn) Approve(ctx context.Context, tool string, args json.RawMessage, d policy.Result) (bool, error) {
	return f(ctx, tool, args, d)
}

func approverFor(f func(context.Context, string, json.RawMessage, Decision) (bool, error)) agent.Approver {
	if f == nil {
		// No approver means nobody to ask, so anything needing approval is
		// refused. Defaulting to yes would make an embedded agent quietly more
		// permissive than the same policy on the command line.
		return agent.AutoApprove{Yes: false}
	}
	return approverFn(f)
}

func (a *Agent) lastMessage() string {
	msgs := a.loop.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleAssistant && msgs[i].Content != "" {
			return msgs[i].Content
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
