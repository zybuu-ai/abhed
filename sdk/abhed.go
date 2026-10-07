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
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/embedded"
	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/webfetch"
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
	// overrides it. Allow is refused if the managed configuration sets any
	// permissions setting. A bash allow rule whose pattern holds ; & | ( ) < >
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
	// limits.max_turns, which it may not exceed. Set, it wins over the
	// ConfigDir and user files, below the managed ceiling.
	MaxTurns int

	// ConfiguredLimits takes limits.max_turns from the configuration, as the
	// CLI does, when MaxTurns is zero. Off, only a managed value binds.
	ConfiguredLimits bool

	// SystemPrompt replaces the built-in prompt entirely. Most callers want
	// AppendSystem instead.
	SystemPrompt string
	// AppendSystem adds host-specific rules to the built-in prompt.
	AppendSystem string

	// Extensions are subprocesses that may veto a tool call. With
	// ConfiguredTools, the tools they provide are offered too.
	Extensions []ExtensionConfig

	// Warn receives what the tool set skipped or found unsafe as it was
	// built: an MCP server or extension that did not start, a cluster or
	// host that skips verification; and, at Close, a fence that found state
	// planted in the workspace or could not list it. Nil discards it.
	Warn func(format string, args ...any)

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

	// Background is what background tasks do, with ConfiguredTools: "off"
	// (the default) joins them, so Run returns when the work is done, as it
	// always has; "notify" lets them outlive a Run, their results recorded
	// and delivered to OnEvent as they arrive, for the next Run or an
	// explicit Wake; "auto" does too, and a result that arrives while no run
	// is in progress starts a wake run on its own, its events to OnEvent.
	// The configuration may only tighten it.
	Background string

	// HostWake, with Background "auto", hosts each wake run: it is called
	// with the finished tasks' ids and the run to start, and reports whether
	// it started it. Nil runs it on the agent's own goroutine.
	HostWake func(taskIDs []string, run func(ctx context.Context) (string, error)) bool

	// Suggest offers a next prompt after each completed Run, as a
	// suggestion.offered event after Run returns, when the configuration's
	// suggest.enabled allows it. Off by default: one more model call per Run.
	Suggest bool

	// Sandbox runs bash in the tier the configuration's sandbox section asks
	// for (process by default), as the CLI does. New returns an error when
	// that tier is not available here, rather than running bash without it.
	Sandbox bool

	// Store keeps the agent's record. Nil keeps it in memory, where it ends
	// with the process; OpenLocalRecord gives the durable, chained local
	// record the command line uses. See store.go.
	Store Store
}

// TaskInfo describes one background task.
type TaskInfo = agent.TaskInfo

// ErrNothingToWake is Wake's answer when no background result waits.
var ErrNothingToWake = agent.ErrNothingToWake

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
	store    agent.Store
	set      *toolset.Set
	id       string
	fwd      *forwarder
	redact   *secrets.Fresh
	trust    config.WorkspaceTrust
	// cfg is the configuration New loaded, the only source of models to
	// switch to by name; current names the one the loop runs on, under forkMu.
	cfg     config.Config
	current string
	// sandbox is the backend New built for commands, closed with the agent;
	// warn is Options.Warn, which hears what closing it found.
	sandbox sandbox.Sandbox
	warn    func(format string, args ...any)
	// running counts the runs in progress, under forkMu: Fork holds it while
	// it forks and refuses while a run is in progress, and a run starting
	// meanwhile waits for the fork to finish.
	forkMu  sync.Mutex
	running int
	// wakes are the wake runs this agent started on its own, by cancel.
	wakes wakeRuns
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

	// The record Options.Store names is state, as a managed record.dir is.
	if cfg, err = withRecordState(cfg, opts); err != nil {
		return nil, err
	}
	provider, err := cfg.Provider()
	if err != nil {
		return nil, fmt.Errorf("abhed: %w", err)
	}
	// The CLI's redactor; a store that exists but cannot be loaded refuses the session.
	first, err := secrets.Default().LoadRedactor()
	if err != nil {
		return nil, fmt.Errorf("abhed: %w", err)
	}
	// Read again as the store changes: bash reads it by name at each call,
	// so a secret added during the session is redacted from then on.
	red := secrets.Default().Fresh(first)
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
	// Set whatever the tool set holds: web_fetch with no host list asks.
	pol.AskReadOnly = webfetch.AskReadOnly(cfg.WebFetch.Enabled, cfg.WebFetch.AllowedHosts)
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
	if err := pol.AllowGitExtensions(cfg.Permissions.GitExtensions...); err != nil {
		return nil, fmt.Errorf("abhed: permissions.git_extensions: %w", err)
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
	cfg.Sandbox.WriteProtected = append(cfg.Sandbox.WriteProtected, embedded.From(ctx).Protect...)
	cfg.Sandbox.ProtectGit = cfg.Sandbox.ProtectGit || embedded.From(ctx).ProtectGit
	// A chosen tier confines commands too: none runs unconfined in its place.
	var fence *sandbox.Fence
	var sbox sandbox.Sandbox
	if opts.Sandbox || cfg.ManagedSets("sandbox") || cfg.Sandbox.Tier != "" {
		sb, err := sandboxconfig.Build(cfg, opts.Workspace, stateRoots...)
		if err != nil {
			return nil, fmt.Errorf("abhed: %w", err)
		}
		sbox = sb
		bash.Sandbox = sb.Command
		bash.Isolation = sandboxconfig.Isolation(cfg, string(sb.Tier()))
		fence, _ = sb.(*sandbox.Fence)
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
		Vault: secrets.Default(), Warn: opts.Warn, Sandbox: sbox,
	})
	// What New made is released on every error from here: the tools, and
	// the sandbox's cgroup and private temp.
	fail := func(err error) (*Agent, error) {
		set.Close()
		_ = sandbox.Close(sbox)
		return nil, err
	}
	// A skill's own directory is reachable, as it is from the command line.
	for _, dir := range set.SkillDirs() {
		if err := sess.AddRoot(dir); err != nil {
			return fail(fmt.Errorf("abhed: skill directory: %w", err))
		}
	}
	toolset.Police(set.Extensions, pol, "embedded")

	x := embedded.From(ctx)
	id := x.ID
	if id == "" {
		id = fmt.Sprintf("embedded-%d", time.Now().UnixNano())
	}
	store, err := recordFor(ctx, opts, id, cfg, x)
	if err != nil {
		return fail(err)
	}
	// Every write goes through the forwarder, so OnEvent misses none, from the
	// first event on.
	fwd := newForwarder(store, opts.OnEvent != nil)
	rec := agent.NewRecorder(fwd, id, "")
	rec.Redact = red

	loopCfg := toolset.LoopConfig(cfg, "")
	// The file's max_turns binds an embedded agent when the organisation sets
	// it, or when the caller asks for the configured limits.
	loopCfg.MaxTurns = agent.DefaultConfig().MaxTurns
	if (opts.MaxTurns > 0 || opts.ConfiguredLimits || cfg.ManagedSets("limits.max_turns")) && cfg.Limits.MaxTurns > 0 {
		loopCfg.MaxTurns = cfg.Limits.MaxTurns
	}

	approver := approverFor(opts.Approve, red)
	registry := set.Registry
	budget := toolset.Budget(cfg)
	// Off unless the caller asks, so Run keeps returning when the work is done.
	mode, err := agent.ParseWakeMode(opts.Background)
	if opts.Background == "" {
		mode, err = agent.WakeOff, nil
	}
	if err != nil {
		return fail(fmt.Errorf("abhed: Background is off, notify or auto, not %q", opts.Background))
	}
	bgCfg := cfg
	bgCfg.Subagents.Wake = string(mode.Tighter(wakeOf(cfg)))
	if opts.ConfiguredTools {
		// The child's events stay in the store, reached through the parent's
		// subagent.* events; OnEvent carries this agent's own record, as the
		// command line's JSON output does.
		f := &agent.SubagentFactory{Adapter: adapter, Policy: pol, Session: sess, Store: store,
			Budget: budget, Config: loopCfg, Workspace: opts.Workspace, Redact: red, Definitions: set.Agents, Background: true,
			Models: toolset.ModelResolver(cfg), ModelNames: toolset.OfferedModels(cfg)}
		registry = toolset.Subagents(registry, f, cfg.Limits.MaxParallelSubagents)
	}

	// Set once the tools are known, so the prompt names only those there.
	system := opts.SystemPrompt
	switch {
	case system != "":
	case opts.ConfiguredTools:
		system = toolset.SystemPrompt(opts.Workspace, adapter, set.SkillListing, registry.Names())
	default:
		// No ABHED.md: an embedder running on repositories it does not own
		// takes the workspace's instructions only by opting in.
		system = agent.BuildSystemPrompt(agent.BuildOptions{
			Profile: "main", Workspace: opts.Workspace,
			Model: adapter.Profile().Name, ContextWindow: adapter.Profile().ContextWindow,
			Tools: registry.Names(),
		})
	}
	if opts.AppendSystem != "" {
		system += "\n\n" + opts.AppendSystem
	}
	loopCfg.SystemPrompt = system

	loop := agent.NewLoop(adapter, registry, pol, approver, sess, rec, loopCfg)
	loop.Provider = cfg.Model.Default
	agent.NewBackground(loop, toolset.BackgroundPolicy(bgCfg, agent.WakeAuto))
	loop.Compactor = agent.NewCompactor(adapter, loopCfg.CompactAt)
	toolset.Summarize(loop.Compactor, set.Extensions, id)
	loop.Budget = budget
	if opts.Suggest {
		loop.Suggest = toolset.Suggester(cfg)
	}

	// The loop runs on its own copy of the registry, which RunJSON must add its tool to.
	// A surface's new session says how it started, as the command line's does.
	if x.Surface != "" && !x.Resume {
		start := map[string]any{"surface": x.Surface, "headless": opts.Approve == nil, "provider": cfg.Model.Default,
			"model": adapter.Profile().Name, "mode": string(pol.Mode), "web": toolset.WebState(cfg)}
		if _, err := rec.Record(agent.EvSessionStarted, agent.ActorSystem, agent.Trusted, start); err != nil {
			return fail(fmt.Errorf("abhed: recording the session start: %w", err))
		}
		attempts := toolset.ConfigAttempts(cfg, toolset.LocalPrincipal(sandbox.InAgentCommand()))
		if err := toolset.RecordConfigAttempts(rec, attempts); err != nil {
			return fail(fmt.Errorf("abhed: recording the configuration attempts: %w", err))
		}
	}
	// Every agent under the fence records what qualified it, a plain SDK
	// caller's included, and gives it the record before any command runs.
	if fence != nil {
		if _, err := rec.Record(agent.EvFenceQualified, agent.ActorSystem, agent.Trusted, fence.Qualification()); err != nil {
			return fail(fmt.Errorf("abhed: recording the fence's qualification: %w", err))
		}
		fence.SetRecord(agent.SandboxRecord(rec))
	}
	a := &Agent{loop: loop, store: store, set: set, id: id, registry: loop.Tools, fwd: fwd, redact: red, trust: cfg.Workspace,
		cfg: cfg, current: cfg.Model.Default, sandbox: sbox, warn: opts.Warn}
	a.hookWake(opts.HostWake)
	if opts.OnEvent != nil {
		go fwd.run(opts.OnEvent)
	}
	return a, nil
}

// Run sends a prompt and returns the agent's final message.
func (a *Agent) Run(ctx context.Context, prompt string) (string, error) {
	defer a.startRun()()
	reason, err := a.loop.Run(ctx, prompt)
	if err != nil {
		return "", err
	}
	if reason != agent.TermCompleted {
		return a.lastMessage(), &EndedError{Reason: reason}
	}
	return a.lastMessage(), nil
}

// EndedError is a run that ended for another reason than completing its
// work, such as the turn limit; Reason is the terminal reason recorded.
type EndedError struct{ Reason TerminalReason }

func (e *EndedError) Error() string { return fmt.Sprintf("abhed: ended as %s", e.Reason) }

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
	defer a.startRun()()
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
		return raw, &EndedError{Reason: reason}
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

// Queued counts steering messages not yet delivered: sent while no run was in
// progress, or as the last run ended. The next run delivers them first.
func (a *Agent) Queued() int { return len(a.loop.Queued()) }

// RunQueued continues the conversation with only the queued steering
// messages, for one that arrived as the last run ended. With none it does
// nothing and returns the last message.
func (a *Agent) RunQueued(ctx context.Context) (string, error) {
	defer a.startRun()()
	reason, err := a.loop.RunQueued(ctx)
	if err != nil {
		return "", err
	}
	if reason != agent.TermCompleted {
		return a.lastMessage(), fmt.Errorf("abhed: ended as %s", reason)
	}
	return a.lastMessage(), nil
}

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
// It returns ErrForkDuringRun while Run, Continue, RunJSON or RunStructured is
// in progress: a fork rewrites the conversation and ends its logins, which
// must not happen under a turn. A run started while a fork is under way
// waits for it.
func (a *Agent) Fork(throughSeq int64) error {
	a.forkMu.Lock()
	defer a.forkMu.Unlock()
	if a.running > 0 {
		return ErrForkDuringRun
	}
	_, err := a.loop.ForkTo(a.Events(), throughSeq)
	return err
}

// startRun counts a run in progress, after any fork under way, and returns
// what ends it.
func (a *Agent) startRun() func() {
	a.forkMu.Lock()
	a.running++
	a.forkMu.Unlock()
	return func() {
		a.forkMu.Lock()
		a.running--
		a.forkMu.Unlock()
	}
}

// ErrForkDuringRun is Fork's refusal while a run is in progress. Fork once
// the run has returned.
var ErrForkDuringRun = errors.New("abhed: cannot fork while a run is in progress; fork after it returns")

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
	// Under forkMu, as SwitchModelNamed is, so the two never interleave.
	a.forkMu.Lock()
	defer a.forkMu.Unlock()
	// Recorded, so the record names the model that answers from here on.
	if err := a.loop.SwitchModel("", next); err != nil {
		return err
	}
	a.current = "" // no configured model is current now
	return nil
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
//
// Background tasks still running are cancelled as session_closed first, and
// Close waits a bounded time for them to record their end.
func (a *Agent) Close() {
	a.endWakes()
	a.loop.Background.Close(agent.TermSessionClosed)
	a.fwd.close()
	a.set.Close()
	a.loop.Session.CloseScoped()
	// The fence closes before the record is released, so state it finds
	// planted then is recorded; its error goes to Warn.
	if a.sandbox != nil {
		if err := sandbox.Close(a.sandbox); err != nil && a.warn != nil {
			a.warn("abhed: %v", err)
		}
	}
	a.releaseRecord()
}

// Background lists this agent's background tasks. Approve may be called for
// one of them at any time until Close, including after Run has returned.
func (a *Agent) Background() []TaskInfo { return a.loop.Background.Tasks() }

// CancelTask stops one running background task, as a person's stop.
func (a *Agent) CancelTask(id string) error {
	if !a.loop.Background.Cancel(id, agent.TermUserInterrupt) {
		return fmt.Errorf("abhed: no running background task %q", id)
	}
	return nil
}

// CancelTasks stops every running background task, as a person's stop, and
// reports how many. A wake run in progress stops too, and no other starts
// until the next Run.
func (a *Agent) CancelTasks() int {
	n := a.loop.Background.CancelAll(agent.TermUserInterrupt)
	a.stopWake()
	return n
}

// WaitBackground waits until no background task is running, or ctx ends.
func (a *Agent) WaitBackground(ctx context.Context) error {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for a.loop.Background.Live() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

// Wake runs the agent on the background results waiting for it, with no new
// prompt, and returns its answer. It is recorded as session.woken by the
// caller. ErrNothingToWake when no result waits.
func (a *Agent) Wake(ctx context.Context) (string, error) {
	// A wake is a run: Fork and a model switch wait for it or refuse.
	defer a.startRun()()
	reason, err := a.loop.RunWoken(ctx, agent.Wake{By: "caller"})
	if err != nil {
		return "", err
	}
	if reason != agent.TermCompleted && reason != agent.TermWakeLimit {
		return a.lastMessage(), &EndedError{Reason: reason}
	}
	return a.lastMessage(), nil
}

// wakeOf is the configured wake mode: auto when unset, and off, the
// tightest, when the setting cannot be read.
func wakeOf(cfg config.Config) agent.WakeMode {
	m, err := agent.ParseWakeMode(cfg.Subagents.Wake)
	if err != nil {
		return agent.WakeOff
	}
	return m
}

// Providers lists the model provider types this build supports.
func Providers() []string { return model.Providers() }

type approverFn func(context.Context, string, json.RawMessage, policy.Result) (bool, error)

func (f approverFn) Approve(ctx context.Context, tool string, args json.RawMessage, d policy.Result) (bool, error) {
	return f(ctx, tool, args, d)
}

func approverFor(f func(context.Context, string, json.RawMessage, Decision) (bool, error), red *secrets.Fresh) agent.Approver {
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
func redactJSON(red *secrets.Fresh, b json.RawMessage) json.RawMessage {
	out := red.Redact(b)
	if !json.Valid(out) && !bytes.Equal(out, b) {
		return withheld
	}
	return out
}

// redactText replaces stored values in text, withholding it if that fails.
func redactText(red *secrets.Fresh, s string) string {
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
