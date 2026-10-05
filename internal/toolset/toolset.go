// Package toolset assembles the agent every surface runs: the tools, the
// system prompt, the loop settings, the shared budget and the subagent tools.
//
// The command line, the server, rpc, acp, eval and the SDK each built their
// own tool set, and the ones built later fell behind: a console agent told a
// person it could not run subagents because nobody had added them there. One
// builder means a surface differs from the CLI only where it says why.
package toolset

import (
	"context"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/index"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/skills"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/webfetch"
)

// Part is a piece of the tool set the configuration can add.
type Part uint

const (
	// MCP connects the configured MCP servers and offers their tools.
	MCP Part = 1 << iota
	// Vetoes starts the configured extensions so their policy hook can refuse
	// or ask about a call. It never permits one.
	Vetoes
	// ExtensionTools offers the tools extensions provide; it implies Vetoes.
	ExtensionTools
	// Skills offers the skill tool, running a skill's declared pipeline on
	// the calling loop, and lists the skills in the prompt.
	Skills
	// WebSearch offers web search when web_search.enabled.
	WebSearch
	// Retrieval offers the code index when retrieval.enabled.
	Retrieval
	// RAG offers a tool per enabled rag corpus.
	RAG
	// Infra offers the Kubernetes and SSH tools when enabled.
	Infra
	// WebFetch offers web_fetch when web_fetch.enabled. A policy engine the
	// set is used with needs webfetch.AskReadOnly, or the tool never asks.
	WebFetch
	// Agents loads subagent definitions beside the built-in roles: managed,
	// the workspace's when trusted, and the operator's directories.
	Agents

	// All is what the CLI runs with.
	All = MCP | Vetoes | ExtensionTools | Skills | WebSearch | Retrieval | RAG | Infra | WebFetch | Agents
)

// Options are what a surface decides; everything else comes from the configuration.
type Options struct {
	// Workspace is the directory the retrieval index covers.
	Workspace string
	// Bash is the shell tool. The surface builds it because the sandbox is the
	// surface's to choose and to refuse to start without; nothing here weakens it.
	Bash tools.Bash
	// Parts selects what the configuration may add beyond the built-in tools.
	Parts Part
	// Agents are subagent definitions given for this run (-agents), parsed
	// and checked as files are; the managed ones still hold their names.
	Agents []*agent.Definition
	// Extensions are started beside the configuration's own, as the SDK's
	// Options.Extensions are. The configuration's are subject to workspace
	// trust when it is loaded: an untrusted file adds none.
	Extensions []extension.Config
	// Vault is the secrets store bash and a login tool read a credential from
	// by name, and web_fetch refuses a URL holding a value from. Nil offers no
	// stored secrets, and leaves web_fetch unable to fetch.
	Vault *secrets.Store
	// Warn receives what failed and was skipped: an MCP server, an extension,
	// a skill, a corpus. Nil discards them.
	Warn func(format string, args ...any)
}

// Set is one assembled tool set. Registry holds no session-bound tool, so a
// server may share it across sessions; Subagents binds task and tasks to one.
type Set struct {
	Registry     *tools.Registry
	Skills       *skills.Registry
	SkillListing string
	Gateway      *mcp.Gateway
	Extensions   *extension.Host
	Index        *index.Index
	// Agents are the subagent types a session built from this set offers:
	// the built-in roles, plus the loaded definitions with the Agents part.
	Agents *agent.Definitions
	// extensionTools names the tools extensions provided.
	extensionTools []string
}

// ExtensionToolNames are the tools the set's extensions provided.
func (s *Set) ExtensionToolNames() []string { return s.extensionTools }

// Build assembles the tools for a workspace from cfg. Something configured
// that cannot start is reported through Warn and left out, as the CLI always
// has: one broken MCP server or corpus should not stop the agent.
func Build(ctx context.Context, cfg config.Config, o Options) *Set {
	warn := o.Warn
	if warn == nil {
		warn = func(string, ...any) {}
	}
	// Connections and processes outlive the call that built them; Close ends them.
	ctx = context.WithoutCancel(ctx)
	// Every surface's bash reads stored secrets from the same vault as the
	// login tools; each name still needs its own secret(NAME) allow rule.
	if o.Vault != nil && o.Bash.Secrets == nil {
		o.Bash.Secrets = o.Vault.Env
		o.Bash.SecretNames = VaultNames(o.Vault)
	}
	s := &Set{
		Registry: tools.NewRegistry(
			tools.Read{}, tools.Write{}, tools.Edit{},
			tools.Glob{}, tools.Grep{}, o.Bash,
			agent.ShellOutput{}, agent.ShellKill{},
			agent.TodoTool{},
		),
		Skills: skills.NewRegistry(),
		Agents: agent.BuiltinDefinitions(),
	}
	if o.Parts&(Vetoes|ExtensionTools) != 0 {
		s.Extensions = extension.NewHost(o.Warn)
		specs := append(cfg.ExtensionSpecs(), cfg.NarrowHooks(o.Extensions)...)
		for _, err := range s.Extensions.Load(ctx, specs) {
			warn("%v", err)
		}
	}
	// MCP servers extend the tool surface. Every remote tool is namespaced and
	// routes through the policy engine, since Abhed cannot know what it does.
	if o.Parts&MCP != 0 {
		s.Gateway = mcp.NewGateway()
		for _, err := range s.Gateway.Connect(ctx, MCPConfigs(cfg)) {
			warn("%v", err)
		}
		for _, t := range s.Gateway.Tools() {
			s.Registry.Add(t)
		}
		DeferMCP(s.Registry, DeferThreshold)
	}
	// A tool an extension provides goes through policy and the record like
	// any other: a capability added, never a way around the rules.
	if o.Parts&ExtensionTools != 0 && s.Extensions.Len() > 0 {
		ts, errs := s.Extensions.Tools(ctx)
		for _, err := range errs {
			warn("%v", err)
		}
		for _, t := range ts {
			s.Registry.Add(t)
			s.extensionTools = append(s.extensionTools, t.Name())
		}
	}
	if o.Parts&RAG != 0 {
		for _, t := range RAGTools(cfg, warn) {
			s.Registry.Add(t)
		}
	}
	if o.Parts&Infra != 0 {
		for _, t := range InfraTools(cfg, o.Vault, warn) {
			s.Registry.Add(t)
		}
	}
	if o.Parts&Skills != 0 {
		s.Skills, s.SkillListing = LoadSkills(cfg, warn)
		if s.Skills.Len() > 0 {
			s.Registry.Add(SkillTool(s.Skills))
		}
	}
	if o.Parts&Agents != 0 {
		s.Agents = LoadAgents(cfg, cfg.Workspace, o.Agents, warn)
	}
	var fetch *webfetch.Tool
	if o.Parts&WebFetch != 0 {
		fetch = WebFetchTool(cfg, o.Vault)
	}
	if o.Parts&WebSearch != 0 {
		if t, err := WebSearchTool(cfg, o.Vault); err != nil {
			warn("web search disabled: %v", err)
		} else if t != nil {
			// Results point at web_fetch only where it is offered.
			t.Fetch = fetch != nil
			s.Registry.Add(t)
		}
	}
	if fetch != nil {
		s.Registry.Add(fetch)
	}
	// Retrieval is an accelerator over grep, not a replacement.
	if o.Parts&Retrieval != 0 && cfg.Retrieval.Enabled {
		if ix, err := OpenIndex(ctx, cfg, o.Workspace); err != nil {
			warn("index unavailable, falling back to grep: %v", err)
		} else {
			s.Index = ix
			s.Registry.Add(&index.SearchTool{Index: ix})
		}
	}
	return s
}

// Close ends the MCP connections and the extension processes.
func (s *Set) Close() {
	if s == nil {
		return
	}
	if s.Gateway != nil {
		s.Gateway.Close()
	}
	if s.Extensions != nil {
		s.Extensions.Close()
	}
}

// Extension states, as ExtensionStatus reports them.
const (
	ExtensionRunning    = "running"
	ExtensionStopped    = "stopped"     // started, then crashed, hung or was closed
	ExtensionNotStarted = "not started" // configured, but it failed to start
)

// ExtensionStatus says of each configured extension whether its veto is in
// force. One that is not running leaves every session without it, which an
// operator of a shared server needs to see rather than find in a log.
func ExtensionStatus(cfg config.Config, h *extension.Host) map[string]string {
	running := map[string]bool{}
	if h != nil {
		running = h.Running()
	}
	out := make(map[string]string, len(cfg.Extensions))
	for _, e := range cfg.Extensions {
		up, started := running[e.Name]
		switch {
		case !started:
			out[e.Name] = ExtensionNotStarted
		case up:
			out[e.Name] = ExtensionRunning
		default:
			out[e.Name] = ExtensionStopped
		}
	}
	return out
}

// SkillDirs are the loaded skills' own directories, which a session is
// granted so a skill can reference the scripts shipped beside it.
func (s *Set) SkillDirs() []string {
	if s == nil || s.Skills == nil {
		return nil
	}
	var out []string
	for _, sk := range s.Skills.All() {
		if sk.Dir != "" {
			out = append(out, sk.Dir)
		}
	}
	return out
}

// Police puts the extensions' veto on a session's policy. It runs first so it
// can refuse, and is structurally unable to return Allow, so deny stays absolute.
func Police(h *extension.Host, pol *policy.Engine, sessionID string) {
	if h == nil || h.Len() == 0 || pol == nil {
		return
	}
	pol.EngineHooks = append(pol.EngineHooks, func(e *policy.Engine) policy.Hook {
		return h.PolicyHookFor(context.Background(), sessionID, e)
	})
}

// Summarize lets an extension supply or refuse a compaction summary: the
// default summarizer cannot know what this deployment must keep.
func Summarize(c *agent.Compactor, h *extension.Host, sessionID string) {
	if c == nil || h == nil || h.Len() == 0 {
		return
	}
	c.Summarizer = func(msgs []model.Message) (string, bool) {
		out := make([]extension.Message, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, extension.Message{Role: string(m.Role), Content: m.Content})
		}
		return h.OnBeforeCompact(context.Background(), sessionID, out)
	}
}

// SystemPrompt is the main agent's prompt, named as the adapter names itself
// so a model switch can rewrite the line, with the workspace's ABHED.md files
// and the skill listing. toolNames are the session's own tools, so the
// prompt names only the web tools it has.
func SystemPrompt(workspace string, adapter model.Adapter, skillListing string, toolNames []string) string {
	return agent.BuildSystemPrompt(agent.BuildOptions{
		Profile:       "main",
		Workspace:     workspace,
		Model:         adapter.Profile().Name,
		ContextWindow: adapter.Profile().ContextWindow,
		MemoryFiles:   agent.DiscoverMemoryFiles(workspace),
		Skills:        skillListing,
		Tools:         toolNames,
	})
}

// LoopConfig is the loop's settings from the configuration's limits and
// context sections; a value left at zero keeps the loop's default.
func LoopConfig(cfg config.Config, system string) agent.Config {
	c := agent.DefaultConfig()
	c.SystemPrompt = system
	if n := cfg.Limits.MaxTurns; n > 0 {
		c.MaxTurns = n
	}
	if n := cfg.Limits.MaxTokens; n > 0 {
		c.MaxTokens = n
	}
	if at := cfg.Context.CompactAt; at > 0 {
		c.CompactAt = at
	}
	c.OffloadAt = cfg.Context.OffloadFraction()
	return c
}

// Budget is the one allowance a session's loop and its subagents share, so a
// fan-out cannot multiply spend unseen.
func Budget(cfg config.Config) *agent.Budget {
	return agent.NewBudget(int64(cfg.Limits.MaxBudgetTokens), cfg.Limits.MaxSubagents, cfg.Limits.NestedSubagents)
}

// Subagents returns a copy of reg with task and tasks bound to one session's
// factory, which spawns into f.Workspace and takes f.Tools to be that copy.
// The copy keeps the shared registry free of one session's subagents.
//
// A child's calls are judged by f.Policy (with its own root added) and put to
// the approver of the loop that spawned it; f.Approver answers only a spawn
// with no loop and, left nil, refuses. f.Budget must be the loop's Budget.
//
// The tools offer f.Definitions, the session's agent types: a set's Agents,
// or the built-in roles when nil. They are fixed for the session.
func Subagents(reg *tools.Registry, f *agent.SubagentFactory, maxParallel int) *tools.Registry {
	out := reg.Clone()
	f.Tools = out
	if f.Definitions == nil {
		f.Definitions = agent.BuiltinDefinitions()
	}
	var models []string
	if f.Models != nil {
		models = f.ModelNames
	}
	var bg func(context.Context, agent.SubagentRequest) (string, error)
	if f.Background {
		bg = f.SpawnBackground
		out.Add(agent.TaskStatus{})
		out.Add(agent.TaskCancel{})
	}
	out.Add(agent.Task{Spawn: f.Spawn, Agents: f.Definitions, Workspace: f.Workspace, Models: models, Background: bg})
	out.Add(agent.Tasks{Spawn: f.Spawn, Agents: f.Definitions,
		Workspace: f.Workspace, MaxParallel: maxParallel, Models: models, Background: bg})
	return out
}

// BackgroundPolicy is a session's background limits from the configuration,
// with the wake mode no wider than ceiling, the most the surface can host:
// off where nobody can come back to the conversation, notify where nothing
// can start a run on its own.
func BackgroundPolicy(cfg config.Config, ceiling agent.WakeMode) agent.BackgroundPolicy {
	mode, err := agent.ParseWakeMode(cfg.Subagents.Wake)
	if err != nil {
		mode = agent.WakeOff // Validate refuses it; fail to the tightest here too
	}
	minutes := cfg.Limits.BackgroundMaxMinutes
	if minutes <= 0 {
		minutes = 60
	}
	// The session's own switch may go no higher than what the configuration
	// and the surface both allow.
	mode = mode.Tighter(ceiling)
	return agent.BackgroundPolicy{
		Wake:            mode,
		Ceiling:         mode,
		MaxLive:         cfg.Limits.MaxBackgroundSubagents,
		Lifetime:        time.Duration(min(minutes, 480)) * time.Minute,
		MaxWakesPerHour: cfg.Subagents.MaxWakesPerHour,
		WakeMaxTurns:    cfg.Subagents.WakeMaxTurns,
		MaxShells:       cfg.Limits.BackgroundShells,
	}
}

// SkillTool offers reg's skills. A skill declaring a pipeline is run rather
// than described, its tool steps put through the loop that called it.
func SkillTool(reg *skills.Registry) skills.Tool {
	return skills.Tool{R: reg, RunPipeline: PipelineRunner(nil)}
}

// VaultNames lists what the model may ask for. An unreadable store lists
// nothing: the failure surfaces when a secret is used, with its reason.
func VaultNames(v *secrets.Store) []string {
	names, err := v.Names()
	if err != nil {
		return nil
	}
	return names
}
